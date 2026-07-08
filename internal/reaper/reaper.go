// Package reaper enforces sandbox lifetime limits.
package reaper

import (
	"context"
	"log/slog"
	"time"

	"github.com/megashrieks/sandbox-orchestrator/internal/manager"
	"github.com/megashrieks/sandbox-orchestrator/internal/session"
)

// Reaper periodically stops idle running sandboxes and purges old stopped ones.
type Reaper struct {
	m            *manager.Manager
	runningTTL   time.Duration
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

	running, err := r.m.Store().ListByState(ctx, session.StateRunning)
	if err != nil {
		slog.Error("list running sessions for reaping", "error", err)
	} else {
		for _, s := range running {
			if !runningExpired(s, now, r.runningTTL) {
				continue
			}
			if err := r.m.Stop(ctx, s.ID); err != nil {
				slog.Error("stop expired running sandbox", "session_id", s.ID, "error", err)
				continue
			}
			slog.Info("stopped expired running sandbox", "session_id", s.ID, "last_activity_at", s.LastActivityAt, "running_ttl", r.runningTTL)
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

func runningExpired(s *session.Session, now time.Time, ttl time.Duration) bool {
	return s != nil && s.State == session.StateRunning && now.Sub(s.LastActivityAt) > ttl
}

func stoppedExpired(s *session.Session, now time.Time, ttl time.Duration) bool {
	return s != nil && s.State == session.StateStopped && now.Sub(s.StoppedAt) > ttl
}
