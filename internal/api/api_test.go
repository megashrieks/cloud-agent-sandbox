package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/megashrieks/cloud-agent-sandbox/internal/config"
	"github.com/megashrieks/cloud-agent-sandbox/internal/manager"
	"github.com/megashrieks/cloud-agent-sandbox/internal/runtime"
	"github.com/megashrieks/cloud-agent-sandbox/internal/session"
)

type fakeRuntime struct{}

func (fakeRuntime) Create(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxHandle, error) {
	return &runtime.SandboxHandle{PodName: "pod-" + spec.SessionID, PVCName: "pvc-" + spec.SessionID, Phase: "Running", Ready: true}, nil
}

func (fakeRuntime) WaitReady(ctx context.Context, podName string, timeout time.Duration) error {
	return nil
}

func (fakeRuntime) Stop(ctx context.Context, podName string) error {
	return nil
}

func (fakeRuntime) Resume(ctx context.Context, spec runtime.SandboxSpec, pvcName string) (*runtime.SandboxHandle, error) {
	return &runtime.SandboxHandle{PodName: "pod-" + spec.SessionID + "-resumed", PVCName: pvcName, Phase: "Running", Ready: true}, nil
}

func (fakeRuntime) Purge(ctx context.Context, podName, pvcName string) error {
	return nil
}

func (fakeRuntime) Get(ctx context.Context, podName string) (*runtime.SandboxHandle, error) {
	return &runtime.SandboxHandle{PodName: podName, Phase: "Running", Ready: true}, nil
}

func (fakeRuntime) ListSandboxes(ctx context.Context) ([]runtime.SandboxRef, error) {
	return nil, nil
}

func (fakeRuntime) ListWorkspaces(ctx context.Context) ([]runtime.WorkspaceRef, error) {
	return nil, nil
}

func TestSessionLifecycleRoutes(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.MaxRunning = 5
	m := manager.New(cfg, session.NewMemoryStore(), fakeRuntime{}, nil, nil)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes()

	created := doJSON[sessionDTO](t, h, http.MethodPost, "/sessions", []byte(`{"image":"example:test","useKata":true}`), http.StatusCreated)
	if created.ID == "" {
		t.Fatal("expected created session id")
	}
	if created.State != session.StateRunning {
		t.Fatalf("expected state %q, got %q", session.StateRunning, created.State)
	}
	if created.Image != "example:test" {
		t.Fatalf("expected custom image, got %q", created.Image)
	}
	if created.RuntimeClass != cfg.Sandbox.KataRuntimeClass {
		t.Fatalf("expected kata runtime class %q, got %q", cfg.Sandbox.KataRuntimeClass, created.RuntimeClass)
	}

	got := doJSON[sessionDTO](t, h, http.MethodGet, "/sessions/"+created.ID, nil, http.StatusOK)
	if got.ID != created.ID {
		t.Fatalf("expected id %q, got %q", created.ID, got.ID)
	}

	missing := doJSON[errorResponse](t, h, http.MethodGet, "/sessions/does-not-exist", nil, http.StatusNotFound)
	if missing.Error != "invalid session" {
		t.Fatalf("expected invalid session error, got %q", missing.Error)
	}

	stopped := doJSON[sessionDTO](t, h, http.MethodPost, "/sessions/"+created.ID+"/stop", nil, http.StatusOK)
	if stopped.State != session.StateStopped {
		t.Fatalf("expected state %q, got %q", session.StateStopped, stopped.State)
	}

	list := doJSON[[]sessionDTO](t, h, http.MethodGet, "/sessions", nil, http.StatusOK)
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("expected list to contain created session, got %+v", list)
	}

	req := httptest.NewRequest(http.MethodDelete, "/sessions/"+created.ID, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d body %q", http.StatusNoContent, rr.Code, rr.Body.String())
	}
}

func doJSON[T any](t *testing.T, h http.Handler, method, path string, body []byte, wantStatus int) T {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != wantStatus {
		t.Fatalf("%s %s: expected status %d, got %d body %q", method, path, wantStatus, rr.Code, rr.Body.String())
	}

	var out T
	if rr.Body.Len() == 0 {
		return out
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}
