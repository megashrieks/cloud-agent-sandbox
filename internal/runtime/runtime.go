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
}
