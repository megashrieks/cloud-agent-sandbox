// Package manager is the integration core: it ties together the session store,
// the container runtime, the warm pool, the proxy assigner, and the executor to
// implement the full sandbox lifecycle. The REST API and the MCP server both
// drive sandboxes exclusively through a Manager.
//
// The Manager holds NO credentials. Real tokens live only in the user-configured
// MITM proxy; the Manager only injects the proxy endpoint and the proxy's CA
// cert into sandboxes.
package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/megashrieks/sandbox-orchestrator/internal/config"
	"github.com/megashrieks/sandbox-orchestrator/internal/runtime"
	"github.com/megashrieks/sandbox-orchestrator/internal/session"
)

// Pool is a warm pool of pre-created, ready sandboxes that can be claimed to
// satisfy a session quickly. Implemented by internal/pool. Satisfied
// structurally (implementations need not import this package).
type Pool interface {
	// Acquire hands over a ready warm sandbox for the given image if one is
	// available; ok=false means the caller must cold-create. The returned
	// handle's pod has already been unlabeled from the pool.
	Acquire(ctx context.Context, image string) (handle *runtime.SandboxHandle, ok bool)
	// Start launches background replenishment to maintain the configured
	// MinIdleReady. It returns immediately.
	Start(ctx context.Context)
	// Stats reports current pool occupancy.
	Stats() PoolStats
}

// PoolStats reports warm-pool occupancy.
type PoolStats struct {
	Idle    int
	Warming int
}

// ProxyAssigner assigns a session to a MITM proxy and provides the proxy's CA
// certificate for injection into the sandbox trust store. Implemented by
// internal/proxy.
type ProxyAssigner interface {
	// Assign returns the proxy endpoint (host:port) the sandbox must route
	// egress through, the PEM-encoded proxy CA cert to trust, and an opaque
	// proxy id for bookkeeping.
	Assign(ctx context.Context, sessionID string) (endpoint string, caCert []byte, proxyID string, err error)
	// Release frees any per-session proxy assignment state.
	Release(ctx context.Context, sessionID string)
}

// Manager owns sandbox session lifecycle.
type Manager struct {
	cfg     config.Config
	store   session.Store
	rt      runtime.Runtime
	pool    Pool
	proxy   ProxyAssigner
	readyTO time.Duration
}

// New builds a Manager. pool and proxy may be nil for degraded/dev modes
// (cold-create only, no proxy injection).
func New(cfg config.Config, store session.Store, rt runtime.Runtime, pool Pool, proxy ProxyAssigner) *Manager {
	return &Manager{
		cfg:     cfg,
		store:   store,
		rt:      rt,
		pool:    pool,
		proxy:   proxy,
		readyTO: 90 * time.Second,
	}
}

// Store exposes the underlying session store (read-only helpers for callers
// like the reaper).
func (m *Manager) Store() session.Store { return m.store }

// Runtime exposes the underlying runtime (used by the reaper to stop/purge).
func (m *Manager) Runtime() runtime.Runtime { return m.rt }

// Config exposes the effective configuration.
func (m *Manager) Config() config.Config { return m.cfg }

// Create provisions a new sandbox session and returns it once the sandbox is
// ready for exec.
func (m *Manager) Create(ctx context.Context, opts session.CreateOptions) (*session.Session, error) {
	image := opts.Image
	if image == "" {
		image = m.cfg.Sandbox.DefaultImage
	}

	// Enforce the global running cap.
	running, err := m.store.ListByState(ctx, session.StateRunning)
	if err != nil {
		return nil, fmt.Errorf("list running: %w", err)
	}
	creating, err := m.store.ListByState(ctx, session.StateCreating)
	if err != nil {
		return nil, fmt.Errorf("list creating: %w", err)
	}
	if len(running)+len(creating) >= m.cfg.Pool.MaxRunning {
		return nil, fmt.Errorf("max running sandboxes reached (%d)", m.cfg.Pool.MaxRunning)
	}

	id := session.NewID()
	runtimeClass := m.cfg.Sandbox.RuntimeClass
	if opts.UseKata && m.cfg.Sandbox.KataRuntimeClass != "" {
		runtimeClass = m.cfg.Sandbox.KataRuntimeClass
	}

	sess := &session.Session{
		ID:             id,
		State:          session.StateCreating,
		Image:          image,
		RuntimeClass:   runtimeClass,
		CreatedAt:      time.Now(),
		LastActivityAt: time.Now(),
	}

	// Assign a proxy (endpoint + CA) unless running without a proxy assigner.
	var endpoint string
	var caCert []byte
	if m.proxy != nil {
		ep, ca, proxyID, perr := m.proxy.Assign(ctx, id)
		if perr != nil {
			return nil, fmt.Errorf("assign proxy: %w", perr)
		}
		endpoint, caCert, sess.ProxyID = ep, ca, proxyID
	}

	// Try the warm pool first (default image only; warm pods are generic and
	// already wired to the shared proxy Service + CA).
	if m.pool != nil && image == m.cfg.Sandbox.DefaultImage {
		if handle, ok := m.pool.Acquire(ctx, image); ok {
			sess.PodName = handle.PodName
			sess.PVCName = handle.PVCName
			sess.FromPool = true
		}
	}

	// Cold-create if the pool did not satisfy us.
	if sess.PodName == "" {
		handle, cerr := m.rt.Create(ctx, runtime.SandboxSpec{
			SessionID:     id,
			Image:         image,
			RuntimeClass:  runtimeClass,
			ProxyEndpoint: endpoint,
			CACert:        caCert,
		})
		if cerr != nil {
			if m.proxy != nil {
				m.proxy.Release(ctx, id)
			}
			return nil, fmt.Errorf("create sandbox: %w", cerr)
		}
		sess.PodName = handle.PodName
		sess.PVCName = handle.PVCName
	}

	if err := m.store.Create(ctx, sess); err != nil {
		// Best-effort cleanup of the just-created pod.
		_ = m.rt.Purge(context.Background(), sess.PodName, sess.PVCName)
		if m.proxy != nil {
			m.proxy.Release(ctx, id)
		}
		return nil, fmt.Errorf("persist session: %w", err)
	}

	if err := m.rt.WaitReady(ctx, sess.PodName, m.readyTO); err != nil {
		return nil, fmt.Errorf("sandbox not ready: %w", err)
	}

	sess.State = session.StateRunning
	if err := m.store.Update(ctx, sess); err != nil {
		return nil, fmt.Errorf("update session: %w", err)
	}
	return sess, nil
}

// Get returns a session by id, or session.ErrInvalidSession if unknown.
func (m *Manager) Get(ctx context.Context, id string) (*session.Session, error) {
	s, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, session.ErrInvalidSession
	}
	return s, nil
}

// Require returns a session only if it is currently running; otherwise it
// returns session.ErrInvalidSession. Tool handlers use this to validate a
// session id before operating on it.
func (m *Manager) Require(ctx context.Context, id string) (*session.Session, error) {
	s, err := m.store.Get(ctx, id)
	if err != nil || s == nil || !s.Running() {
		return nil, session.ErrInvalidSession
	}
	return s, nil
}

// Touch records activity on a session (resets its idle timer).
func (m *Manager) Touch(ctx context.Context, id string) error {
	return m.store.Touch(ctx, id)
}

// List returns all known sessions.
func (m *Manager) List(ctx context.Context) ([]*session.Session, error) {
	return m.store.List(ctx)
}

// Stop deletes the sandbox pod but retains its workspace so it can be resumed.
func (m *Manager) Stop(ctx context.Context, id string) error {
	s, err := m.store.Get(ctx, id)
	if err != nil {
		return session.ErrInvalidSession
	}
	if s.State == session.StateStopped || s.State == session.StateDead {
		return nil
	}
	if err := m.rt.Stop(ctx, s.PodName); err != nil {
		return fmt.Errorf("stop sandbox: %w", err)
	}
	s.State = session.StateStopped
	s.StoppedAt = time.Now()
	return m.store.Update(ctx, s)
}

// Resume re-creates a pod bound to a stopped session's retained workspace.
func (m *Manager) Resume(ctx context.Context, id string) (*session.Session, error) {
	s, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, session.ErrInvalidSession
	}
	if s.State == session.StateRunning {
		return s, nil
	}
	if s.State != session.StateStopped {
		return nil, fmt.Errorf("session %s is not resumable (state=%s)", id, s.State)
	}

	var endpoint string
	var caCert []byte
	if m.proxy != nil {
		ep, ca, proxyID, perr := m.proxy.Assign(ctx, id)
		if perr != nil {
			return nil, fmt.Errorf("assign proxy: %w", perr)
		}
		endpoint, caCert, s.ProxyID = ep, ca, proxyID
	}

	handle, err := m.rt.Resume(ctx, runtime.SandboxSpec{
		SessionID:     id,
		Image:         s.Image,
		RuntimeClass:  s.RuntimeClass,
		ProxyEndpoint: endpoint,
		CACert:        caCert,
	}, s.PVCName)
	if err != nil {
		return nil, fmt.Errorf("resume sandbox: %w", err)
	}
	s.PodName = handle.PodName
	s.State = session.StateCreating
	if err := m.store.Update(ctx, s); err != nil {
		return nil, err
	}
	if err := m.rt.WaitReady(ctx, s.PodName, m.readyTO); err != nil {
		return nil, fmt.Errorf("resumed sandbox not ready: %w", err)
	}
	s.State = session.StateRunning
	s.LastActivityAt = time.Now()
	return s, m.store.Update(ctx, s)
}

// Delete purges the sandbox (pod + workspace) and marks the session dead.
func (m *Manager) Delete(ctx context.Context, id string) error {
	s, err := m.store.Get(ctx, id)
	if err != nil {
		return session.ErrInvalidSession
	}
	if err := m.rt.Purge(ctx, s.PodName, s.PVCName); err != nil {
		return fmt.Errorf("purge sandbox: %w", err)
	}
	if m.proxy != nil {
		m.proxy.Release(ctx, id)
	}
	s.State = session.StateDead
	return m.store.Update(ctx, s)
}
