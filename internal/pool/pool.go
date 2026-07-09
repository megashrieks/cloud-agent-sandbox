package pool

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/megashrieks/cloud-agent-sandbox/internal/config"
	"github.com/megashrieks/cloud-agent-sandbox/internal/manager"
	"github.com/megashrieks/cloud-agent-sandbox/internal/runtime"
)

var _ manager.Pool = (*WarmPool)(nil)

const poolIdleLabel = "sandbox/pool"

// WarmPool keeps ready, unassigned default-image sandboxes for fast startup.
type WarmPool struct {
	rt            runtime.Runtime
	cfg           config.Config
	proxyEndpoint string
	caCert        []byte
	defaultImage  string

	mu      sync.Mutex
	idle    []*runtime.SandboxHandle
	warming int
	started bool
}

// NewWarmPool builds a warm pool that creates generic sandboxes wired to the
// shared proxy endpoint and CA certificate.
func NewWarmPool(rt runtime.Runtime, cfg config.Config, proxyEndpoint string, caCert []byte) *WarmPool {
	return &WarmPool{
		rt:            rt,
		cfg:           cfg,
		proxyEndpoint: proxyEndpoint,
		caCert:        append([]byte(nil), caCert...),
		defaultImage:  cfg.Sandbox.DefaultImage,
	}
}

// Start launches background replenishment and returns immediately.
func (p *WarmPool) Start(ctx context.Context) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	p.mu.Unlock()

	go p.run(ctx)
}

// Acquire returns one ready default-image warm sandbox if available.
func (p *WarmPool) Acquire(ctx context.Context, image string) (*runtime.SandboxHandle, bool) {
	if ctx.Err() != nil || image != p.defaultImage {
		return nil, false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.idle) == 0 {
		return nil, false
	}

	handle := p.idle[0]
	copy(p.idle, p.idle[1:])
	p.idle[len(p.idle)-1] = nil
	p.idle = p.idle[:len(p.idle)-1]
	return handle, true
}

// Stats reports current pool occupancy.
func (p *WarmPool) Stats() manager.PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return manager.PoolStats{
		Idle:    len(p.idle),
		Warming: p.warming,
	}
}

func (p *WarmPool) run(ctx context.Context) {
	p.replenish(ctx)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.replenish(ctx)
		}
	}
}

func (p *WarmPool) replenish(ctx context.Context) {
	for p.reserveWarmingSlot() {
		if err := p.createOne(ctx); err != nil {
			slog.ErrorContext(ctx, "warm pool replenish failed", "err", err)
			return
		}
	}
}

func (p *WarmPool) reserveWarmingSlot() bool {
	target := p.targetIdle()
	if target <= 0 {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.idle)+p.warming >= target {
		return false
	}
	p.warming++
	return true
}

func (p *WarmPool) targetIdle() int {
	target := p.cfg.Pool.MinIdleReady
	if p.cfg.Pool.MaxRunning > 0 && target > p.cfg.Pool.MaxRunning {
		target = p.cfg.Pool.MaxRunning
	}
	return target
}

func (p *WarmPool) createOne(ctx context.Context) error {
	handle, err := p.rt.Create(ctx, runtime.SandboxSpec{
		SessionID:     "pool-" + uuid.NewString(),
		Image:         p.defaultImage,
		RuntimeClass:  p.cfg.Sandbox.RuntimeClass,
		ProxyEndpoint: p.proxyEndpoint,
		CACert:        append([]byte(nil), p.caCert...),
		RunAsRoot:     p.cfg.Sandbox.RunAsRoot,
		WritableRoot:  p.cfg.Sandbox.WritableRootFilesystem,
		Labels: map[string]string{
			poolIdleLabel: "idle",
		},
		Warm: true,
	})
	if err != nil {
		p.finishWarming(nil)
		return err
	}

	if err := p.rt.WaitReady(ctx, handle.PodName, 90*time.Second); err != nil {
		_ = p.rt.Purge(context.Background(), handle.PodName, handle.PVCName)
		p.finishWarming(nil)
		return err
	}

	p.finishWarming(handle)
	return nil
}

func (p *WarmPool) finishWarming(handle *runtime.SandboxHandle) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.warming > 0 {
		p.warming--
	}
	if handle != nil && len(p.idle) < p.targetIdle() {
		p.idle = append(p.idle, handle)
	}
}
