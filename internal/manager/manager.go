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
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/megashrieks/cloud-agent-sandbox/internal/config"
	"github.com/megashrieks/cloud-agent-sandbox/internal/runtime"
	"github.com/megashrieks/cloud-agent-sandbox/internal/session"
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

// ProxyScaler ensures MITM proxy capacity is available on demand. Implemented
// by internal/proxy's Autoscaler. Optional: when nil the orchestrator assumes a
// statically-provisioned proxy.
type ProxyScaler interface {
	// EnsureReady scales the proxy up if needed and blocks until at least one
	// proxy replica is Ready, so a sandbox about to start can reach it.
	EnsureReady(ctx context.Context) error
}

// Manager owns sandbox session lifecycle.
type Manager struct {
	cfg     config.Config
	store   session.Store
	rt      runtime.Runtime
	pool    Pool
	proxy   ProxyAssigner
	scaler  ProxyScaler
	readyTO time.Duration

	// activeMu guards active, which counts in-flight MCP calls per session so
	// the reaper never treats a session with a running tool call as idle.
	activeMu sync.Mutex
	active   map[string]int

	// keyedMu guards keyed, a per-session-id lock set that serialises
	// get-or-create so concurrent requests carrying the same X-Session-Id
	// (eager-load plus the first tool call) cannot create duplicate sandboxes.
	keyedMu sync.Mutex
	keyed   map[string]*sync.Mutex
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
		active:  make(map[string]int),
		keyed:   make(map[string]*sync.Mutex),
	}
}

// lockSession returns an unlock func for the per-id keyed lock, creating the
// lock on first use. Callers MUST defer the returned func.
func (m *Manager) lockSession(id string) func() {
	m.keyedMu.Lock()
	mu, ok := m.keyed[id]
	if !ok {
		mu = &sync.Mutex{}
		m.keyed[id] = mu
	}
	m.keyedMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// Store exposes the underlying session store (read-only helpers for callers
// like the reaper).
func (m *Manager) Store() session.Store { return m.store }

// Runtime exposes the underlying runtime (used by the reaper to stop/purge).
func (m *Manager) Runtime() runtime.Runtime { return m.rt }

// Config exposes the effective configuration.
func (m *Manager) Config() config.Config { return m.cfg }

// SetProxyScaler wires an optional on-demand proxy autoscaler. When set,
// Create asks it to ensure proxy capacity before starting a sandbox.
func (m *Manager) SetProxyScaler(s ProxyScaler) { m.scaler = s }

// resolveImage maps friendly aliases to concrete image references. An empty
// string or the alias "default" resolves to the configured DefaultImage, so
// callers (e.g. LLM tool calls) that pass image:"default" don't end up trying
// to pull docker.io/library/default:latest, which does not exist and would
// leave the pod in ImagePullBackOff until the ready deadline is exceeded.
func (m *Manager) resolveImage(image string) string {
	switch strings.TrimSpace(image) {
	case "", "default", "sandbox-default":
		return m.cfg.Sandbox.DefaultImage
	default:
		return image
	}
}

// resolveBool returns the pointer's value when set, else the default. Used to
// let per-session CreateOptions override the configured posture (writable-root,
// run-as-root) while defaulting to the orchestrator's configuration.
func resolveBool(override *bool, def bool) bool {
	if override != nil {
		return *override
	}
	return def
}

// SweepOrphans reconciles the platform against the (in-memory) session store
// and removes sandboxes/workspaces the store no longer knows about. This is the
// self-healing counterpart to the reaper: because the store is in-memory and
// wiped on every orchestrator restart, any pod/PVC created before a restart is
// no longer tracked and would otherwise live forever. Warm-pool resources and
// resources still tracked by the store are left alone; tracked ones are handled
// by the normal reaper path.
//
// An orphan pod older than MaxLifetime is purged (pod + PVC). An orphan PVC with
// no backing pod, older than StoppedTTL, is deleted.
func (m *Manager) SweepOrphans(ctx context.Context) {
	now := time.Now()

	known := make(map[string]bool)
	if all, err := m.store.List(ctx); err != nil {
		slog.Error("sweep orphans: list sessions", "error", err)
		return
	} else {
		for _, s := range all {
			known[s.ID] = true
		}
	}

	pods, err := m.rt.ListSandboxes(ctx)
	if err != nil {
		slog.Error("sweep orphans: list sandboxes", "error", err)
		return
	}

	livePodSession := make(map[string]bool, len(pods))
	maxLife := m.cfg.Lifetime.MaxLifetime
	for _, p := range pods {
		livePodSession[p.SessionID] = true
		if p.Pool || known[p.SessionID] {
			continue
		}
		// Grace: never touch a pod younger than the max-lifetime cap, so we
		// cannot race a just-created session that isn't in the store yet.
		if maxLife > 0 && now.Sub(p.CreatedAt) <= maxLife {
			continue
		}
		if err := m.rt.Purge(ctx, p.PodName, p.PVCName); err != nil {
			slog.Error("sweep orphans: purge orphan pod", "pod", p.PodName, "error", err)
			continue
		}
		if m.proxy != nil && p.SessionID != "" {
			m.proxy.Release(ctx, p.SessionID)
		}
		slog.Info("purged orphan sandbox pod", "pod", p.PodName, "session_id", p.SessionID,
			"created_at", p.CreatedAt, "age", now.Sub(p.CreatedAt).String())
	}

	workspaces, err := m.rt.ListWorkspaces(ctx)
	if err != nil {
		slog.Error("sweep orphans: list workspaces", "error", err)
		return
	}
	stoppedTTL := m.cfg.Lifetime.StoppedTTL
	for _, w := range workspaces {
		if w.Pool || known[w.SessionID] || livePodSession[w.SessionID] {
			continue
		}
		if stoppedTTL > 0 && now.Sub(w.CreatedAt) <= stoppedTTL {
			continue
		}
		if err := m.rt.Purge(ctx, w.PodName, w.PVCName); err != nil {
			slog.Error("sweep orphans: delete orphan pvc", "pvc", w.PVCName, "error", err)
			continue
		}
		slog.Info("purged orphan workspace pvc", "pvc", w.PVCName, "session_id", w.SessionID,
			"created_at", w.CreatedAt, "age", now.Sub(w.CreatedAt).String())
	}
}

// Create provisions a new sandbox session and returns it once the sandbox is
// ready for exec.
func (m *Manager) Create(ctx context.Context, opts session.CreateOptions) (*session.Session, error) {
	image := m.resolveImage(opts.Image)
	writableRoot := resolveBool(opts.WritableRoot, m.cfg.Sandbox.WritableRootFilesystem)
	runAsRoot := resolveBool(opts.RunAsRoot, m.cfg.Sandbox.RunAsRoot)

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

	// A caller-supplied X-Session-Id (opts.Ref) is canonicalised into a
	// stable, Kubernetes-safe id so the same header always maps to the same
	// sandbox. REST creates without a ref get a fresh random id.
	id := session.NewID()
	if opts.Ref != "" {
		id = session.CanonicalID(opts.Ref)
	}
	runtimeClass := m.cfg.Sandbox.RuntimeClass
	if opts.UseKata && m.cfg.Sandbox.KataRuntimeClass != "" {
		runtimeClass = m.cfg.Sandbox.KataRuntimeClass
	}

	sess := &session.Session{
		ID:             id,
		Ref:            opts.Ref,
		OrgID:          opts.OrgID,
		UserID:         opts.UserID,
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

		// Ensure proxy capacity is up and Ready before the sandbox starts
		// sending egress (scale-from-zero on demand).
		if m.scaler != nil {
			if serr := m.scaler.EnsureReady(ctx); serr != nil {
				m.proxy.Release(ctx, id)
				return nil, fmt.Errorf("ensure proxy ready: %w", serr)
			}
		}
	}

	// Try the warm pool first (default image only; warm pods are generic and
	// already wired to the shared proxy Service + CA). Only reuse a warm pod
	// when the requested root/writable posture matches the pool's (which is
	// built from the configured defaults). Otherwise cold-create so the pod's
	// security context is correct.
	poolEligible := image == m.cfg.Sandbox.DefaultImage &&
		writableRoot == m.cfg.Sandbox.WritableRootFilesystem &&
		runAsRoot == m.cfg.Sandbox.RunAsRoot
	if m.pool != nil && poolEligible {
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
			RunAsRoot:     runAsRoot,
			WritableRoot:  writableRoot,
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
		m.cleanupFailedCreate(sess)
		return nil, fmt.Errorf("sandbox not ready: %w", err)
	}

	sess.State = session.StateRunning
	if err := m.store.Update(ctx, sess); err != nil {
		m.cleanupFailedCreate(sess)
		return nil, fmt.Errorf("update session: %w", err)
	}
	return sess, nil
}

// cleanupFailedCreate tears down the pod/PVC and proxy assignment for a session
// that was persisted but never became Ready, and marks it Dead so it stops
// counting toward the MaxRunning cap. Uses a background context so cleanup still
// runs when the caller's context has been cancelled or timed out.
func (m *Manager) cleanupFailedCreate(sess *session.Session) {
	ctx := context.Background()
	_ = m.rt.Purge(ctx, sess.PodName, sess.PVCName)
	if m.proxy != nil {
		m.proxy.Release(ctx, sess.ID)
	}
	sess.State = session.StateDead
	_ = m.store.Update(ctx, sess)
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

// EnsureSession is the get-or-create entry point for the header-driven MCP
// surface. It maps the caller-supplied reference (X-Session-Id) to a stable
// canonical id and guarantees a running sandbox for it:
//
//   - no sandbox yet            => create one (eager load),
//   - already running           => return it (optionally recreate on image
//     change when recreateOnImageChange is set),
//   - stopped                   => resume it,
//   - stuck (creating/dead)     => purge and recreate.
//
// opts.Ref is mandatory. When recreateOnImageChange is true and the caller
// requests an explicit image that differs from the running sandbox's, the old
// sandbox is purged and replaced (create_sandbox semantics). Concurrent calls
// for the same reference are serialised on a per-id lock so eager load and the
// first tool call cannot race into two sandboxes.
func (m *Manager) EnsureSession(ctx context.Context, opts session.CreateOptions, recreateOnImageChange bool) (*session.Session, error) {
	if opts.Ref == "" {
		return nil, session.ErrInvalidSession
	}
	id := session.CanonicalID(opts.Ref)

	unlock := m.lockSession(id)
	defer unlock()

	existing, err := m.store.Get(ctx, id)
	if err == nil && existing != nil {
		desiredImage := m.resolveImage(opts.Image)
		imageChange := recreateOnImageChange && opts.Image != "" && existing.Image != desiredImage

		switch {
		case imageChange:
			// Fall through to recreate with the newly requested image.
			m.purgeAndForget(context.WithoutCancel(ctx), existing)
		case existing.Running():
			return existing, nil
		case existing.State == session.StateStopped:
			return m.Resume(ctx, id)
		default:
			// Stuck in creating/dead: self-heal by purging and recreating.
			m.purgeAndForget(context.WithoutCancel(ctx), existing)
		}
	}

	return m.Create(ctx, opts)
}

// Clear purges the sandbox (pod + workspace) for a caller reference and forgets
// it entirely, so a later request with the same X-Session-Id starts fresh.
func (m *Manager) Clear(ctx context.Context, ref string) error {
	if ref == "" {
		return session.ErrInvalidSession
	}
	id := session.CanonicalID(ref)

	unlock := m.lockSession(id)
	defer unlock()

	s, err := m.store.Get(ctx, id)
	if err != nil || s == nil {
		return session.ErrInvalidSession
	}
	m.purgeAndForget(ctx, s)
	return nil
}

// purgeAndForget tears down the sandbox and removes its record from the store
// so the canonical id is free to be recreated. Unlike Delete, it does not leave
// a tombstone (which would block re-creation under the same id).
func (m *Manager) purgeAndForget(ctx context.Context, s *session.Session) {
	_ = m.rt.Purge(ctx, s.PodName, s.PVCName)
	if m.proxy != nil {
		m.proxy.Release(ctx, s.ID)
	}
	_ = m.store.Delete(ctx, s.ID)
}

// Touch records activity on a session (resets its idle timer).
func (m *Manager) Touch(ctx context.Context, id string) error {
	return m.store.Touch(ctx, id)
}

// BeginActivity marks the start of an in-flight MCP call for a session and
// refreshes its idle timer. Pair every call with EndActivity (defer).
func (m *Manager) BeginActivity(ctx context.Context, id string) {
	m.activeMu.Lock()
	m.active[id]++
	m.activeMu.Unlock()
	_ = m.store.Touch(ctx, id)
}

// EndActivity marks the end of an in-flight MCP call for a session and
// refreshes its idle timer so the idle countdown starts from when the call
// finished (not when it started).
func (m *Manager) EndActivity(ctx context.Context, id string) {
	m.activeMu.Lock()
	if n := m.active[id]; n <= 1 {
		delete(m.active, id)
	} else {
		m.active[id] = n - 1
	}
	m.activeMu.Unlock()
	_ = m.store.Touch(ctx, id)
}

// HasActiveCalls reports whether a session currently has an in-flight MCP call.
// The reaper uses this so an idle-timeout never fires mid-call.
func (m *Manager) HasActiveCalls(id string) bool {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	return m.active[id] > 0
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
		m.cleanupFailedResume(s)
		return nil, err
	}
	if err := m.rt.WaitReady(ctx, s.PodName, m.readyTO); err != nil {
		m.cleanupFailedResume(s)
		return nil, fmt.Errorf("resumed sandbox not ready: %w", err)
	}
	s.State = session.StateRunning
	s.LastActivityAt = time.Now()
	return s, m.store.Update(ctx, s)
}

// cleanupFailedResume deletes the freshly-created pod (retaining the workspace
// PVC) and releases the proxy for a resume that never became Ready, restoring
// the session to StateStopped so it can be resumed again later.
func (m *Manager) cleanupFailedResume(s *session.Session) {
	ctx := context.Background()
	_ = m.rt.Stop(ctx, s.PodName)
	if m.proxy != nil {
		m.proxy.Release(ctx, s.ID)
	}
	s.PodName = ""
	s.State = session.StateStopped
	_ = m.store.Update(ctx, s)
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
