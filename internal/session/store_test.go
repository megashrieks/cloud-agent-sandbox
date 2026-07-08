package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStoreCreate(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	createdAt := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	s := &Session{ID: "session-1", State: StateCreating, Image: "alpine", CreatedAt: createdAt, LastActivityAt: createdAt}

	if err := store.Create(ctx, s); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	s.State = StateDead
	got, err := store.Get(ctx, "session-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != StateCreating {
		t.Fatalf("Create() stored caller-owned pointer; got state %q", got.State)
	}
}

func TestMemoryStoreCreateDuplicate(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	s := &Session{ID: "session-1", State: StateCreating}

	if err := store.Create(ctx, s); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.Create(ctx, s); err == nil {
		t.Fatal("Create() duplicate error = nil")
	}
}

func TestMemoryStoreGet(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	want := &Session{ID: "session-1", State: StateRunning, Image: "image", RuntimeClass: "kata", PodName: "pod", PVCName: "pvc", ProxyID: "proxy", FromPool: true}
	if err := store.Create(ctx, want); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := store.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != want.ID || got.State != want.State || got.Image != want.Image || got.RuntimeClass != want.RuntimeClass || got.PodName != want.PodName || got.PVCName != want.PVCName || got.ProxyID != want.ProxyID || got.FromPool != want.FromPool {
		t.Fatalf("Get() = %+v, want %+v", got, want)
	}

	got.State = StateDead
	again, err := store.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("Get() again error = %v", err)
	}
	if again.State != StateRunning {
		t.Fatalf("Get() returned internal pointer; state = %q", again.State)
	}
}

func TestMemoryStoreGetNotFound(t *testing.T) {
	store := NewMemoryStore()
	_, err := store.Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreUpdate(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Create(ctx, &Session{ID: "session-1", State: StateCreating}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	updated := &Session{ID: "session-1", State: StateRunning, Image: "updated"}
	if err := store.Update(ctx, updated); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	updated.Image = "mutated"

	got, err := store.Get(ctx, "session-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != StateRunning || got.Image != "updated" {
		t.Fatalf("updated session = %+v", got)
	}

	if err := store.Update(ctx, &Session{ID: "missing", State: StateRunning}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update() missing error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreDelete(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Create(ctx, &Session{ID: "session-1", State: StateCreating}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.Delete(ctx, "session-1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, "session-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() after Delete() error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "session-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete() missing error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreList(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	for _, s := range []*Session{{ID: "session-1", State: StateRunning}, {ID: "session-2", State: StateStopped}} {
		if err := store.Create(ctx, s); err != nil {
			t.Fatalf("Create(%q) error = %v", s.ID, err)
		}
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List() len = %d, want 2", len(got))
	}

	got[0].State = StateDead
	stored, err := store.Get(ctx, got[0].ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.State == StateDead {
		t.Fatal("List() returned internal pointers")
	}
}

func TestMemoryStoreListByState(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	sessions := []*Session{
		{ID: "session-1", State: StateRunning},
		{ID: "session-2", State: StateStopped},
		{ID: "session-3", State: StateRunning},
	}
	for _, s := range sessions {
		if err := store.Create(ctx, s); err != nil {
			t.Fatalf("Create(%q) error = %v", s.ID, err)
		}
	}

	got, err := store.ListByState(ctx, StateRunning)
	if err != nil {
		t.Fatalf("ListByState() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByState() len = %d, want 2", len(got))
	}
	for _, s := range got {
		if s.State != StateRunning {
			t.Fatalf("ListByState() included state %q", s.State)
		}
	}

	got[0].State = StateDead
	stored, err := store.Get(ctx, got[0].ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.State == StateDead {
		t.Fatal("ListByState() returned internal pointers")
	}
}

func TestMemoryStoreTouch(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	oldActivity := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	if err := store.Create(ctx, &Session{ID: "session-1", State: StateRunning, LastActivityAt: oldActivity}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	before := time.Now()
	if err := store.Touch(ctx, "session-1"); err != nil {
		t.Fatalf("Touch() error = %v", err)
	}
	after := time.Now()

	got, err := store.Get(ctx, "session-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.LastActivityAt.After(oldActivity) {
		t.Fatalf("LastActivityAt = %v, want after %v", got.LastActivityAt, oldActivity)
	}
	if got.LastActivityAt.Before(before) || got.LastActivityAt.After(after) {
		t.Fatalf("LastActivityAt = %v, want between %v and %v", got.LastActivityAt, before, after)
	}

	if err := store.Touch(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Touch() missing error = %v, want ErrNotFound", err)
	}
}

func TestNewID(t *testing.T) {
	const prefix = "sbx-"
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := NewID()
		if len(id) != len(prefix)+8 || id[:len(prefix)] != prefix {
			t.Fatalf("NewID() = %q, want prefix %q and 8 hex chars", id, prefix)
		}
		if seen[id] {
			t.Fatalf("NewID() returned duplicate %q", id)
		}
		seen[id] = true
	}
}
