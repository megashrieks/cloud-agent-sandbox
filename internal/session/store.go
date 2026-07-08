// Package session provides in-memory session storage helpers.
package session

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemoryStore is a thread-safe, in-memory Store implementation.
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewMemoryStore creates an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{sessions: make(map[string]*Session)}
}

// Create stores a new session. It fails if the session is nil, has no ID, or already exists.
func (m *MemoryStore) Create(ctx context.Context, s *Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.ID == "" {
		return ErrInvalidSession
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.sessions[s.ID]; ok {
		return fmt.Errorf("create session %q: %w", s.ID, ErrInvalidSession)
	}
	m.sessions[s.ID] = cloneSession(s)
	return nil
}

// Get returns a session by ID.
func (m *MemoryStore) Get(ctx context.Context, id string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneSession(s), nil
}

// Update replaces an existing session record.
func (m *MemoryStore) Update(ctx context.Context, s *Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.ID == "" {
		return ErrInvalidSession
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.sessions[s.ID]; !ok {
		return ErrNotFound
	}
	m.sessions[s.ID] = cloneSession(s)
	return nil
}

// Delete removes a session by ID.
func (m *MemoryStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.sessions[id]; !ok {
		return ErrNotFound
	}
	delete(m.sessions, id)
	return nil
}

// List returns all sessions.
func (m *MemoryStore) List(ctx context.Context) ([]*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, cloneSession(s))
	}
	return sessions, nil
}

// ListByState returns all sessions in the given state.
func (m *MemoryStore) ListByState(ctx context.Context, state State) ([]*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	sessions := make([]*Session, 0)
	for _, s := range m.sessions {
		if s.State == state {
			sessions = append(sessions, cloneSession(s))
		}
	}
	return sessions, nil
}

// Touch updates LastActivityAt to the current time for a session.
func (m *MemoryStore) Touch(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[id]
	if !ok {
		return ErrNotFound
	}
	s.LastActivityAt = time.Now()
	return nil
}

func cloneSession(s *Session) *Session {
	if s == nil {
		return nil
	}
	clone := *s
	return &clone
}
