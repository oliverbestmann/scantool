// Package store persists scan sessions and the action log in a SQLite
// database, so the web UI can show what the daemon has been doing.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Session status values.
const (
	StatusActive    = "active"
	StatusSaved     = "saved"
	StatusDiscarded = "discarded"
	StatusFailed    = "failed"
)

// Kind identifies what happened. Kinds are stable strings, the web UI maps
// them to labels.
type Kind string

// Action kinds.
const (
	KindDaemonStarted    Kind = "daemon-started"
	KindDaemonStopped    Kind = "daemon-stopped"
	KindKeyPressed       Kind = "key-pressed"
	KindWebAction        Kind = "web-action"
	KindKeyIgnored       Kind = "key-ignored"
	KindSessionStarted   Kind = "session-started"
	KindPageScanned      Kind = "page-scanned"
	KindScanFailed       Kind = "scan-failed"
	KindSessionSaved     Kind = "session-saved"
	KindSessionDiscarded Kind = "session-discarded"
	KindSaveFailed       Kind = "save-failed"
)

// Failed reports whether the action describes something going wrong.
func (k Kind) Failed() bool {
	switch k {
	case KindScanFailed, KindSaveFailed:
		return true
	default:
		return false
	}
}

// Session is one scan session, i.e. one resulting PDF document.
type Session struct {
	ID         int64      `json:"id"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	Pages      int        `json:"pages"`
	Status     string     `json:"status"`
	OutputPath string     `json:"output_path,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Action is one entry of the action log.
type Action struct {
	ID int64 `json:"id"`
	// Time is when the action happened.
	Time time.Time `json:"time"`
	// Kind is what happened.
	Kind Kind `json:"kind"`
	// SessionID is the session involved, or 0.
	SessionID int64 `json:"session_id,omitempty"`
	// Page is the page number involved, or 0.
	Page int `json:"page,omitempty"`
	// Detail carries the interesting value: the key, the output path, ...
	Detail string `json:"detail,omitempty"`
	// Error is set when the action failed.
	Error string `json:"error,omitempty"`
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	started_at  TEXT NOT NULL,
	ended_at    TEXT,
	pages       INTEGER NOT NULL DEFAULT 0,
	status      TEXT NOT NULL,
	output_path TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS actions (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	time       TEXT NOT NULL,
	kind       TEXT NOT NULL,
	session_id INTEGER NOT NULL DEFAULT 0,
	page       INTEGER NOT NULL DEFAULT 0,
	detail     TEXT NOT NULL DEFAULT '',
	error      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS actions_session ON actions (session_id);
`

// Store is a SQLite backed persistence layer. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens, and if needed creates, the database at path. Use ":memory:"
// for an ephemeral database.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	if path == ":memory:" {
		dsn = "file::memory:"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// The daemon sees very little traffic. A single connection avoids any
	// "database is locked" surprises on a slow SD card, and keeps an
	// in-memory database alive.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: create schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// CreateSession inserts a new active session and returns its id.
func (s *Store) CreateSession(startedAt time.Time) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO sessions (started_at, status) VALUES (?, ?)`,
		formatTime(startedAt), StatusActive,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert session: %w", err)
	}
	return res.LastInsertId()
}

// SetSessionPages records how many pages have been scanned into a session.
func (s *Store) SetSessionPages(id int64, pages int) error {
	if _, err := s.db.Exec(`UPDATE sessions SET pages = ? WHERE id = ?`, pages, id); err != nil {
		return fmt.Errorf("store: update session pages: %w", err)
	}
	return nil
}

// FinishSession marks a session as finished with the given terminal status.
func (s *Store) FinishSession(id int64, endedAt time.Time, status, outputPath, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET ended_at = ?, status = ?, output_path = ?, error = ? WHERE id = ?`,
		formatTime(endedAt), status, outputPath, errMsg, id,
	)
	if err != nil {
		return fmt.Errorf("store: finish session: %w", err)
	}
	return nil
}

// Session loads a single session by id.
func (s *Store) Session(id int64) (Session, error) {
	rows, err := s.db.Query(`
		SELECT id, started_at, ended_at, pages, status, output_path, error
		FROM sessions WHERE id = ?`, id)
	if err != nil {
		return Session{}, fmt.Errorf("store: query session: %w", err)
	}
	defer rows.Close()

	sessions, err := scanSessions(rows)
	if err != nil {
		return Session{}, err
	}
	if len(sessions) == 0 {
		return Session{}, sql.ErrNoRows
	}
	return sessions[0], nil
}

// RecentSessions returns the newest sessions, most recent first.
func (s *Store) RecentSessions(limit int) ([]Session, error) {
	rows, err := s.db.Query(`
		SELECT id, started_at, ended_at, pages, status, output_path, error
		FROM sessions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: query sessions: %w", err)
	}
	defer rows.Close()
	return scanSessions(rows)
}

func scanSessions(rows *sql.Rows) ([]Session, error) {
	var out []Session
	for rows.Next() {
		var (
			sess    Session
			started string
			ended   sql.NullString
		)
		if err := rows.Scan(&sess.ID, &started, &ended, &sess.Pages, &sess.Status, &sess.OutputPath, &sess.Error); err != nil {
			return nil, fmt.Errorf("store: scan session: %w", err)
		}

		var err error
		if sess.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if ended.Valid {
			t, err := parseTime(ended.String)
			if err != nil {
				return nil, err
			}
			sess.EndedAt = &t
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// AppendAction stores one entry of the action log. The ID field is ignored.
func (s *Store) AppendAction(a Action) error {
	if a.Time.IsZero() {
		a.Time = time.Now()
	}

	_, err := s.db.Exec(
		`INSERT INTO actions (time, kind, session_id, page, detail, error)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		formatTime(a.Time), string(a.Kind), a.SessionID, a.Page, a.Detail, a.Error,
	)
	if err != nil {
		return fmt.Errorf("store: insert action: %w", err)
	}
	return nil
}

// RecentActions returns the newest log entries, most recent first.
func (s *Store) RecentActions(limit int) ([]Action, error) {
	return s.queryActions(`
		SELECT id, time, kind, session_id, page, detail, error
		FROM actions ORDER BY id DESC LIMIT ?`, limit)
}

// SessionActions returns the log entries of one session, oldest first.
func (s *Store) SessionActions(sessionID int64) ([]Action, error) {
	return s.queryActions(`
		SELECT id, time, kind, session_id, page, detail, error
		FROM actions WHERE session_id = ? ORDER BY id`, sessionID)
}

func (s *Store) queryActions(query string, args ...any) ([]Action, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query actions: %w", err)
	}
	defer rows.Close()

	var out []Action
	for rows.Next() {
		var (
			a    Action
			ts   string
			kind string
		)
		if err := rows.Scan(&a.ID, &ts, &kind, &a.SessionID, &a.Page, &a.Detail, &a.Error); err != nil {
			return nil, fmt.Errorf("store: scan action: %w", err)
		}

		a.Kind = Kind(kind)
		if a.Time, err = parseTime(ts); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TrimActions deletes all but the newest keep entries, so the database does
// not grow without bound on a long running Pi.
func (s *Store) TrimActions(keep int) error {
	_, err := s.db.Exec(`
		DELETE FROM actions WHERE id <= COALESCE(
			(SELECT id FROM actions ORDER BY id DESC LIMIT 1 OFFSET ?), -1)`, keep)
	if err != nil {
		return fmt.Errorf("store: trim actions: %w", err)
	}
	return nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parse time %q: %w", s, err)
	}
	return t.Local(), nil
}
