// Package session defines the core sandbox session model and the store contract.
// A session is the stable, LLM-facing identity for a sandbox; it outlives the
// underlying pod (a stopped session retains its workspace and can be resumed).
package session

import (
	"context"
	"errors"
	"time"
)

// State is the lifecycle state of a sandbox session.
type State string

const (
	// StateCreating: the pod is being scheduled / started.
	StateCreating State = "creating"
	// StateRunning: the pod is Ready and accepting exec.
	StateRunning State = "running"
	// StateStopped: the pod is deleted, workspace PVC + metadata retained.
	StateStopped State = "stopped"
	// StateDead: fully purged (terminal); kept briefly for observability.
	StateDead State = "dead"
)

// ErrNotFound is returned when a session id is unknown.
var ErrNotFound = errors.New("session not found")

// ErrInvalidSession is returned to tool callers when a session id is unknown,
// expired, or not currently usable. Tools surface this as an "invalid" result.
var ErrInvalidSession = errors.New("invalid session")

// Session is the persisted record for a sandbox.
type Session struct {
	// ID is the canonical, Kubernetes-safe session identifier (derived from Ref
	// via CanonicalID). It is the store key and the basis for pod/PVC names.
	ID string
	// Ref is the raw, caller-supplied session reference (the X-Session-Id
	// header) that ID was derived from. Kept for display and correlation.
	Ref string
	// OrgID / UserID are optional caller-supplied identity metadata (the
	// X-Org-Id / X-User-Id headers). Recorded for attribution only; they are not
	// used for access-control decisions.
	OrgID  string
	UserID string
	// State is the current lifecycle state.
	State State
	// Image is the container image backing this sandbox.
	Image string
	// RuntimeClass is the isolation runtime selected for this sandbox.
	RuntimeClass string
	// PodName / PVCName are the underlying Kubernetes object names.
	PodName string
	PVCName string
	// ProxyID is the assigned MITM proxy instance (pool-per-group).
	ProxyID string
	// FromPool indicates the sandbox was claimed from the warm pool.
	FromPool bool

	CreatedAt      time.Time
	LastActivityAt time.Time
	// StoppedAt is set when the session transitions to Stopped.
	StoppedAt time.Time
}

// Running reports whether the session is currently usable for exec.
func (s *Session) Running() bool { return s.State == StateRunning }

// CreateOptions are the inputs for creating a new session.
type CreateOptions struct {
	// Ref / OrgID / UserID are the caller-supplied identity headers. Ref is the
	// raw X-Session-Id; OrgID / UserID are optional attribution metadata.
	Ref    string
	OrgID  string
	UserID string
	// Image overrides the default sandbox image when non-empty.
	Image string
	// ProxyGroup optionally pins the session to a specific proxy group.
	ProxyGroup string
	// UseKata requests the stronger (Kata) isolation runtime class.
	UseKata bool
	// WritableRoot / RunAsRoot override the configured defaults for this
	// session. nil means "use the orchestrator's configured default". Set them
	// to allow (or forbid) system package installs in the chosen image.
	WritableRoot *bool
	RunAsRoot    *bool
}

// Store persists and retrieves sessions. Implementations must be safe for
// concurrent use.
type Store interface {
	Create(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	Update(ctx context.Context, s *Session) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]*Session, error)
	// ListByState returns sessions in the given state.
	ListByState(ctx context.Context, state State) ([]*Session, error)
	// Touch updates LastActivityAt to now for the given session.
	Touch(ctx context.Context, id string) error
}
