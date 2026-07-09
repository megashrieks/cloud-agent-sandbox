package pool

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/megashrieks/sandbox-orchestrator/internal/config"
	"github.com/megashrieks/sandbox-orchestrator/internal/runtime"
)

type fakeRuntime struct {
	mu          sync.Mutex
	createErr   error
	waitErr     error
	createdSpec []runtime.SandboxSpec
}

func (f *fakeRuntime) Create(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.createErr != nil {
		return nil, f.createErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdSpec = append(f.createdSpec, spec)
	n := len(f.createdSpec)
	return &runtime.SandboxHandle{
		PodName: fmt.Sprintf("pod-%d", n),
		PVCName: fmt.Sprintf("pvc-%d", n),
		Ready:   true,
	}, nil
}

func (f *fakeRuntime) WaitReady(ctx context.Context, podName string, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.waitErr
}

func (f *fakeRuntime) Stop(ctx context.Context, podName string) error { return nil }

func (f *fakeRuntime) Resume(ctx context.Context, spec runtime.SandboxSpec, pvcName string) (*runtime.SandboxHandle, error) {
	return &runtime.SandboxHandle{PodName: "resumed", PVCName: pvcName, Ready: true}, nil
}

func (f *fakeRuntime) Purge(ctx context.Context, podName, pvcName string) error { return nil }

func (f *fakeRuntime) Get(ctx context.Context, podName string) (*runtime.SandboxHandle, error) {
	return &runtime.SandboxHandle{PodName: podName, Ready: true}, nil
}

func (f *fakeRuntime) ListSandboxes(ctx context.Context) ([]runtime.SandboxRef, error) {
	return nil, nil
}

func (f *fakeRuntime) ListWorkspaces(ctx context.Context) ([]runtime.WorkspaceRef, error) {
	return nil, nil
}

func (f *fakeRuntime) created() []runtime.SandboxSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.SandboxSpec(nil), f.createdSpec...)
}

func testConfig() config.Config {
	cfg := config.Default()
	cfg.Sandbox.DefaultImage = "default:latest"
	cfg.Pool.MinIdleReady = 0
	cfg.Pool.MaxRunning = 10
	return cfg
}

func TestAcquireEmptyPoolReturnsFalse(t *testing.T) {
	cfg := testConfig()
	p := NewWarmPool(&fakeRuntime{}, cfg, "proxy:8080", []byte("ca"))

	if handle, ok := p.Acquire(context.Background(), cfg.Sandbox.DefaultImage); ok || handle != nil {
		t.Fatalf("Acquire() = (%v, %v), want nil,false", handle, ok)
	}
}

func TestAcquireReturnsSeededIdleAndDecrements(t *testing.T) {
	cfg := testConfig()
	p := NewWarmPool(&fakeRuntime{}, cfg, "proxy:8080", []byte("ca"))
	p.idle = []*runtime.SandboxHandle{
		{PodName: "pod-1", PVCName: "pvc-1", Ready: true},
		{PodName: "pod-2", PVCName: "pvc-2", Ready: true},
	}

	handle, ok := p.Acquire(context.Background(), cfg.Sandbox.DefaultImage)
	if !ok {
		t.Fatal("Acquire() ok = false, want true")
	}
	if handle.PodName != "pod-1" {
		t.Fatalf("Acquire() pod = %q, want pod-1", handle.PodName)
	}

	stats := p.Stats()
	if stats.Idle != 1 {
		t.Fatalf("Stats().Idle = %d, want 1", stats.Idle)
	}
}

func TestAcquireNonDefaultImageReturnsFalse(t *testing.T) {
	cfg := testConfig()
	p := NewWarmPool(&fakeRuntime{}, cfg, "proxy:8080", []byte("ca"))
	p.idle = []*runtime.SandboxHandle{{PodName: "pod-1", PVCName: "pvc-1", Ready: true}}

	handle, ok := p.Acquire(context.Background(), "custom:latest")
	if ok || handle != nil {
		t.Fatalf("Acquire() = (%v, %v), want nil,false", handle, ok)
	}
	if got := p.Stats().Idle; got != 1 {
		t.Fatalf("Stats().Idle after failed acquire = %d, want 1", got)
	}
}

func TestStatsReflectsCounts(t *testing.T) {
	cfg := testConfig()
	p := NewWarmPool(&fakeRuntime{}, cfg, "proxy:8080", []byte("ca"))
	p.idle = []*runtime.SandboxHandle{{PodName: "pod-1"}, {PodName: "pod-2"}}
	p.warming = 3

	stats := p.Stats()
	if stats.Idle != 2 || stats.Warming != 3 {
		t.Fatalf("Stats() = %+v, want Idle=2 Warming=3", stats)
	}
}

func TestStartReplenishesIdlePool(t *testing.T) {
	cfg := testConfig()
	cfg.Pool.MinIdleReady = 2
	rt := &fakeRuntime{}
	p := NewWarmPool(rt, cfg, "proxy:8080", []byte("ca"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Idle == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	stats := p.Stats()
	if stats.Idle != 2 || stats.Warming != 0 {
		t.Fatalf("Stats() after replenish = %+v, want Idle=2 Warming=0", stats)
	}

	specs := rt.created()
	if len(specs) != 2 {
		t.Fatalf("created specs = %d, want 2", len(specs))
	}
	for _, spec := range specs {
		if !spec.Warm {
			t.Fatalf("spec.Warm = false, want true")
		}
		if spec.Image != cfg.Sandbox.DefaultImage {
			t.Fatalf("spec.Image = %q, want %q", spec.Image, cfg.Sandbox.DefaultImage)
		}
		if spec.ProxyEndpoint != "proxy:8080" {
			t.Fatalf("spec.ProxyEndpoint = %q, want proxy:8080", spec.ProxyEndpoint)
		}
		if string(spec.CACert) != "ca" {
			t.Fatalf("spec.CACert = %q, want ca", string(spec.CACert))
		}
		if spec.Labels[poolIdleLabel] != "idle" {
			t.Fatalf("spec.Labels[%q] = %q, want idle", poolIdleLabel, spec.Labels[poolIdleLabel])
		}
		if spec.SessionID == "" {
			t.Fatal("spec.SessionID is empty")
		}
	}
}
