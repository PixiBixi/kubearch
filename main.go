package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

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
	)
	flag.Parse()

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
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	k8sClient, err := buildK8sClient(*kubeconfig, *kubeContext, logger)
	if err != nil {
		logger.Error("failed to build Kubernetes client", "err", err)
		os.Exit(1)
	}

	s := store.New()
	insp := inspector.New(k8sClient)
	w := watcher.New(k8sClient, *namespace, s, insp, logger)

	reg := prometheus.NewRegistry()
	reg.MustRegister(collector.New(s))

	go w.Run(ctx)

	// Go 1.22+: method+path pattern routing in http.ServeMux.
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		if err := srv.Shutdown(context.Background()); err != nil {
			logger.Error("graceful shutdown error", "err", err)
		}
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
	logger.Info("server stopped")
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

	ctx := kubeContext
	if ctx == "" {
		ctx = "current"
	}
	logger.Info("config: kubeconfig", "context", ctx)
	return kubernetes.NewForConfig(cfg)
}

func nsLabel(ns string) string {
	if ns == "" {
		return "all"
	}
	return ns
}
