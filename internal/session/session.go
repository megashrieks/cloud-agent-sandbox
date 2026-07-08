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
	// ID is the opaque, LLM-facing session identifier.
	ID string
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
	// Image overrides the default sandbox image when non-empty.
	Image string
	// ProxyGroup optionally pins the session to a specific proxy group.
	ProxyGroup string
	// UseKata requests the stronger (Kata) isolation runtime class.
	UseKata bool
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
