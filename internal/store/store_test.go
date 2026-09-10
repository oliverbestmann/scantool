package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oliverbestmann/scantool/internal/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "scantool.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSessionLifecycle(t *testing.T) {
	db := open(t)

	started := time.Date(2026, 9, 8, 11, 44, 0, 0, time.UTC)
	id, err := db.CreateSessionWithAction(started)
	if err != nil {
		t.Fatalf("CreateSessionWithAction: %v", err)
	}

	sess, err := db.Session(id)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if sess.Status != store.StatusActive {
		t.Fatalf("status = %q, want %q", sess.Status, store.StatusActive)
	}
	if !sess.StartedAt.Equal(started) {
		t.Fatalf("started_at = %v, want %v", sess.StartedAt, started)
	}
	if sess.EndedAt != nil {
		t.Fatalf("ended_at = %v, want nil", sess.EndedAt)
	}

	if err := db.AddPage(id, 3, "/work/page-003.pdf", store.Action{Kind: store.KindPageScanned, SessionID: id, Page: 3}); err != nil {
		t.Fatalf("AddPage: %v", err)
	}

	ended := started.Add(90 * time.Second)
	err = db.FinishSessionWithAction(id, ended, store.StatusSaved, "/scans/20260908-114400.pdf", "",
		store.Action{Kind: store.KindSessionSaved, SessionID: id})
	if err != nil {
		t.Fatalf("FinishSessionWithAction: %v", err)
	}

	sess, err = db.Session(id)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	switch {
	case sess.Pages != 3:
		t.Fatalf("pages = %d, want 3", sess.Pages)
	case sess.Status != store.StatusSaved:
		t.Fatalf("status = %q, want %q", sess.Status, store.StatusSaved)
	case sess.OutputPath != "/scans/20260908-114400.pdf":
		t.Fatalf("output_path = %q", sess.OutputPath)
	case sess.EndedAt == nil || !sess.EndedAt.Equal(ended):
		t.Fatalf("ended_at = %v, want %v", sess.EndedAt, ended)
	}
}

func TestSessionNotFound(t *testing.T) {
	db := open(t)

	if _, err := db.Session(42); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Session(42) error = %v, want sql.ErrNoRows", err)
	}
}

func TestRecentSessionsIsNewestFirst(t *testing.T) {
	db := open(t)

	base := time.Date(2026, 9, 8, 9, 0, 0, 0, time.UTC)
	for i := range 5 {
		if _, err := db.CreateSessionWithAction(base.Add(time.Duration(i) * time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	sessions, err := db.RecentSessions(3)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("got %d sessions, want 3", len(sessions))
	}
	for i, want := range []int64{5, 4, 3} {
		if sessions[i].ID != want {
			t.Fatalf("sessions[%d].ID = %d, want %d", i, sessions[i].ID, want)
		}
	}
}

func TestActionLogRoundTrip(t *testing.T) {
	db := open(t)

	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	want := store.Action{
		Time:      now,
		Kind:      store.KindPageScanned,
		SessionID: 7,
		Page:      2,
		Detail:    "/scans/page-002.pdf",
	}
	if err := db.AppendAction(want); err != nil {
		t.Fatalf("AppendAction: %v", err)
	}
	if err := db.AppendAction(store.Action{Time: now.Add(time.Second), Kind: store.KindScanFailed, SessionID: 7, Error: "device busy"}); err != nil {
		t.Fatal(err)
	}

	actions, err := db.RecentActions(10)
	if err != nil {
		t.Fatalf("RecentActions: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(actions))
	}

	// Newest first.
	if actions[0].Kind != store.KindScanFailed || actions[0].Error != "device busy" {
		t.Fatalf("actions[0] = %+v", actions[0])
	}

	got := actions[1]
	got.ID = 0
	if !got.Time.Equal(want.Time) {
		t.Fatalf("time = %v, want %v", got.Time, want.Time)
	}
	got.Time = want.Time
	if got != want {
		t.Fatalf("action = %+v, want %+v", got, want)
	}
}

func TestActionKindFailed(t *testing.T) {
	for kind, want := range map[store.Kind]bool{
		store.KindScanFailed:    true,
		store.KindSaveFailed:    true,
		store.KindPageScanned:   false,
		store.KindSessionSaved:  false,
		store.KindDaemonStarted: false,
	} {
		if got := kind.Failed(); got != want {
			t.Errorf("%q.Failed() = %v, want %v", kind, got, want)
		}
	}
}

func TestSessionActionsAreOldestFirst(t *testing.T) {
	db := open(t)

	now := time.Now()
	for i, kind := range []store.Kind{store.KindSessionStarted, store.KindPageScanned, store.KindSessionSaved} {
		err := db.AppendAction(store.Action{Time: now.Add(time.Duration(i) * time.Second), Kind: kind, SessionID: 3})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AppendAction(store.Action{Time: now, Kind: store.KindPageScanned, SessionID: 4}); err != nil {
		t.Fatal(err)
	}

	actions, err := db.SessionActions(3)
	if err != nil {
		t.Fatalf("SessionActions: %v", err)
	}
	if len(actions) != 3 {
		t.Fatalf("got %d actions, want 3", len(actions))
	}
	if actions[0].Kind != store.KindSessionStarted || actions[2].Kind != store.KindSessionSaved {
		t.Fatalf("unexpected order: %+v", actions)
	}
}

func TestTrimActionsKeepsNewest(t *testing.T) {
	db := open(t)

	now := time.Now()
	for i := range 10 {
		err := db.AppendAction(store.Action{Time: now.Add(time.Duration(i) * time.Second), Kind: store.KindKeyPressed, Detail: "b"})
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := db.TrimActions(4); err != nil {
		t.Fatalf("TrimActions: %v", err)
	}

	actions, err := db.RecentActions(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 4 {
		t.Fatalf("got %d actions after trim, want 4", len(actions))
	}
	if actions[0].ID != 10 || actions[3].ID != 7 {
		t.Fatalf("trim kept the wrong rows: %d..%d", actions[3].ID, actions[0].ID)
	}
}

func TestTrimActionsBelowLimitKeepsEverything(t *testing.T) {
	db := open(t)

	if err := db.AppendAction(store.Action{Kind: store.KindDaemonStarted}); err != nil {
		t.Fatal(err)
	}
	if err := db.TrimActions(100); err != nil {
		t.Fatalf("TrimActions: %v", err)
	}

	actions, err := db.RecentActions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions))
	}
}

func TestAppendActionDefaultsTime(t *testing.T) {
	db := open(t)

	if err := db.AppendAction(store.Action{Kind: store.KindDaemonStarted}); err != nil {
		t.Fatal(err)
	}

	actions, err := db.RecentActions(1)
	if err != nil {
		t.Fatal(err)
	}
	if actions[0].Time.IsZero() {
		t.Fatal("time was not filled in")
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scantool.db")

	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateSessionWithAction(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	sessions, err := db.RecentSessions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions after reopen, want 1", len(sessions))
	}
}
