package watcher

import (
	"context"
	"fmt"
	"iter"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/PixiBixi/kubearch/internal/inspector"
	"github.com/PixiBixi/kubearch/internal/store"
)

const maxConcurrentInspections = 10

// Watcher watches Kubernetes pod events and triggers image inspections for new images.
type Watcher struct {
	client    kubernetes.Interface
	namespace string
	store     *store.Store
	inspector *inspector.Inspector
	logger    *slog.Logger
	sem       chan struct{} // limits concurrent registry fetches
}

func New(client kubernetes.Interface, namespace string, s *store.Store, insp *inspector.Inspector, logger *slog.Logger) *Watcher {
	return &Watcher{
		client:    client,
		namespace: namespace,
		store:     s,
		inspector: insp,
		logger:    logger,
		sem:       make(chan struct{}, maxConcurrentInspections),
	}
}

// Run starts the pod informer and blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		w.client, 0, // no periodic resync — we rely on watch events
		informers.WithNamespace(w.namespace),
	)

	podInformer := factory.Core().V1().Pods().Informer()
	if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if pod, ok := obj.(*corev1.Pod); ok {
				w.onAdd(ctx, pod)
			}
		},
		// UpdateFunc intentionally omitted: container spec is immutable in Kubernetes.
		DeleteFunc: func(obj any) {
			if pod, ok := toPod(obj); ok {
				w.onDelete(pod)
			}
		},
	}); err != nil {
		w.logger.Error("failed to register pod event handler", "err", err)
		return
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced) {
		w.logger.Error("timed out waiting for cache sync")
		return
	}

	ns := w.namespace
	if ns == "" {
		ns = "all"
	}
	w.logger.Info("cache synced, watching pods", "namespace", ns)
	<-ctx.Done()
}

func (w *Watcher) onAdd(ctx context.Context, pod *corev1.Pod) {
	podRef := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	auth := inspector.PodAuth{
		Namespace:          pod.Namespace,
		ServiceAccountName: pod.Spec.ServiceAccountName,
		ImagePullSecrets:   pullSecretNames(pod),
	}

	for imageRef := range uniqueImages(pod) {
		if !w.store.TrackPodImage(podRef, imageRef) {
			continue // already known or inspection in progress
		}

		w.logger.Info("new image detected, queuing inspection", "image", imageRef, "pod", podRef)

		// Go 1.22+: loop variable is scoped per iteration, no explicit capture needed.
		go func() {
			w.sem <- struct{}{}
			defer func() { <-w.sem }()

			digest, platforms, err := w.inspector.Inspect(ctx, imageRef, auth)
			if err != nil {
				w.logger.Error("inspection failed", "image", imageRef, "err", err)
				w.store.FailImage(imageRef)
				return
			}

			w.store.SetImage(imageRef, digest, platforms)
			w.logger.Info("inspection done",
				"image", imageRef,
				"digest", shortDigest(digest),
				"platforms", len(platforms),
			)
		}()
	}
}

func (w *Watcher) onDelete(pod *corev1.Pod) {
	podRef := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	w.store.RemovePod(podRef)
	w.logger.Debug("pod removed", "pod", podRef)
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

// uniqueImages returns an iterator over deduplicated, non-empty image refs from all
// container types (init, regular, ephemeral) in a pod.
// Go 1.23: returns iter.Seq[string] for use with range.
func uniqueImages(pod *corev1.Pod) iter.Seq[string] {
	return func(yield func(string) bool) {
		seen := make(map[string]bool)
		for img := range containerImages(pod) {
			if img == "" || seen[img] {
				continue
			}
			seen[img] = true
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

// shortDigest returns the first 19 characters of a digest (algo + 12 chars of hash).
func shortDigest(digest string) string {
	if len(digest) > 19 {
		return digest[:19]
	}
	return digest
}
