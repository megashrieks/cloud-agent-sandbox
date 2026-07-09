// Package api exposes the sandbox session lifecycle over HTTP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/megashrieks/cloud-agent-sandbox/internal/manager"
	"github.com/megashrieks/cloud-agent-sandbox/internal/session"
)

// Handler serves the REST API backed by a Manager.
type Handler struct {
	manager *manager.Manager
	log     *slog.Logger
}

// New constructs a Handler.
func New(m *manager.Manager, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{manager: m, log: log}
}

// Routes returns the HTTP routes for the sandbox session lifecycle API.
func (h *Handler) Routes() http.Handler {
	return h.withMiddleware(http.HandlerFunc(h.route))
}

type createSessionRequest struct {
	Image   string `json:"image,omitempty"`
	UseKata bool   `json:"useKata,omitempty"`
}

type sessionDTO struct {
	ID             string        `json:"id"`
	State          session.State `json:"state"`
	Image          string        `json:"image"`
	RuntimeClass   string        `json:"runtimeClass"`
	CreatedAt      time.Time     `json:"createdAt"`
	LastActivityAt time.Time     `json:"lastActivityAt"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}

	if path == "/sessions" {
		switch r.Method {
		case http.MethodPost:
			h.createSession(w, r)
		case http.MethodGet:
			h.listSessions(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	const prefix = "/sessions/"
	if !strings.HasPrefix(path, prefix) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}

	rest := strings.TrimPrefix(path, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	id := parts[0]

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.getSession(w, r, id)
		case http.MethodDelete:
			h.deleteSession(w, r, id)
		default:
			w.Header().Set("Allow", "GET, DELETE")
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	if len(parts) == 2 && r.Method == http.MethodPost {
		switch parts[1] {
		case "stop":
			h.stopSession(w, r, id)
		case "resume":
			h.resumeSession(w, r, id)
		default:
			writeErr(w, http.StatusNotFound, "not found")
		}
		return
	}

	writeErr(w, http.StatusNotFound, "not found")
}

func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if r.Body != nil {
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			writeErr(w, http.StatusBadRequest, "invalid json")
			return
		}
	}

	s, err := h.manager.Create(r.Context(), session.CreateOptions{Image: req.Image, UseKata: req.UseKata})
	if err != nil {
		h.writeManagerErr(w, "create session", err)
		return
	}
	writeJSON(w, http.StatusCreated, toSessionDTO(s))
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := h.manager.List(r.Context())
	if err != nil {
		h.writeManagerErr(w, "list sessions", err)
		return
	}

	out := make([]sessionDTO, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, toSessionDTO(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getSession(w http.ResponseWriter, r *http.Request, id string) {
	s, err := h.manager.Get(r.Context(), id)
	if err != nil {
		h.writeManagerErr(w, "get session", err)
		return
	}
	writeJSON(w, http.StatusOK, toSessionDTO(s))
}

func (h *Handler) stopSession(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.manager.Stop(r.Context(), id); err != nil {
		h.writeManagerErr(w, "stop session", err)
		return
	}
	s, err := h.manager.Get(r.Context(), id)
	if err != nil {
		h.writeManagerErr(w, "get stopped session", err)
		return
	}
	writeJSON(w, http.StatusOK, toSessionDTO(s))
}

func (h *Handler) resumeSession(w http.ResponseWriter, r *http.Request, id string) {
	s, err := h.manager.Resume(r.Context(), id)
	if err != nil {
		h.writeManagerErr(w, "resume session", err)
		return
	}
	writeJSON(w, http.StatusOK, toSessionDTO(s))
}

func (h *Handler) deleteSession(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.manager.Delete(r.Context(), id); err != nil {
		h.writeManagerErr(w, "delete session", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) writeManagerErr(w http.ResponseWriter, operation string, err error) {
	if errors.Is(err, session.ErrInvalidSession) {
		writeErr(w, http.StatusNotFound, "invalid session")
		return
	}
	if strings.Contains(strings.ToLower(err.Error()), "max running") {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	h.log.Error(operation, "error", err)
	writeErr(w, http.StatusInternalServerError, "internal server error")
}

func (h *Handler) withMiddleware(next http.Handler) http.Handler {
	return h.recoverer(h.requestLogger(h.requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	}))))
}

func (h *Handler) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = time.Now().UTC().Format("20060102150405.000000000")
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		h.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rw.status, "duration", time.Since(start))
	})
}

func (h *Handler) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				h.log.Error("panic serving request", "panic", rec, "stack", string(debug.Stack()))
				writeErr(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func toSessionDTO(s *session.Session) sessionDTO {
	if s == nil {
		return sessionDTO{}
	}
	return sessionDTO{
		ID:             s.ID,
		State:          s.State,
		Image:          s.Image,
		RuntimeClass:   s.RuntimeClass,
		CreatedAt:      s.CreatedAt,
		LastActivityAt: s.LastActivityAt,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status == http.StatusNoContent {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
