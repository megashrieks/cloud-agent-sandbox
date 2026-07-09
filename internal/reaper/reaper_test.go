package reaper

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/megashrieks/sandbox-orchestrator/internal/config"
	"github.com/megashrieks/sandbox-orchestrator/internal/manager"
	"github.com/megashrieks/sandbox-orchestrator/internal/runtime"
	"github.com/megashrieks/sandbox-orchestrator/internal/session"
)

type fakeRuntime struct {
	stopped []string
	purged  []purgeCall
}

type purgeCall struct {
	podName string
	pvcName string
}

func (f *fakeRuntime) Create(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxHandle, error) {
	return &runtime.SandboxHandle{PodName: "pod-" + spec.SessionID, PVCName: "pvc-" + spec.SessionID, Ready: true}, nil
}

func (f *fakeRuntime) WaitReady(ctx context.Context, podName string, timeout time.Duration) error {
	return nil
}

func (f *fakeRuntime) Stop(ctx context.Context, podName string) error {
	f.stopped = append(f.stopped, podName)
	return nil
}

func (f *fakeRuntime) Resume(ctx context.Context, spec runtime.SandboxSpec, pvcName string) (*runtime.SandboxHandle, error) {
	return &runtime.SandboxHandle{PodName: "pod-" + spec.SessionID, PVCName: pvcName, Ready: true}, nil
}

func (f *fakeRuntime) Purge(ctx context.Context, podName, pvcName string) error {
	f.purged = append(f.purged, purgeCall{podName: podName, pvcName: pvcName})
	return nil
}

func (f *fakeRuntime) Get(ctx context.Context, podName string) (*runtime.SandboxHandle, error) {
	return &runtime.SandboxHandle{PodName: podName, Ready: true}, nil
}

func (f *fakeRuntime) ListSandboxes(ctx context.Context) ([]runtime.SandboxRef, error) {
	return nil, nil
}

func (f *fakeRuntime) ListWorkspaces(ctx context.Context) ([]runtime.WorkspaceRef, error) {
	return nil, nil
}

func TestReapOnceStopsIdleRunningAndPurgesOldStopped(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 8, 20, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Lifetime.RunningTTL = time.Hour
	cfg.Lifetime.StoppedTTL = 24 * time.Hour
	cfg.Lifetime.ReapInterval = time.Minute

	store := session.NewMemoryStore()
	rt := &fakeRuntime{}
	m := manager.New(cfg, store, rt, nil, nil)
	r := New(m)
	r.now = func() time.Time { return now }

	seedSessions(t, ctx, store, []*session.Session{
		{
			ID:             "idle-running",
			State:          session.StateRunning,
			PodName:        "pod-idle-running",
			PVCName:        "pvc-idle-running",
			CreatedAt:      now.Add(-2 * time.Hour),
			LastActivityAt: now.Add(-time.Hour - time.Nanosecond),
		},
		{
			ID:             "fresh-running",
			State:          session.StateRunning,
			PodName:        "pod-fresh-running",
			PVCName:        "pvc-fresh-running",
			CreatedAt:      now.Add(-30 * time.Minute),
			LastActivityAt: now.Add(-30 * time.Minute),
		},
		{
			ID:             "old-stopped",
			State:          session.StateStopped,
			PodName:        "pod-old-stopped",
			PVCName:        "pvc-old-stopped",
			CreatedAt:      now.Add(-48 * time.Hour),
			LastActivityAt: now.Add(-47 * time.Hour),
			StoppedAt:      now.Add(-24*time.Hour - time.Nanosecond),
		},
		{
			ID:             "fresh-stopped",
			State:          session.StateStopped,
			PodName:        "pod-fresh-stopped",
			PVCName:        "pvc-fresh-stopped",
			CreatedAt:      now.Add(-25 * time.Hour),
			LastActivityAt: now.Add(-25 * time.Hour),
			StoppedAt:      now.Add(-23 * time.Hour),
		},
	})

	r.reapOnce(ctx)

	if !reflect.DeepEqual(rt.stopped, []string{"pod-idle-running"}) {
		t.Fatalf("stopped pods = %v, want [pod-idle-running]", rt.stopped)
	}
	if !reflect.DeepEqual(rt.purged, []purgeCall{{podName: "pod-old-stopped", pvcName: "pvc-old-stopped"}}) {
		t.Fatalf("purged sandboxes = %v, want old stopped sandbox", rt.purged)
	}

	assertState(t, ctx, store, "idle-running", session.StateStopped)
	assertState(t, ctx, store, "fresh-running", session.StateRunning)
	assertState(t, ctx, store, "old-stopped", session.StateDead)
	assertState(t, ctx, store, "fresh-stopped", session.StateStopped)
}

func TestExpirationHelpersRequireStrictlyOlderThanTTL(t *testing.T) {
	now := time.Date(2026, 7, 8, 20, 0, 0, 0, time.UTC)

	cfg := config.Default()
	cfg.Lifetime.RunningTTL = time.Hour
	cfg.Lifetime.MaxLifetime = 100 * time.Hour // large so only idle is exercised here
	cfg.Lifetime.StoppedTTL = 24 * time.Hour
	m := manager.New(cfg, session.NewMemoryStore(), &fakeRuntime{}, nil, nil)
	r := New(m)

	atTTL := &session.Session{State: session.StateRunning, CreatedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Hour)}
	if _, expired := r.runningExpired(atTTL, now); expired {
		t.Fatal("running session at exactly the idle TTL should not be expired")
	}
	pastTTL := &session.Session{State: session.StateRunning, CreatedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Hour - time.Nanosecond)}
	if _, expired := r.runningExpired(pastTTL, now); !expired {
		t.Fatal("running session idle longer than the TTL should be expired")
	}
	if stoppedExpired(&session.Session{State: session.StateStopped, StoppedAt: now.Add(-24 * time.Hour)}, now, 24*time.Hour) {
		t.Fatal("stopped session at exactly the TTL should not be expired")
	}
	if !stoppedExpired(&session.Session{State: session.StateStopped, StoppedAt: now.Add(-24*time.Hour - time.Nanosecond)}, now, 24*time.Hour) {
		t.Fatal("stopped session older than the TTL should be expired")
	}
}

func TestRunningExpiredMaxLifetimeAndActiveCalls(t *testing.T) {
	now := time.Date(2026, 7, 8, 20, 0, 0, 0, time.UTC)

	cfg := config.Default()
	cfg.Lifetime.RunningTTL = 10 * time.Minute
	cfg.Lifetime.MaxLifetime = time.Hour
	m := manager.New(cfg, session.NewMemoryStore(), &fakeRuntime{}, nil, nil)
	r := New(m)

	// Idle past the idle TTL with no active call -> expired (idle).
	idle := &session.Session{ID: "s-idle", State: session.StateRunning, CreatedAt: now.Add(-20 * time.Minute), LastActivityAt: now.Add(-11 * time.Minute)}
	if reason, expired := r.runningExpired(idle, now); !expired || reason != "idle" {
		t.Fatalf("idle session: got reason=%q expired=%v, want idle/true", reason, expired)
	}

	// Same idle session but with an in-flight MCP call -> NOT expired.
	m.BeginActivity(context.Background(), "s-idle")
	if _, expired := r.runningExpired(idle, now); expired {
		t.Fatal("session with an active MCP call should not be idle-expired")
	}
	m.EndActivity(context.Background(), "s-idle")

	// Exceeds max lifetime -> expired even with an active call.
	old := &session.Session{ID: "s-old", State: session.StateRunning, CreatedAt: now.Add(-2 * time.Hour), LastActivityAt: now}
	m.BeginActivity(context.Background(), "s-old")
	if reason, expired := r.runningExpired(old, now); !expired || reason != "max-lifetime" {
		t.Fatalf("over-max-lifetime session: got reason=%q expired=%v, want max-lifetime/true", reason, expired)
	}
	m.EndActivity(context.Background(), "s-old")
}

func seedSessions(t *testing.T, ctx context.Context, store session.Store, sessions []*session.Session) {
	t.Helper()
	for _, s := range sessions {
		if err := store.Create(ctx, s); err != nil {
			t.Fatalf("seed session %s: %v", s.ID, err)
		}
	}
}

func assertState(t *testing.T, ctx context.Context, store session.Store, id string, want session.State) {
	t.Helper()
	s, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get session %s: %v", id, err)
	}
	if s.State != want {
		t.Fatalf("session %s state = %s, want %s", id, s.State, want)
	}
}
