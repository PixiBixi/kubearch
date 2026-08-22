package watcher

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"runtime/pprof"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/PixiBixi/kubearch/internal/inspector"
	"github.com/PixiBixi/kubearch/internal/store"
	"github.com/PixiBixi/kubearch/internal/types"
)

const (
	// defaultWorkers bounds goroutines and concurrent registry fetches alike:
	// the queue holds the backlog, so a cluster-wide initial sync no longer
	// spawns one goroutine per image.
	defaultWorkers = 10
	// inspectTimeout bounds a single inspection. Without it, a registry that
	// accepts the connection but never answers would hold a worker forever.
	inspectTimeout = 30 * time.Second
	// maxAttempts before an image is dropped from the queue. With the backoff
	// below, that spans a bit over two minutes of retries.
	maxAttempts    = 5
	baseRetryDelay = 1 * time.Second
	maxRetryDelay  = 1 * time.Minute
	// shortDigestLen is "sha256:" (7 chars) + 12 hex chars — enough to identify a digest in logs.
	shortDigestLen = 19
	// workerLabel tags the inspection workers in profiles and tracebacks.
	workerLabel = "inspect-worker"
)

// Inspector inspects a container image and returns its digest and supported platforms.
// Implementations must be safe for concurrent use.
type Inspector interface {
	Inspect(ctx context.Context, imageRef string, auth inspector.PodAuth) (string, []types.Platform, error)
}

// Metrics receives inspection outcomes for self-monitoring. It is an
// interface, not a *collector.SelfMetrics field, so this package does not
// have to import collector (which already imports store, same as this
// package — a direct dependency back from here would cycle).
type Metrics interface {
	// ObserveInspection records the outcome and duration of one inspection attempt.
	ObserveInspection(result string, d time.Duration)
	// ObserveRetry records that a failed inspection is being requeued.
	ObserveRetry()
}

// noopMetrics is used when New is called without a Metrics implementation,
// so tests and callers that don't care about self-monitoring aren't forced to
// provide one.
type noopMetrics struct{}

func (noopMetrics) ObserveInspection(string, time.Duration) {}
func (noopMetrics) ObserveRetry()                           {}

// Watcher watches Kubernetes pod events and triggers image inspections for new images.
type Watcher struct {
	client    kubernetes.Interface
	namespace string
	store     *store.Store
	inspector Inspector
	logger    *slog.Logger
	workers   int
	metrics   Metrics

	// queue dedupes images, rate-limits retries and bounds the backlog.
	queue workqueue.TypedRateLimitingInterface[string]

	// authByImage keeps the pod credentials an image was discovered with, so a
	// retry does not depend on the originating pod still being around. Entries
	// live only while the image is queued.
	authMu      sync.Mutex
	authByImage map[string]inspector.PodAuth
}

// New builds a Watcher. metrics may be nil, in which case inspection
// outcomes are silently dropped instead of forcing every caller (tests
// included) to supply a Metrics implementation.
func New(client kubernetes.Interface, namespace string, s *store.Store, insp Inspector, logger *slog.Logger, metrics Metrics) *Watcher {
	if metrics == nil {
		metrics = noopMetrics{}
	}
	return &Watcher{
		client:    client,
		namespace: namespace,
		store:     s,
		inspector: insp,
		logger:    logger,
		workers:   defaultWorkers,
		metrics:   metrics,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](baseRetryDelay, maxRetryDelay),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "images"},
		),
		authByImage: make(map[string]inspector.PodAuth),
	}
}

// QueueDepth reports the number of images currently waiting in the work
// queue, for self-monitoring.
func (w *Watcher) QueueDepth() int {
	return w.queue.Len()
}

// Run starts the pod informer and blocks until ctx is cancelled.
// It returns an error if the informer cannot be initialised (handler registration
// or cache sync failure). A nil return means the context was cancelled normally.
func (w *Watcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		w.client, 0, // no periodic resync — we rely on watch events
		informers.WithNamespace(w.namespace),
	)

	podInformer := factory.Core().V1().Pods().Informer()
	if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if pod, ok := obj.(*corev1.Pod); ok {
				w.onPod(pod)
			}
		},
		// A pod spec is not fully immutable: `kubectl set image` rewrites
		// spec.containers[*].image, and ephemeral containers are added to a
		// running pod. Status updates are far more frequent than either, so
		// bail out unless the image set actually changed.
		UpdateFunc: func(oldObj, newObj any) {
			oldPod, ok := oldObj.(*corev1.Pod)
			if !ok {
				return
			}
			newPod, ok := newObj.(*corev1.Pod)
			if !ok || sameImages(oldPod, newPod) {
				return
			}
			w.onPod(newPod)
		},
		DeleteFunc: func(obj any) {
			if pod, ok := toPod(obj); ok {
				w.onDelete(pod)
			}
		},
	}); err != nil {
		w.logger.Error("failed to register pod event handler", "err", err)
		return fmt.Errorf("register pod event handler: %w", err)
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced) {
		w.logger.Error("timed out waiting for cache sync")
		return errors.New("timed out waiting for pod cache sync")
	}

	ns := w.namespace
	if ns == "" {
		ns = "all"
	}
	w.logger.Info("cache synced, watching pods", "namespace", ns, "workers", w.workers)

	stopWorkers := w.startWorkers(ctx)
	<-ctx.Done()
	stopWorkers()
	return nil
}

// startWorkers launches the inspection workers and returns a function that
// drains the queue and waits for them to exit.
func (w *Watcher) startWorkers(ctx context.Context) (stop func()) {
	var wg sync.WaitGroup
	for range w.workers {
		// Go 1.25: WaitGroup.Go pairs Add/Done with the goroutine itself.
		wg.Go(func() {
			// Go 1.27 prints goroutine labels in tracebacks, not just in
			// profiles: a panic or a SIGQUIT dump then names the worker pool
			// instead of showing ten anonymous closures.
			pprof.Do(ctx, pprof.Labels("component", workerLabel), func(ctx context.Context) {
				for w.processNext(ctx) {
				}
			})
		})
	}
	return func() {
		w.queue.ShutDown()
		wg.Wait()
	}
}

// processNext handles one queued image. It returns false once the queue is shut down.
func (w *Watcher) processNext(ctx context.Context) bool {
	imageRef, shutdown := w.queue.Get()
	if shutdown {
		return false
	}
	defer w.queue.Done(imageRef)

	err := w.inspect(ctx, imageRef)
	switch {
	case err == nil:
		w.queue.Forget(imageRef)

	case ctx.Err() != nil:
		// Shutting down: drop the item without burning a retry.
		w.queue.Forget(imageRef)
		w.store.FailImage(imageRef)
		w.forgetAuth(imageRef)

	case w.queue.NumRequeues(imageRef)+1 >= maxAttempts:
		w.logger.Error("inspection failed, giving up",
			"image", imageRef, "attempts", maxAttempts, "err", err)
		w.queue.Forget(imageRef)
		// Clearing pending lets a later pod event re-trigger the image.
		w.store.FailImage(imageRef)
		w.forgetAuth(imageRef)

	default:
		w.logger.Warn("inspection failed, retrying",
			"image", imageRef, "attempt", w.queue.NumRequeues(imageRef)+1, "err", err)
		w.metrics.ObserveRetry()
		w.queue.AddRateLimited(imageRef)
	}
	return true
}

// inspect performs one inspection attempt and stores the result on success.
func (w *Watcher) inspect(ctx context.Context, imageRef string) error {
	auth, ok := w.authFor(imageRef)
	if !ok {
		return nil // no longer tracked: the image was resolved or dropped meanwhile
	}

	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()

	start := time.Now()
	digest, platforms, err := w.inspector.Inspect(ctx, imageRef, auth)
	if err != nil {
		w.metrics.ObserveInspection("failure", time.Since(start))
		return fmt.Errorf("inspect %q: %w", imageRef, err)
	}
	w.metrics.ObserveInspection("success", time.Since(start))

	w.store.SetImage(imageRef, digest, platforms)
	w.forgetAuth(imageRef)
	w.logger.Info("inspection done",
		"image", imageRef,
		"digest", shortDigest(digest),
		"platforms", len(platforms),
	)
	return nil
}

// onPod reconciles a pod's image set and queues whatever needs inspecting.
// It is used for both add and update events.
func (w *Watcher) onPod(pod *corev1.Pod) {
	podRef := pod.Namespace + "/" + pod.Name
	auth := inspector.PodAuth{
		Namespace:          pod.Namespace,
		ServiceAccountName: pod.Spec.ServiceAccountName,
		ImagePullSecrets:   pullSecretNames(pod),
	}

	for _, imageRef := range w.store.SetPodImages(podRef, slices.Collect(uniqueImages(pod))) {
		w.rememberAuth(imageRef, auth)
		w.queue.Add(imageRef)
		w.logger.Info("new image detected, queuing inspection", "image", imageRef, "pod", podRef)
	}
}

func (w *Watcher) onDelete(pod *corev1.Pod) {
	podRef := pod.Namespace + "/" + pod.Name
	w.store.RemovePod(podRef)
	w.logger.Debug("pod removed", "pod", podRef)
}

func (w *Watcher) rememberAuth(imageRef string, auth inspector.PodAuth) {
	w.authMu.Lock()
	defer w.authMu.Unlock()
	w.authByImage[imageRef] = auth
}

func (w *Watcher) authFor(imageRef string) (inspector.PodAuth, bool) {
	w.authMu.Lock()
	defer w.authMu.Unlock()
	auth, ok := w.authByImage[imageRef]
	return auth, ok
}

func (w *Watcher) forgetAuth(imageRef string) {
	w.authMu.Lock()
	defer w.authMu.Unlock()
	delete(w.authByImage, imageRef)
}

// toPod extracts a *corev1.Pod from an event object, handling DeletedFinalStateUnknown.
func toPod(obj any) (*corev1.Pod, bool) {
	if pod, ok := obj.(*corev1.Pod); ok {
		return pod, true
	}
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		pod, ok := d.Obj.(*corev1.Pod)
		return pod, ok
	}
	return nil, false
}

// sameImages reports whether two revisions of a pod expose the same image set.
// Go 1.23: iter.Pull walks both sequences in lockstep, without materialising them.
func sameImages(a, b *corev1.Pod) bool {
	nextA, stopA := iter.Pull(uniqueImages(a))
	defer stopA()
	nextB, stopB := iter.Pull(uniqueImages(b))
	defer stopB()

	for {
		imgA, okA := nextA()
		imgB, okB := nextB()
		if okA != okB || imgA != imgB {
			return false
		}
		if !okA {
			return true
		}
	}
}

// uniqueImages returns an iterator over deduplicated, non-empty image refs from all
// container types (init, regular, ephemeral) in a pod.
// Go 1.23: returns iter.Seq[string] for use with range.
func uniqueImages(pod *corev1.Pod) iter.Seq[string] {
	return func(yield func(string) bool) {
		seen := make(map[string]struct{})
		for img := range containerImages(pod) {
			if img == "" {
				continue
			}
			if _, ok := seen[img]; ok {
				continue
			}
			seen[img] = struct{}{}
			if !yield(img) {
				return
			}
		}
	}
}

// containerImages yields raw image refs from all container types in a pod.
// Go 1.23: returns iter.Seq[string].
func containerImages(pod *corev1.Pod) iter.Seq[string] {
	return func(yield func(string) bool) {
		for _, c := range pod.Spec.InitContainers {
			if !yield(c.Image) {
				return
			}
		}
		for _, c := range pod.Spec.Containers {
			if !yield(c.Image) {
				return
			}
		}
		for _, c := range pod.Spec.EphemeralContainers {
			if !yield(c.Image) {
				return
			}
		}
	}
}

func pullSecretNames(pod *corev1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.ImagePullSecrets))
	for _, s := range pod.Spec.ImagePullSecrets {
		names = append(names, s.Name)
	}
	return names
}

// shortDigest returns the first shortDigestLen characters of a digest for log display.
func shortDigest(digest string) string {
	if len(digest) > shortDigestLen {
		return digest[:shortDigestLen]
	}
	return digest
}
