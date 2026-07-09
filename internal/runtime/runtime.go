// Package runtime abstracts the sandbox lifecycle over a container platform.
// The initial implementation targets Kubernetes (pods as sandboxes), but the
// interface is deliberately platform-agnostic.
package runtime

import (
	"context"
	"time"
)

// SandboxSpec describes a sandbox to be created.
type SandboxSpec struct {
	// SessionID is the stable identity applied as a label/annotation.
	SessionID string
	// Image is the container image to run.
	Image string
	// RuntimeClass selects the isolation runtime (gvisor/kata/""=default).
	RuntimeClass string
	// ProxyEndpoint is the HTTPS_PROXY value injected into the sandbox
	// (host:port of the assigned MITM proxy). May be empty for pool warm-up.
	ProxyEndpoint string
	// CACert is the PEM-encoded proxy CA cert to inject into the trust store.
	CACert []byte
	// Labels are extra labels applied to the pod (e.g. pool membership).
	Labels map[string]string
	// Warm indicates a pre-warmed pool sandbox not yet bound to a session.
	Warm bool
	// RunAsRoot runs the container as UID 0 (and disables RunAsNonRoot) so an
	// arbitrary image can install system packages into itself.
	RunAsRoot bool
	// WritableRoot makes the container root filesystem writable (package
	// installs). /workspace and /tmp are always writable regardless.
	WritableRoot bool
}

// SandboxHandle identifies a created sandbox at the platform level.
type SandboxHandle struct {
	PodName string
	PVCName string
	// Phase is the raw platform phase (e.g. Pending/Running).
	Phase string
	// Ready reports whether the sandbox is ready for exec.
	Ready bool
}

// SandboxRef is a lightweight reference to an existing sandbox pod discovered
// by listing the platform. Used for orphan reconciliation: because the session
// store is in-memory and lost on orchestrator restart, pods created before a
// restart are no longer tracked and must be swept directly from the platform.
type SandboxRef struct {
	PodName   string
	PVCName   string
	SessionID string
	// Pool is true when the pod belongs to the warm pool (managed separately).
	Pool      bool
	CreatedAt time.Time
}

// WorkspaceRef references a retained workspace volume (PVC) discovered by
// listing the platform, for orphan reconciliation.
type WorkspaceRef struct {
	PVCName   string
	PodName   string
	SessionID string
	Pool      bool
	CreatedAt time.Time
}

// Runtime creates and manages sandbox instances on a container platform.
type Runtime interface {
	// Create schedules a new sandbox and returns its handle. It does not wait
	// for readiness; use WaitReady.
	Create(ctx context.Context, spec SandboxSpec) (*SandboxHandle, error)
	// WaitReady blocks until the sandbox is ready or the context/timeout fires.
	WaitReady(ctx context.Context, podName string, timeout time.Duration) error
	// Stop deletes the pod but retains the workspace PVC so the sandbox can be
	// resumed later.
	Stop(ctx context.Context, podName string) error
	// Resume re-creates a pod bound to a retained workspace PVC.
	Resume(ctx context.Context, spec SandboxSpec, pvcName string) (*SandboxHandle, error)
	// Purge deletes the pod (if any) and the workspace PVC permanently.
	Purge(ctx context.Context, podName, pvcName string) error
	// Get returns the current handle/status for a pod.
	Get(ctx context.Context, podName string) (*SandboxHandle, error)
	// ListSandboxes returns all sandbox pods currently on the platform. Used to
	// reconcile orphans that outlived the in-memory session store.
	ListSandboxes(ctx context.Context) ([]SandboxRef, error)
	// ListWorkspaces returns all retained workspace PVCs on the platform.
	ListWorkspaces(ctx context.Context) ([]WorkspaceRef, error)
}
