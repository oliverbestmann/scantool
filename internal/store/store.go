// Package store persists scan sessions and the action log in a SQLite
// database, so the web UI can show what the daemon has been doing.
package store

import (
	"database/sql"
	"fmt"
	"strings"
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

// Upload status values, recorded on a session once it has been saved.
const (
	UploadNone     = ""
	UploadUploaded = "uploaded"
	UploadFailed   = "failed"
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
	KindIdleTimeout      Kind = "idle-timeout"
	KindSessionStarted   Kind = "session-started"
	KindPageScanned      Kind = "page-scanned"
	KindScanFailed       Kind = "scan-failed"
	KindPageProcessed    Kind = "page-processed"
	KindProcessFailed    Kind = "process-failed"
	KindSessionSaved     Kind = "session-saved"
	KindSessionDiscarded Kind = "session-discarded"
	KindSaveFailed       Kind = "save-failed"
	KindUploadSucceeded  Kind = "upload-succeeded"
	KindUploadFailed     Kind = "upload-failed"
)

// Failed reports whether the action describes something going wrong.
func (k Kind) Failed() bool {
	switch k {
	case KindScanFailed, KindProcessFailed, KindSaveFailed, KindUploadFailed:
		return true
	default:
		return false
	}
}

// Session is one scan session, i.e. one resulting PDF document.
type Session struct {
	ID           int64      `json:"id"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	Pages        int        `json:"pages"`
	Status       string     `json:"status"`
	OutputPath   string     `json:"output_path,omitempty"`
	Error        string     `json:"error,omitempty"`
	UploadStatus string     `json:"upload_status,omitempty"`
	UploadError  string     `json:"upload_error,omitempty"`
	// LemmaryID is the record id the lemmary server assigned the document on
	// upload, so it can be looked up there later.
	LemmaryID string `json:"lemmary_id,omitempty"`
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
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	started_at    TEXT NOT NULL,
	ended_at      TEXT,
	pages         INTEGER NOT NULL DEFAULT 0,
	status        TEXT NOT NULL,
	output_path   TEXT NOT NULL DEFAULT '',
	error         TEXT NOT NULL DEFAULT '',
	upload_status TEXT NOT NULL DEFAULT '',
	upload_error  TEXT NOT NULL DEFAULT '',
	lemmary_id    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS pages (
	session_id INTEGER NOT NULL,
	page       INTEGER NOT NULL,
	path       TEXT NOT NULL,
	PRIMARY KEY (session_id, page)
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

// querier is satisfied by both *sql.DB and *sql.Tx, so the statement helpers
// below work whether or not they run inside a transaction.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
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

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// migrate adds columns introduced after the initial schema to databases
// that predate them. CREATE TABLE IF NOT EXISTS does not update existing
// tables, so new columns need an explicit, idempotent ALTER TABLE.
func migrate(db *sql.DB) error {
	columns := []string{
		`ALTER TABLE sessions ADD COLUMN upload_status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN upload_error TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN lemmary_id TEXT NOT NULL DEFAULT ''`,
	}
	for _, stmt := range columns {
		if _, err := db.Exec(stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// withTx runs fn inside a transaction, committing on success and rolling
// back on error or panic.
func (s *Store) withTx(fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
		if err != nil {
			tx.Rollback()
			return
		}
		err = tx.Commit()
	}()

	return fn(tx)
}

// CreateSessionWithAction inserts a new active session and appends the
// corresponding action log entry in a single transaction, so a session
// never exists without a matching "session started" record.
func (s *Store) CreateSessionWithAction(startedAt time.Time) (int64, error) {
	var id int64
	err := s.withTx(func(tx *sql.Tx) error {
		var err error
		if id, err = insertSession(tx, startedAt); err != nil {
			return err
		}
		_, err = insertAction(tx, Action{Kind: KindSessionStarted, SessionID: id, Time: startedAt})
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

func insertSession(q querier, startedAt time.Time) (int64, error) {
	res, err := q.Exec(
		`INSERT INTO sessions (started_at, status) VALUES (?, ?)`,
		formatTime(startedAt), StatusActive,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert session: %w", err)
	}
	return res.LastInsertId()
}

// AddPage records a scanned page and appends the matching action, updating
// the session's page count, all in one transaction.
func (s *Store) AddPage(sessionID int64, page int, path string, action Action) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`INSERT INTO pages (session_id, page, path) VALUES (?, ?, ?)`,
			sessionID, page, path,
		); err != nil {
			return fmt.Errorf("store: insert page: %w", err)
		}
		if _, err := tx.Exec(`UPDATE sessions SET pages = ? WHERE id = ?`, page, sessionID); err != nil {
			return fmt.Errorf("store: update session pages: %w", err)
		}
		_, err := insertAction(tx, action)
		return err
	})
}

// SessionPages returns the recorded page file paths of a session, ordered by
// page number.
func (s *Store) SessionPages(sessionID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT path FROM pages WHERE session_id = ? ORDER BY page`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: query pages: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("store: scan page: %w", err)
		}
		out = append(out, path)
	}
	return out, rows.Err()
}

// FinishSessionWithAction marks a session as finished with the given
// terminal status and appends the matching action, in one transaction.
func (s *Store) FinishSessionWithAction(id int64, endedAt time.Time, status, outputPath, errMsg string, action Action) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`UPDATE sessions SET ended_at = ?, status = ?, output_path = ?, error = ? WHERE id = ?`,
			formatTime(endedAt), status, outputPath, errMsg, id,
		); err != nil {
			return fmt.Errorf("store: finish session: %w", err)
		}
		_, err := insertAction(tx, action)
		return err
	})
}

// RecordUploadWithAction stores the outcome of uploading a session's
// document, including the lemmary record id on success, and appends the
// matching action, in one transaction.
func (s *Store) RecordUploadWithAction(id int64, status, lemmaryID, errMsg string, action Action) error {
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`UPDATE sessions SET upload_status = ?, upload_error = ?, lemmary_id = ? WHERE id = ?`,
			status, errMsg, lemmaryID, id,
		); err != nil {
			return fmt.Errorf("store: record upload: %w", err)
		}
		_, err := insertAction(tx, action)
		return err
	})
}

// ActiveSession returns the most recent session still marked active, so the
// daemon can resume it after a restart. It returns sql.ErrNoRows if none is
// active.
func (s *Store) ActiveSession() (Session, error) {
	rows, err := s.db.Query(`
		SELECT id, started_at, ended_at, pages, status, output_path, error, upload_status, upload_error, lemmary_id
		FROM sessions WHERE status = ? ORDER BY id DESC LIMIT 1`, StatusActive)
	if err != nil {
		return Session{}, fmt.Errorf("store: query active session: %w", err)
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

// Session loads a single session by id.
func (s *Store) Session(id int64) (Session, error) {
	rows, err := s.db.Query(`
		SELECT id, started_at, ended_at, pages, status, output_path, error, upload_status, upload_error, lemmary_id
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
		SELECT id, started_at, ended_at, pages, status, output_path, error, upload_status, upload_error, lemmary_id
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
		if err := rows.Scan(&sess.ID, &started, &ended, &sess.Pages, &sess.Status, &sess.OutputPath, &sess.Error, &sess.UploadStatus, &sess.UploadError, &sess.LemmaryID); err != nil {
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

// AppendAction stores one entry of the action log and returns its id, so a
// caller that logged an event before its session existed (e.g. the key press
// that starts one) can attach the session once known, via
// SetActionSessionID. The ID field of a is ignored.
func (s *Store) AppendAction(a Action) (int64, error) {
	return insertAction(s.db, a)
}

func insertAction(q querier, a Action) (int64, error) {
	if a.Time.IsZero() {
		a.Time = time.Now()
	}

	res, err := q.Exec(
		`INSERT INTO actions (time, kind, session_id, page, detail, error)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		formatTime(a.Time), string(a.Kind), a.SessionID, a.Page, a.Detail, a.Error,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert action: %w", err)
	}
	return res.LastInsertId()
}

// SetActionSessionID attaches a session to an action log entry that was
// recorded before the session existed, e.g. the key press or web request
// that started it. It only touches rows still unattached, so it can't
// clobber a legitimate value.
func (s *Store) SetActionSessionID(actionID, sessionID int64) error {
	if _, err := s.db.Exec(`UPDATE actions SET session_id = ? WHERE id = ? AND session_id = 0`, sessionID, actionID); err != nil {
		return fmt.Errorf("store: set action session: %w", err)
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
