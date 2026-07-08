package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the top-level orchestrator configuration. All values have sane
// defaults and can be overridden via environment variables (SANDBOX_* prefix).
type Config struct {
	// HTTP / MCP listen address for the orchestrator (serves both the REST API
	// and the streamable-HTTP MCP endpoint).
	ListenAddr string

	Kube     KubeConfig
	Sandbox  SandboxConfig
	Pool     PoolConfig
	Lifetime LifetimeConfig
	Proxy    ProxyConfig
}

// KubeConfig controls how the orchestrator talks to Kubernetes.
type KubeConfig struct {
	// Kubeconfig path. Empty => in-cluster config.
	Kubeconfig string
	// Namespace in which sandbox pods and PVCs are created.
	Namespace string
}

// SandboxConfig describes defaults for newly created sandboxes.
type SandboxConfig struct {
	// DefaultImage is used when a session is created without an explicit image.
	DefaultImage string
	// RuntimeClass selects the isolation runtime (e.g. "gvisor", "kata").
	// Empty string means the cluster default runtime (hardened baseline only).
	RuntimeClass string
	// KataRuntimeClass is the optional stronger-isolation runtime class name.
	KataRuntimeClass string
	// WorkspacePath is the writable working directory mounted into the sandbox.
	WorkspacePath string
	// WorkspaceSize is the PVC size for the persistent workspace (e.g. "5Gi").
	WorkspaceSize string
	// CPULimit / MemoryLimit are the per-sandbox resource limits.
	CPULimit    string
	MemoryLimit string
	// PidsLimit caps the number of processes inside a sandbox.
	PidsLimit int64
	// RunAsUser is the non-root UID the sandbox process runs as.
	RunAsUser int64
	// CACertPath is where the trusted proxy CA cert is mounted inside the pod.
	CACertPath string
	// ImagePullPolicy for the sandbox container ("IfNotPresent", "Always",
	// "Never"). Defaults to "IfNotPresent" so locally loaded images (e.g. kind)
	// are used without attempting a registry pull.
	ImagePullPolicy string
}

// PoolConfig controls the warm pool and global scaling limits.
type PoolConfig struct {
	// MinIdleReady is the number of pre-warmed, unassigned sandboxes (default
	// image) kept ready to accept incoming sessions.
	MinIdleReady int
	// MaxRunning is the hard cap on simultaneously running sandboxes.
	MaxRunning int
	// MaxStopped is the hard cap on retained stopped sandboxes.
	MaxStopped int
}

// LifetimeConfig controls automatic reaping of sandboxes.
type LifetimeConfig struct {
	// Running sandboxes are reaped (stopped) after this duration.
	RunningTTL time.Duration
	// Stopped sandboxes are purged (PVC + metadata deleted) after this duration.
	StoppedTTL time.Duration
	// ReapInterval is how often the reaper scans for expired sandboxes.
	ReapInterval time.Duration
}

// ProxyConfig controls the MITM proxy pool assignment.
type ProxyConfig struct {
	// PoolSize is the number of mitmproxy instances in the pool.
	PoolSize int
	// SandboxesPerProxy is the target group size (pool-per-group topology).
	SandboxesPerProxy int
	// ServiceName / Port is how sandboxes reach their assigned proxy.
	ServiceName string
	Port        int
}

// Default returns a Config populated with sensible defaults.
func Default() Config {
	return Config{
		ListenAddr: ":8080",
		Kube: KubeConfig{
			Namespace: "sandboxes",
		},
		Sandbox: SandboxConfig{
			DefaultImage:     "ghcr.io/megashrieks/sandbox-default:latest",
			RuntimeClass:     "gvisor",
			KataRuntimeClass: "kata",
			WorkspacePath:    "/workspace",
			WorkspaceSize:    "5Gi",
			CPULimit:         "2",
			MemoryLimit:      "2Gi",
			PidsLimit:        512,
			RunAsUser:        1000,
			CACertPath:       "/etc/sandbox/ca.crt",
			ImagePullPolicy:  "IfNotPresent",
		},
		Pool: PoolConfig{
			MinIdleReady: 2,
			MaxRunning:   50,
			MaxStopped:   100,
		},
		Lifetime: LifetimeConfig{
			RunningTTL:   time.Hour,
			StoppedTTL:   24 * time.Hour,
			ReapInterval: time.Minute,
		},
		Proxy: ProxyConfig{
			PoolSize:          2,
			SandboxesPerProxy: 10,
			ServiceName:       "mitmproxy",
			Port:              8080,
		},
	}
}

// Load builds a Config from defaults overlaid with SANDBOX_* environment vars.
func Load() (Config, error) {
	c := Default()

	c.ListenAddr = env("SANDBOX_LISTEN_ADDR", c.ListenAddr)
	c.Kube.Kubeconfig = env("SANDBOX_KUBECONFIG", c.Kube.Kubeconfig)
	c.Kube.Namespace = env("SANDBOX_NAMESPACE", c.Kube.Namespace)

	c.Sandbox.DefaultImage = env("SANDBOX_DEFAULT_IMAGE", c.Sandbox.DefaultImage)
	c.Sandbox.RuntimeClass = envAllowEmpty("SANDBOX_RUNTIME_CLASS", c.Sandbox.RuntimeClass)
	c.Sandbox.KataRuntimeClass = env("SANDBOX_KATA_RUNTIME_CLASS", c.Sandbox.KataRuntimeClass)
	c.Sandbox.ImagePullPolicy = env("SANDBOX_IMAGE_PULL_POLICY", c.Sandbox.ImagePullPolicy)
	c.Sandbox.WorkspaceSize = env("SANDBOX_WORKSPACE_SIZE", c.Sandbox.WorkspaceSize)

	var err error
	if c.Pool.MinIdleReady, err = envInt("SANDBOX_MIN_IDLE_READY", c.Pool.MinIdleReady); err != nil {
		return c, err
	}
	if c.Pool.MaxRunning, err = envInt("SANDBOX_MAX_RUNNING", c.Pool.MaxRunning); err != nil {
		return c, err
	}
	if c.Pool.MaxStopped, err = envInt("SANDBOX_MAX_STOPPED", c.Pool.MaxStopped); err != nil {
		return c, err
	}
	if c.Lifetime.RunningTTL, err = envDuration("SANDBOX_RUNNING_TTL", c.Lifetime.RunningTTL); err != nil {
		return c, err
	}
	if c.Lifetime.StoppedTTL, err = envDuration("SANDBOX_STOPPED_TTL", c.Lifetime.StoppedTTL); err != nil {
		return c, err
	}

	return c, c.Validate()
}

// Validate checks the configuration for internal consistency.
func (c Config) Validate() error {
	if c.Pool.MinIdleReady > c.Pool.MaxRunning {
		return fmt.Errorf("MinIdleReady (%d) cannot exceed MaxRunning (%d)", c.Pool.MinIdleReady, c.Pool.MaxRunning)
	}
	if c.Lifetime.RunningTTL <= 0 || c.Lifetime.StoppedTTL <= 0 {
		return fmt.Errorf("lifetime TTLs must be positive")
	}
	if c.Kube.Namespace == "" {
		return fmt.Errorf("kube namespace is required")
	}
	return nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// envAllowEmpty returns the env value when the variable is set, even to an empty
// string, so an operator can explicitly clear a defaulted value (e.g. set
// SANDBOX_RUNTIME_CLASS="" to disable gVisor and use the hardened baseline).
func envAllowEmpty(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Errorf("invalid int for %s: %w", key, err)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("invalid duration for %s: %w", key, err)
	}
	return d, nil
}
