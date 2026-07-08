package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/megashrieks/sandbox-orchestrator/internal/api"
	"github.com/megashrieks/sandbox-orchestrator/internal/config"
	"github.com/megashrieks/sandbox-orchestrator/internal/exec"
	"github.com/megashrieks/sandbox-orchestrator/internal/kube"
	"github.com/megashrieks/sandbox-orchestrator/internal/manager"
	mcpserver "github.com/megashrieks/sandbox-orchestrator/internal/mcp"
	"github.com/megashrieks/sandbox-orchestrator/internal/pool"
	"github.com/megashrieks/sandbox-orchestrator/internal/proxy"
	"github.com/megashrieks/sandbox-orchestrator/internal/reaper"
	"github.com/megashrieks/sandbox-orchestrator/internal/runtime"
	"github.com/megashrieks/sandbox-orchestrator/internal/session"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger.Info("starting sandbox orchestrator",
		"listen", cfg.ListenAddr,
		"namespace", cfg.Kube.Namespace,
		"runtimeClass", cfg.Sandbox.RuntimeClass,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Kubernetes client (shared by runtime + exec + proxy).
	kc, err := kube.New(cfg.Kube.Kubeconfig)
	if err != nil {
		return err
	}

	// Ensure the shared proxy CA exists before wiring the assigner (idempotent).
	if err := proxy.EnsureCASecret(ctx, kc.Clientset, cfg.Kube.Namespace); err != nil {
		logger.Warn("could not ensure proxy CA secret; proxy injection may be disabled", "err", err)
	}

	rt := runtime.NewKubeRuntime(kc.Clientset, cfg.Kube.Namespace, cfg.Sandbox)
	executor := exec.NewKubeExecutor(kc.Clientset, kc.Config, cfg.Kube.Namespace, "sandbox")

	// Proxy assigner (endpoint + shared CA). Degrade gracefully if unavailable.
	var proxyAssigner manager.ProxyAssigner
	var warmPool manager.Pool
	if pa, perr := proxy.NewServiceAssigner(ctx, kc.Clientset, cfg.Proxy, cfg.Kube.Namespace); perr != nil {
		logger.Warn("proxy assigner unavailable; running without credential injection", "err", perr)
	} else {
		proxyAssigner = pa
		// Warm pool pods are pre-wired to the shared proxy endpoint + CA.
		if endpoint, caCert, _, aerr := pa.Assign(ctx, "pool-warmup"); aerr == nil && cfg.Pool.MinIdleReady > 0 {
			wp := pool.NewWarmPool(rt, cfg, endpoint, caCert)
			wp.Start(ctx)
			warmPool = wp
		}
	}

	store := session.NewMemoryStore()
	mgr := manager.New(cfg, store, rt, warmPool, proxyAssigner)

	// Lifetime reaper (1h running / 24h stopped).
	rp := reaper.New(mgr)
	go rp.Run(ctx)

	// HTTP surface: health + REST API + streamable-HTTP MCP.
	apiHandler := api.New(mgr, logger).Routes()
	mcpHandler := mcpserver.New(mgr, executor, logger).Handler()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/sessions", apiHandler)
	mux.Handle("/sessions/", apiHandler)
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "err", err)
			stop()
		}
	}()
	logger.Info("orchestrator ready", "listen", cfg.ListenAddr)

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
