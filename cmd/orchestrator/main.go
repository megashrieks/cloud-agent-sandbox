package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/megashrieks/cloud-agent-sandbox/internal/api"
	"github.com/megashrieks/cloud-agent-sandbox/internal/config"
	"github.com/megashrieks/cloud-agent-sandbox/internal/exec"
	"github.com/megashrieks/cloud-agent-sandbox/internal/kube"
	"github.com/megashrieks/cloud-agent-sandbox/internal/manager"
	mcpserver "github.com/megashrieks/cloud-agent-sandbox/internal/mcp"
	"github.com/megashrieks/cloud-agent-sandbox/internal/netpolicy"
	"github.com/megashrieks/cloud-agent-sandbox/internal/pool"
	"github.com/megashrieks/cloud-agent-sandbox/internal/proxy"
	"github.com/megashrieks/cloud-agent-sandbox/internal/reaper"
	"github.com/megashrieks/cloud-agent-sandbox/internal/runtime"
	"github.com/megashrieks/cloud-agent-sandbox/internal/session"
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

	// Fail closed: the control plane (REST + MCP) must never be exposed without
	// authentication. A malicious sandbox that finds the orchestrator address
	// (or any other reachable client) could otherwise list, create, or destroy
	// arbitrary tenants' sandboxes.
	if cfg.Security.APIKey == "" {
		return fmt.Errorf("SANDBOX_API_KEY is required (refusing to start an unauthenticated orchestrator)")
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

	// Fail-closed guard: confirm the cluster will actually ENFORCE the sandbox
	// egress NetworkPolicy before we ever schedule untrusted code. A policy that
	// is present but unenforced (no policy-capable CNI) silently leaks egress.
	npRes, npErr := netpolicy.Verify(ctx, kc.Clientset, netpolicy.Options{
		Namespace:        cfg.Kube.Namespace,
		EnforcedOverride: cfg.Security.NetworkPolicyEnforced,
	})
	if npErr != nil {
		if cfg.Security.RequireNetworkPolicy {
			return fmt.Errorf("refusing to start (sandbox egress containment unverified): %w; "+
				"set SANDBOX_REQUIRE_NETWORK_POLICY=false to bypass at your own risk", npErr)
		}
		logger.Warn("network policy enforcement NOT confirmed; sandbox egress may be uncontained",
			"reason", npRes.Reason)
	} else {
		logger.Info("network policy enforcement confirmed",
			"policy", npRes.PolicyName, "enforcer", npRes.EnforcerName)
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

	// Proxy autoscaler: ~1 mitmproxy per SandboxesPerProxy active sandbox pods,
	// scale-to-zero when idle, on-demand scale-up before a sandbox starts.
	if cfg.Proxy.Autoscale && proxyAssigner != nil {
		as := proxy.NewAutoscaler(kc.Clientset, cfg.Proxy, cfg.Kube.Namespace)
		mgr.SetProxyScaler(as)
		go as.Run(ctx)
		logger.Info("proxy autoscaler enabled",
			"sandboxes_per_proxy", cfg.Proxy.SandboxesPerProxy,
			"min_replicas", cfg.Proxy.MinReplicas,
			"max_replicas", cfg.Proxy.MaxReplicas)
	}

	// Lifetime reaper (idle + max-lifetime running / stopped purge).
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
	authed := requireAPIKey(cfg.Security.APIKey, logger)
	mux.Handle("/sessions", authed(apiHandler))
	mux.Handle("/sessions/", authed(apiHandler))
	mux.Handle("/mcp", authed(mcpHandler))
	mux.Handle("/mcp/", authed(mcpHandler))

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

// requireAPIKey wraps a handler so every request must present the shared secret
// as "Authorization: Bearer <key>". The comparison is constant-time to avoid
// leaking the key through timing. /healthz is registered separately and stays
// open for liveness probes.
func requireAPIKey(key string, logger *slog.Logger) func(http.Handler) http.Handler {
	want := []byte("Bearer " + key)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("Authorization")
			if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
				logger.Warn("rejected unauthenticated request", "path", r.URL.Path, "remote", r.RemoteAddr)
				w.Header().Set("WWW-Authenticate", "Bearer")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
