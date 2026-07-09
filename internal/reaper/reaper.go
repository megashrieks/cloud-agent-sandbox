// Package reaper enforces sandbox lifetime limits.
package reaper

import (
	"context"
	"log/slog"
	"time"

	"github.com/megashrieks/cloud-agent-sandbox/internal/manager"
	"github.com/megashrieks/cloud-agent-sandbox/internal/session"
)

// Reaper periodically stops idle running sandboxes and purges old stopped ones.
type Reaper struct {
	m            *manager.Manager
	runningTTL   time.Duration
	maxLifetime  time.Duration
	stoppedTTL   time.Duration
	reapInterval time.Duration
	now          func() time.Time
}

// New builds a Reaper from the Manager's lifetime configuration.
func New(m *manager.Manager) *Reaper {
	lifetime := m.Config().Lifetime
	return &Reaper{
		m:            m,
		runningTTL:   lifetime.RunningTTL,
		maxLifetime:  lifetime.MaxLifetime,
		stoppedTTL:   lifetime.StoppedTTL,
		reapInterval: lifetime.ReapInterval,
		now:          time.Now,
	}
}

// Run scans for expired sandboxes on each configured interval until ctx is cancelled.
func (r *Reaper) Run(ctx context.Context) {
	interval := r.reapInterval
	if interval <= 0 {
		interval = time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reapOnce(ctx)
		}
	}
}

func (r *Reaper) reapOnce(ctx context.Context) {
	now := r.now()

	// Reconcile platform-level orphans first: pods/PVCs that outlived the
	// in-memory session store (e.g. created before an orchestrator restart) and
	// are therefore invisible to the store-driven passes below.
	r.m.SweepOrphans(ctx)

	running, err := r.m.Store().ListByState(ctx, session.StateRunning)
	if err != nil {
		slog.Error("list running sessions for reaping", "error", err)
	} else {
		for _, s := range running {
			reason, expired := r.runningExpired(s, now)
			if !expired {
				continue
			}
			if err := r.m.Stop(ctx, s.ID); err != nil {
				slog.Error("stop expired running sandbox", "session_id", s.ID, "error", err)
				continue
			}
			slog.Info("stopped expired running sandbox", "session_id", s.ID, "reason", reason,
				"last_activity_at", s.LastActivityAt, "created_at", s.CreatedAt,
				"idle_ttl", r.runningTTL, "max_lifetime", r.maxLifetime)
		}
	}

	stopped, err := r.m.Store().ListByState(ctx, session.StateStopped)
	if err != nil {
		slog.Error("list stopped sessions for reaping", "error", err)
		return
	}
	for _, s := range stopped {
		if !stoppedExpired(s, now, r.stoppedTTL) {
			continue
		}
		if err := r.m.Delete(ctx, s.ID); err != nil {
			slog.Error("purge expired stopped sandbox", "session_id", s.ID, "error", err)
			continue
		}
		slog.Info("purged expired stopped sandbox", "session_id", s.ID, "stopped_at", s.StoppedAt, "stopped_ttl", r.stoppedTTL)
	}
}

// runningExpired decides whether a running sandbox should be stopped. It
// returns a human-readable reason and true when either the hard max-lifetime
// cap is exceeded, or the sandbox has been idle past runningTTL with no
// in-flight MCP call. An active MCP call suppresses the idle timeout but never
// the max-lifetime cap.
func (r *Reaper) runningExpired(s *session.Session, now time.Time) (string, bool) {
	if s == nil || s.State != session.StateRunning {
		return "", false
	}
	if r.maxLifetime > 0 && now.Sub(s.CreatedAt) > r.maxLifetime {
		return "max-lifetime", true
	}
	if now.Sub(s.LastActivityAt) > r.runningTTL && !r.m.HasActiveCalls(s.ID) {
		return "idle", true
	}
	return "", false
}

func stoppedExpired(s *session.Session, now time.Time, ttl time.Duration) bool {
	return s != nil && s.State == session.StateStopped && now.Sub(s.StoppedAt) > ttl
}
