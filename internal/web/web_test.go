package web_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oliverbestmann/scantool/internal/session"
	"github.com/oliverbestmann/scantool/internal/store"
	"github.com/oliverbestmann/scantool/internal/web"
)

type fakeReader struct {
	sessions []store.Session
	actions  []store.Action
	err      error
}

func (f *fakeReader) RecentSessions(int) ([]store.Session, error) { return f.sessions, f.err }
func (f *fakeReader) RecentActions(int) ([]store.Action, error)   { return f.actions, f.err }

func newServer(t *testing.T, opts web.Options) http.Handler {
	t.Helper()

	if opts.State == nil {
		opts.State = func() session.State { return session.State{Status: session.StatusIdle} }
	}
	if opts.Reader == nil {
		opts.Reader = &fakeReader{}
	}
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	server, err := web.New(opts)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return server.Handler()
}

func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func post(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	return rec
}

func TestIndexRenders(t *testing.T) {
	handler := newServer(t, web.Options{Submit: func(session.Action) error { return nil }})

	rec := get(t, handler, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()
	for _, want := range []string{"<title>scantool</title>", `data-key="a"`, `data-key="b"`, `data-key="c"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func TestIndexHidesControlsWhenDisabled(t *testing.T) {
	handler := newServer(t, web.Options{})

	body := get(t, handler, "/").Body.String()
	if strings.Contains(body, `data-key="a"`) {
		t.Error("the page offers buttons although web control is disabled")
	}
}

func TestStateEndpoint(t *testing.T) {
	started := time.Date(2026, 9, 8, 11, 44, 0, 0, time.UTC)
	state := session.State{
		Status:           session.StatusScanning,
		SessionActive:    true,
		SessionID:        7,
		SessionStartedAt: started,
		Pages:            2,
	}

	reader := &fakeReader{
		sessions: []store.Session{{ID: 7, StartedAt: started, Pages: 2, Status: store.StatusActive}},
		actions:  []store.Action{{ID: 3, Time: started, Kind: store.KindPageScanned, SessionID: 7, Page: 2}},
	}

	handler := newServer(t, web.Options{
		State:  func() session.State { return state },
		Reader: reader,
		Submit: func(session.Action) error { return nil },
	})

	rec := get(t, handler, "/api/state")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type = %q", ct)
	}

	var got struct {
		State          session.State   `json:"state"`
		Sessions       []store.Session `json:"sessions"`
		Actions        []store.Action  `json:"actions"`
		ControlEnabled bool            `json:"control_enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	switch {
	case got.State.Status != session.StatusScanning:
		t.Errorf("status = %q", got.State.Status)
	case got.State.SessionID != 7 || got.State.Pages != 2:
		t.Errorf("state = %+v", got.State)
	case len(got.Sessions) != 1 || got.Sessions[0].ID != 7:
		t.Errorf("sessions = %+v", got.Sessions)
	case len(got.Actions) != 1 || got.Actions[0].Kind != store.KindPageScanned:
		t.Errorf("actions = %+v", got.Actions)
	case !got.ControlEnabled:
		t.Error("control_enabled = false, want true")
	}
}

func TestStateEndpointReportsStoreErrors(t *testing.T) {
	handler := newServer(t, web.Options{Reader: &fakeReader{err: errors.New("database is locked")}})

	rec := get(t, handler, "/api/state")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "database is locked") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestActionEndpointQueuesActions(t *testing.T) {
	queued := make(chan session.Action, 4)
	handler := newServer(t, web.Options{
		Submit: func(a session.Action) error {
			queued <- a
			return nil
		},
	})

	tests := map[string]session.Action{
		"/api/action?key=a":            session.ActionScanNew,
		"/api/action?key=b":            session.ActionScanPage,
		"/api/action?key=C":            session.ActionFinish,
		"/api/action?action=scan-page": session.ActionScanPage,
		"/api/action?action=finish":    session.ActionFinish,
	}

	for path, want := range tests {
		rec := post(t, handler, path)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST %s: status = %d, want 202", path, rec.Code)
		}
		if got := <-queued; got != want {
			t.Fatalf("POST %s queued %q, want %q", path, got, want)
		}
	}
}

func TestActionEndpointRejectsBadRequests(t *testing.T) {
	handler := newServer(t, web.Options{Submit: func(session.Action) error { return nil }})

	for _, path := range []string{
		"/api/action",
		"/api/action?action=dance",
		"/api/action?key=x",
		"/api/action?key=abc",
	} {
		if rec := post(t, handler, path); rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s: status = %d, want 400", path, rec.Code)
		}
	}
}

func TestActionEndpointReportsAFullQueue(t *testing.T) {
	handler := newServer(t, web.Options{
		Submit: func(session.Action) error { return errors.New("scantool is busy") },
	})

	rec := post(t, handler, "/api/action?key=b")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "busy") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestActionEndpointIsForbiddenWithoutControl(t *testing.T) {
	handler := newServer(t, web.Options{})

	if rec := post(t, handler, "/api/action?key=b"); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestActionEndpointRejectsGet(t *testing.T) {
	handler := newServer(t, web.Options{Submit: func(session.Action) error { return nil }})

	if rec := get(t, handler, "/api/action?key=b"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	handler := newServer(t, web.Options{})

	rec := get(t, handler, "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func TestUnknownPath(t *testing.T) {
	handler := newServer(t, web.Options{})

	if rec := get(t, handler, "/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	if _, err := web.New(web.Options{Reader: &fakeReader{}}); err == nil {
		t.Error("missing State: want error, got nil")
	}
	if _, err := web.New(web.Options{State: func() session.State { return session.State{} }}); err == nil {
		t.Error("missing Reader: want error, got nil")
	}
}
