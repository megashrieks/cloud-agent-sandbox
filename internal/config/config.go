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
	Security SecurityConfig
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
	// CPURequest / MemoryRequest are the scheduling requests (guaranteed floor).
	// Setting requests makes the scheduler account for sandbox load and prevents
	// noisy-neighbour starvation.
	CPURequest    string
	MemoryRequest string
	// EphemeralStorageLimit / EphemeralStorageRequest cap the container's
	// writable-layer + emptyDir usage so a sandbox cannot fill the node disk and
	// trigger DiskPressure evictions of other pods.
	EphemeralStorageLimit   string
	EphemeralStorageRequest string
	// TmpSizeLimit bounds the /tmp emptyDir (defense in depth alongside the
	// ephemeral-storage limit).
	TmpSizeLimit string
	// PidsLimit caps the number of processes inside a sandbox.
	PidsLimit int64
	// SeccompProfilePath, when set, selects a Localhost seccomp profile (a path
	// under the kubelet seccomp root, e.g. "profiles/sandbox-seccomp.json")
	// instead of RuntimeDefault. Use it to ship a hardened profile that also
	// blocks io_uring (a recurrent kernel-LPE surface RuntimeDefault permits).
	// Empty => RuntimeDefault. The profile MUST be present on every node.
	SeccompProfilePath string
	// RunAsUser is the non-root UID the sandbox process runs as.
	RunAsUser int64
	// RunAsRoot, when true, runs the sandbox container as root (UID 0) and
	// disables the RunAsNonRoot guard. Needed so an arbitrary user-chosen image
	// can install system packages (apk/apt/dnf) into itself. This is the DEFAULT
	// for the session unless overridden per-session. Isolation still relies on
	// the runtime class (gVisor/Kata), dropped capabilities, seccomp, no service
	// account token, and the egress NetworkPolicy. Root here is root INSIDE the
	// sandbox only, not on the host.
	RunAsRoot bool
	// WritableRootFilesystem, when true, makes the container root filesystem
	// writable so package managers can install into /usr, /var, /etc. This is
	// the DEFAULT for the session unless overridden per-session. /workspace and
	// /tmp are always writable regardless.
	WritableRootFilesystem bool
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
	// RunningTTL is the IDLE timeout: a running sandbox is stopped after this
	// much time with no MCP activity (measured from LastActivityAt, which is
	// refreshed at the start and end of every MCP tool call). A sandbox with an
	// in-flight MCP call is never considered idle.
	RunningTTL time.Duration
	// MaxLifetime is the hard cap on total running time: a sandbox is stopped
	// once it has been running this long since creation, regardless of activity.
	MaxLifetime time.Duration
	// Stopped sandboxes are purged (PVC + metadata deleted) after this duration.
	StoppedTTL time.Duration
	// ReapInterval is how often the reaper scans for expired sandboxes.
	ReapInterval time.Duration
}

// ProxyConfig controls the MITM proxy pool assignment and autoscaling.
type ProxyConfig struct {
	// PoolSize is the number of mitmproxy instances in the pool.
	PoolSize int
	// SandboxesPerProxy is the target group size: the autoscaler runs roughly
	// one mitmproxy replica per this many active sandbox pods (pool-per-group).
	SandboxesPerProxy int
	// ServiceName / Port is how sandboxes reach their assigned proxy.
	ServiceName string
	Port        int
	// DeploymentName is the mitmproxy Deployment the autoscaler scales.
	DeploymentName string
	// Autoscale enables dynamic scaling of the mitmproxy Deployment based on
	// the number of active sandbox pods. When true, the orchestrator overrides
	// the Deployment's replica count.
	Autoscale bool
	// MinReplicas is the floor the autoscaler scales down to when there are no
	// sandboxes. Default 0 (scale-to-zero: no proxy runs when idle).
	MinReplicas int
	// MaxReplicas caps how many mitmproxy replicas the autoscaler will create.
	MaxReplicas int
	// AutoscaleInterval is how often the autoscaler reconciles replicas.
	AutoscaleInterval time.Duration
	// ScaleUpTimeout bounds how long create_session waits for a proxy replica to
	// become Ready when scaling up from zero on demand.
	ScaleUpTimeout time.Duration
}

// SecurityConfig controls fail-closed safety checks at startup.
type SecurityConfig struct {
	// RequireNetworkPolicy makes the orchestrator refuse to start unless it can
	// confirm the cluster will enforce the sandbox egress NetworkPolicy. This
	// converts a silent fail-OPEN (policy present but unenforced) into a loud
	// fail-CLOSED. Default true. Override with SANDBOX_REQUIRE_NETWORK_POLICY.
	RequireNetworkPolicy bool
	// NetworkPolicyEnforced, when non-nil, overrides CNI auto-detection:
	// true  => operator asserts enforcement is active (e.g. GKE Dataplane V2,
	//          EKS VPC-CNI NetworkPolicy) that we can't fingerprint.
	// false => force the check to fail.
	// Set via SANDBOX_NETWORK_POLICY_ENFORCED (unset => auto-detect).
	NetworkPolicyEnforced *bool
}

// Default returns a Config populated with sensible defaults.
func Default() Config {
	return Config{
		ListenAddr: ":8080",
		Kube: KubeConfig{
			Namespace: "sandboxes",
		},
		Sandbox: SandboxConfig{
			DefaultImage:     "sandbox-default:latest",
			RuntimeClass:     "gvisor",
			KataRuntimeClass: "kata",
			WorkspacePath:    "/workspace",
			WorkspaceSize:    "5Gi",
			CPULimit:         "2",
			MemoryLimit:      "2Gi",
			CPURequest:       "250m",
			MemoryRequest:    "256Mi",
			EphemeralStorageLimit:   "2Gi",
			EphemeralStorageRequest: "256Mi",
			TmpSizeLimit:            "1Gi",
			PidsLimit:        512,
			RunAsUser:        1000,
			RunAsRoot:              true,
			WritableRootFilesystem: true,
			CACertPath:       "/etc/sandbox/ca.crt",
			ImagePullPolicy:  "IfNotPresent",
		},
		Pool: PoolConfig{
			MinIdleReady: 2,
			MaxRunning:   50,
			MaxStopped:   100,
		},
		Lifetime: LifetimeConfig{
			RunningTTL:   10 * time.Minute,
			MaxLifetime:  time.Hour,
			StoppedTTL:   24 * time.Hour,
			ReapInterval: time.Minute,
		},
		Proxy: ProxyConfig{
			PoolSize:          2,
			SandboxesPerProxy: 100,
			ServiceName:       "mitmproxy",
			Port:              8080,
			DeploymentName:    "mitmproxy",
			Autoscale:         true,
			MinReplicas:       0,
			MaxReplicas:       10,
			AutoscaleInterval: 15 * time.Second,
			ScaleUpTimeout:    60 * time.Second,
		},
		Security: SecurityConfig{
			RequireNetworkPolicy: true,
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
	c.Sandbox.CPULimit = env("SANDBOX_CPU_LIMIT", c.Sandbox.CPULimit)
	c.Sandbox.MemoryLimit = env("SANDBOX_MEMORY_LIMIT", c.Sandbox.MemoryLimit)
	c.Sandbox.CPURequest = env("SANDBOX_CPU_REQUEST", c.Sandbox.CPURequest)
	c.Sandbox.MemoryRequest = env("SANDBOX_MEMORY_REQUEST", c.Sandbox.MemoryRequest)
	c.Sandbox.EphemeralStorageLimit = env("SANDBOX_EPHEMERAL_STORAGE_LIMIT", c.Sandbox.EphemeralStorageLimit)
	c.Sandbox.EphemeralStorageRequest = env("SANDBOX_EPHEMERAL_STORAGE_REQUEST", c.Sandbox.EphemeralStorageRequest)
	c.Sandbox.TmpSizeLimit = env("SANDBOX_TMP_SIZE_LIMIT", c.Sandbox.TmpSizeLimit)
	c.Sandbox.SeccompProfilePath = env("SANDBOX_SECCOMP_PROFILE_PATH", c.Sandbox.SeccompProfilePath)

	var err error
	if c.Sandbox.RunAsRoot, err = envBool("SANDBOX_RUN_AS_ROOT", c.Sandbox.RunAsRoot); err != nil {
		return c, err
	}
	if c.Sandbox.WritableRootFilesystem, err = envBool("SANDBOX_WRITABLE_ROOTFS", c.Sandbox.WritableRootFilesystem); err != nil {
		return c, err
	}
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
	if c.Lifetime.MaxLifetime, err = envDuration("SANDBOX_MAX_LIFETIME", c.Lifetime.MaxLifetime); err != nil {
		return c, err
	}
	if c.Lifetime.StoppedTTL, err = envDuration("SANDBOX_STOPPED_TTL", c.Lifetime.StoppedTTL); err != nil {
		return c, err
	}

	c.Proxy.DeploymentName = env("SANDBOX_PROXY_DEPLOYMENT", c.Proxy.DeploymentName)
	if c.Proxy.Autoscale, err = envBool("SANDBOX_PROXY_AUTOSCALE", c.Proxy.Autoscale); err != nil {
		return c, err
	}
	if c.Proxy.SandboxesPerProxy, err = envInt("SANDBOX_SANDBOXES_PER_PROXY", c.Proxy.SandboxesPerProxy); err != nil {
		return c, err
	}
	if c.Proxy.MinReplicas, err = envInt("SANDBOX_PROXY_MIN_REPLICAS", c.Proxy.MinReplicas); err != nil {
		return c, err
	}
	if c.Proxy.MaxReplicas, err = envInt("SANDBOX_PROXY_MAX_REPLICAS", c.Proxy.MaxReplicas); err != nil {
		return c, err
	}
	if c.Proxy.AutoscaleInterval, err = envDuration("SANDBOX_PROXY_AUTOSCALE_INTERVAL", c.Proxy.AutoscaleInterval); err != nil {
		return c, err
	}
	if c.Proxy.ScaleUpTimeout, err = envDuration("SANDBOX_PROXY_SCALE_UP_TIMEOUT", c.Proxy.ScaleUpTimeout); err != nil {
		return c, err
	}

	if c.Security.RequireNetworkPolicy, err = envBool("SANDBOX_REQUIRE_NETWORK_POLICY", c.Security.RequireNetworkPolicy); err != nil {
		return c, err
	}
	if c.Security.NetworkPolicyEnforced, err = envBoolPtr("SANDBOX_NETWORK_POLICY_ENFORCED"); err != nil {
		return c, err
	}

	return c, c.Validate()
}

// Validate checks the configuration for internal consistency.
func (c Config) Validate() error {
	if c.Pool.MinIdleReady > c.Pool.MaxRunning {
		return fmt.Errorf("MinIdleReady (%d) cannot exceed MaxRunning (%d)", c.Pool.MinIdleReady, c.Pool.MaxRunning)
	}
	if c.Lifetime.RunningTTL <= 0 || c.Lifetime.StoppedTTL <= 0 || c.Lifetime.MaxLifetime <= 0 {
		return fmt.Errorf("lifetime TTLs must be positive")
	}
	if c.Proxy.Autoscale {
		if c.Proxy.SandboxesPerProxy <= 0 {
			return fmt.Errorf("SandboxesPerProxy must be positive when proxy autoscaling is enabled")
		}
		if c.Proxy.MinReplicas < 0 {
			return fmt.Errorf("proxy MinReplicas cannot be negative")
		}
		if c.Proxy.MaxReplicas > 0 && c.Proxy.MinReplicas > c.Proxy.MaxReplicas {
			return fmt.Errorf("proxy MinReplicas (%d) cannot exceed MaxReplicas (%d)", c.Proxy.MinReplicas, c.Proxy.MaxReplicas)
		}
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

func envBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("invalid bool for %s: %w", key, err)
	}
	return b, nil
}

// envBoolPtr returns nil when the variable is unset (meaning "auto-detect"), or
// a pointer to the parsed boolean when it is set.
func envBoolPtr(key string) (*bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil, fmt.Errorf("invalid bool for %s: %w", key, err)
	}
	return &b, nil
}
