package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/PixiBixi/kubearch/internal/collector"
	"github.com/PixiBixi/kubearch/internal/inspector"
	"github.com/PixiBixi/kubearch/internal/store"
	"github.com/PixiBixi/kubearch/internal/watcher"
)

// Injected at build time by GoReleaser via ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	var (
		addr        = flag.String("listen-address", ":9101", "Address to expose Prometheus metrics on")
		namespace   = flag.String("namespace", "", "Kubernetes namespace to watch (empty = all namespaces)")
		kubeconfig  = flag.String("kubeconfig", "", "Path to kubeconfig file (empty = auto-detect)")
		kubeContext = flag.String("context", "", "Kubernetes context to use (empty = current context)")
		showVersion = flag.Bool("version", false, "Print version information and exit")
		digestLabel = flag.Bool("digest-label", true, "Include the digest label on image metrics (disable to avoid a new time series on every image rebuild of the same tag)")
		reinspect   = flag.Duration("reinspect-interval", 0, "Re-inspect known images this often to follow mutable tags (0 = inspect each image once)")
	)
	flag.Parse()

	if *reinspect < 0 {
		fmt.Fprintf(os.Stderr, "-reinspect-interval must not be negative, got %s\n", *reinspect)
		os.Exit(2)
	}

	if *showVersion {
		fmt.Printf("kubearch %s (commit: %s, built: %s)\n", version, commit, date)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("starting kubearch",
		"version", version,
		"commit", commit,
		"addr", *addr,
		"namespace", nsLabel(*namespace),
		"reinspectInterval", *reinspect,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	k8sClient, err := buildK8sClient(*kubeconfig, *kubeContext, logger)
	if err != nil {
		logger.Error("failed to build Kubernetes client", "err", err)
		os.Exit(1)
	}

	s := store.New()
	insp := inspector.New(k8sClient, logger)

	// The watcher reports to selfMetrics and selfMetrics reports on the
	// watcher's queue, so one of the two references has to be late-bound.
	// QueueDepth is only ever called from a scrape, long after w is assigned;
	// the nil check is there so a future reordering degrades to a zero reading
	// instead of crashing the exporter.
	var w *watcher.Watcher
	queueDepth := func() int {
		if w == nil {
			return 0
		}
		return w.QueueDepth()
	}
	selfMetrics := collector.NewSelfMetrics(version, commit, s, queueDepth)
	w = watcher.New(k8sClient, *namespace, s, insp, logger, selfMetrics, watcher.WithReinspectInterval(*reinspect))

	reg := prometheus.NewRegistry()
	reg.MustRegister(collector.New(s, collector.WithDigestLabel(*digestLabel)))
	reg.MustRegister(selfMetrics)

	// A Run error with ctx still live is a real failure: record it so the
	// process exits non-zero once the server has drained.
	watcherErr := make(chan error, 1)
	go func() {
		if err := w.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("watcher exited with error", "err", err)
			watcherErr <- err
			stop()
		}
	}()

	// Go 1.22+: method+path pattern routing in http.ServeMux.
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", readyHandler(w.HasSynced))

	srv := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// Bound the header read phase: a scrape endpoint has no reason to keep
		// a half-open connection alive (Slowloris).
		ReadHeaderTimeout: 10 * time.Second,
		// Well above a typical scrape timeout, so a stuck client cannot pin a
		// connection forever.
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ListenAndServe returns as soon as Shutdown starts, so main must wait on
	// shutdownDone or it exits mid-drain.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown error", "err", err)
		}
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
	<-shutdownDone
	logger.Info("server stopped")

	select {
	case err := <-watcherErr:
		logger.Error("exiting after watcher failure", "err", err)
		os.Exit(1)
	default:
	}
}

// shutdownTimeout bounds how long in-flight scrapes get to finish on SIGTERM.
const shutdownTimeout = 10 * time.Second

// readyHandler answers 200 once synced reports true, 503 before: the exporter
// has nothing meaningful to serve until the informer cache is populated.
func readyHandler(synced func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !synced() {
			http.Error(w, "informer cache not synced", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// buildK8sClient resolves the Kubernetes client config with the following priority:
//  1. In-cluster config  (when running inside a pod, no flags needed)
//  2. Explicit kubeconfig/context flags  (--kubeconfig, --context)
//  3. Default kubeconfig  (KUBECONFIG env var or ~/.kube/config, current context)
func buildK8sClient(kubeconfig, kubeContext string, logger *slog.Logger) (kubernetes.Interface, error) {
	// Attempt in-cluster only when the user hasn't forced a specific config.
	if kubeconfig == "" && kubeContext == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			logger.Info("config: in-cluster")
			return kubernetes.NewForConfig(cfg)
		}
	}

	// Standalone: build from kubeconfig file (explicit path, KUBECONFIG env, or ~/.kube/config).
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kubeContext != "" {
		overrides.CurrentContext = kubeContext
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, err
	}

	ctxName := kubeContext
	if ctxName == "" {
		ctxName = "current"
	}
	logger.Info("config: kubeconfig", "context", ctxName)
	return kubernetes.NewForConfig(cfg)
}

func nsLabel(ns string) string {
	if ns == "" {
		return "all"
	}
	return ns
}
