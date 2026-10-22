// Package store keeps agentdeck's state in SQLite (pure Go driver, no cgo).
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"agentdeck/internal/model"
)

// Store wraps the database. It is safe for concurrent use.
type Store struct{ db *sql.DB }

var migrations = []string{
	`CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		data TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		text TEXT NOT NULL,
		source TEXT NOT NULL,
		at INTEGER NOT NULL
	);
	CREATE INDEX history_session ON history(session_id, id);
	CREATE TABLE events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		type TEXT NOT NULL,
		tool TEXT,
		summary TEXT,
		at INTEGER NOT NULL
	);
	CREATE INDEX events_session ON events(session_id, id);
	CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
	// Phase 2: context items, and tool calls with their status and output.
	`CREATE TABLE context_items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		ref TEXT NOT NULL DEFAULT '',
		payload TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		pinned INTEGER NOT NULL DEFAULT 0,
		sent_hash TEXT NOT NULL DEFAULT '',
		sent_at INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX context_session ON context_items(session_id, id);
	ALTER TABLE events ADD COLUMN tool_use_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE events ADD COLUMN status TEXT NOT NULL DEFAULT '';
	ALTER TABLE events ADD COLUMN input TEXT NOT NULL DEFAULT '';
	ALTER TABLE events ADD COLUMN output TEXT NOT NULL DEFAULT '';`,
}

// HistoryCap is how many prompts are kept in total.
const HistoryCap = 10000

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Sessions are stored as JSON documents: the sidebar needs all fields at
// once and the shape changes often while the app is young.

func (s *Store) SaveSession(x model.Session) error {
	b, err := json.Marshal(x)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO sessions(id, data, created_at) VALUES(?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET data = excluded.data`, x.ID, string(b), x.CreatedAt)
	return err
}

func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

func (s *Store) Sessions() ([]model.Session, error) {
	rows, err := s.db.Query(`SELECT data FROM sessions ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Session
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var x model.Session
		if err := json.Unmarshal([]byte(data), &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// AddHistory records a sent prompt. A prompt identical to the session's
// previous one is not stored again (it also catches the hook reporting a
// prompt the deck just sent). It returns the entry and whether it was added.
func (s *Store) AddHistory(sessionID, text, source string) (model.HistoryEntry, bool, error) {
	var last string
	err := s.db.QueryRow(`SELECT text FROM history WHERE session_id = ? ORDER BY id DESC LIMIT 1`, sessionID).Scan(&last)
	if err != nil && err != sql.ErrNoRows {
		return model.HistoryEntry{}, false, err
	}
	if last == text {
		return model.HistoryEntry{}, false, nil
	}
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO history(session_id, text, source, at) VALUES(?, ?, ?, ?)`, sessionID, text, source, now)
	if err != nil {
		return model.HistoryEntry{}, false, err
	}
	id, _ := res.LastInsertId()
	if id%100 == 0 {
		_, _ = s.db.Exec(`DELETE FROM history WHERE id <= ?`, id-HistoryCap)
	}
	return model.HistoryEntry{ID: id, SessionID: sessionID, Text: text, At: now}, true, nil
}

// History returns prompts oldest first; sessionID "" means all sessions.
func (s *Store) History(sessionID string, limit int) ([]model.HistoryEntry, error) {
	if limit <= 0 {
		limit = HistoryCap
	}
	q := `SELECT id, session_id, text, at FROM (SELECT * FROM history ORDER BY id DESC LIMIT ?) ORDER BY id`
	args := []any{limit}
	if sessionID != "" {
		q = `SELECT id, session_id, text, at FROM (SELECT * FROM history WHERE session_id = ? ORDER BY id DESC LIMIT ?) ORDER BY id`
		args = []any{sessionID, limit}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.HistoryEntry
	for rows.Next() {
		var e model.HistoryEntry
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Text, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AddEvent logs a hook event that isn't a tool call (notifications).
func (s *Store) AddEvent(sessionID, typ, tool, summary string) error {
	_, err := s.db.Exec(`INSERT INTO events(session_id, type, tool, summary, at) VALUES(?, ?, ?, ?, ?)`,
		sessionID, typ, tool, summary, time.Now().Unix())
	return err
}

// maxOutput caps the stored tool input and output.
const maxOutput = 64 << 10

func clip(s string) string {
	if len(s) > maxOutput {
		return s[:maxOutput] + "\n… (cut)"
	}
	return s
}

// StartTool records a tool call that is starting.
func (s *Store) StartTool(sessionID, useID, tool, summary, input string) (model.ToolCall, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO events(session_id, type, tool, summary, at, tool_use_id, status, input)
		VALUES(?, 'tool', ?, ?, ?, ?, ?, ?)`, sessionID, tool, summary, now, useID, model.ToolRunning, clip(input))
	if err != nil {
		return model.ToolCall{}, err
	}
	id, _ := res.LastInsertId()
	return model.ToolCall{ID: id, SessionID: sessionID, Tool: tool, Summary: summary, Status: model.ToolRunning, At: now}, nil
}

// FinishTool marks the matching running call (by tool use id, else the
// newest running call of that tool) as done, and stores its output. A
// call that was never seen starting is recorded as a new one.
func (s *Store) FinishTool(sessionID, useID, tool, summary, input, output, status string) (model.ToolCall, error) {
	var id int64
	err := sql.ErrNoRows
	if useID != "" {
		err = s.db.QueryRow(`SELECT id FROM events WHERE session_id = ? AND type = 'tool' AND tool_use_id = ? ORDER BY id DESC LIMIT 1`,
			sessionID, useID).Scan(&id)
	}
	if err == sql.ErrNoRows {
		err = s.db.QueryRow(`SELECT id FROM events WHERE session_id = ? AND type = 'tool' AND tool = ? AND status = ? ORDER BY id DESC LIMIT 1`,
			sessionID, tool, model.ToolRunning).Scan(&id)
	}
	if err == sql.ErrNoRows {
		c, err := s.StartTool(sessionID, useID, tool, summary, input)
		if err != nil {
			return c, err
		}
		id = c.ID
	} else if err != nil {
		return model.ToolCall{}, err
	}
	if _, err := s.db.Exec(`UPDATE events SET status = ?, output = ? WHERE id = ?`, status, clip(output), id); err != nil {
		return model.ToolCall{}, err
	}
	return s.ToolCall(id)
}

// ToolCall returns one call with its input and output.
func (s *Store) ToolCall(id int64) (model.ToolCall, error) {
	var c model.ToolCall
	err := s.db.QueryRow(`SELECT id, session_id, tool, summary, status, input, output, at FROM events WHERE id = ? AND type = 'tool'`, id).
		Scan(&c.ID, &c.SessionID, &c.Tool, &c.Summary, &c.Status, &c.Input, &c.Output, &c.At)
	return c, err
}

// ToolCalls returns a session's newest calls, oldest first, without their
// input and output.
func (s *Store) ToolCalls(sessionID string, limit int) ([]model.ToolCall, error) {
	rows, err := s.db.Query(`SELECT id, session_id, tool, summary, status, at FROM
		(SELECT * FROM events WHERE session_id = ? AND type = 'tool' ORDER BY id DESC LIMIT ?) ORDER BY id`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ToolCall
	for rows.Next() {
		var c model.ToolCall
		if err := rows.Scan(&c.ID, &c.SessionID, &c.Tool, &c.Summary, &c.Status, &c.At); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// StopRunningTools marks a session's unfinished calls as failed (the
// turn was interrupted, or claude exited).
func (s *Store) StopRunningTools(sessionID string) error {
	_, err := s.db.Exec(`UPDATE events SET status = ? WHERE session_id = ? AND type = 'tool' AND status = ?`,
		model.ToolFailed, sessionID, model.ToolRunning)
	return err
}

// Context items.

const itemCols = `id, session_id, kind, ref, payload, title, pinned, sent_hash, sent_at, created_at`

func scanItem(sc interface{ Scan(...any) error }) (model.ContextItem, error) {
	var it model.ContextItem
	var pinned int
	err := sc.Scan(&it.ID, &it.SessionID, &it.Kind, &it.Ref, &it.Payload, &it.Title, &pinned, &it.SentHash, &it.SentAt, &it.CreatedAt)
	it.Pinned = pinned != 0
	return it, err
}

// AddItem stores a new context item and returns it with its id.
func (s *Store) AddItem(it model.ContextItem) (model.ContextItem, error) {
	it.CreatedAt = time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO context_items(session_id, kind, ref, payload, title, pinned, sent_hash, sent_at, created_at)
		VALUES(?, ?, ?, ?, ?, ?, '', 0, ?)`, it.SessionID, it.Kind, it.Ref, it.Payload, it.Title, b2i(it.Pinned), it.CreatedAt)
	if err != nil {
		return it, err
	}
	it.ID, _ = res.LastInsertId()
	it.SentHash, it.SentAt = "", 0
	return it, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Items returns a session's context items in the order they were added.
func (s *Store) Items(sessionID string) ([]model.ContextItem, error) {
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM context_items WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ContextItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Item returns one context item.
func (s *Store) Item(id int64) (model.ContextItem, error) {
	return scanItem(s.db.QueryRow(`SELECT `+itemCols+` FROM context_items WHERE id = ?`, id))
}

// DeleteItem removes a context item.
func (s *Store) DeleteItem(id int64) error {
	_, err := s.db.Exec(`DELETE FROM context_items WHERE id = ?`, id)
	return err
}

// DeleteItems removes every item of a session (when it is closed).
func (s *Store) DeleteItems(sessionID string) error {
	_, err := s.db.Exec(`DELETE FROM context_items WHERE session_id = ?`, sessionID)
	return err
}

// SetPinned pins or unpins an item.
func (s *Store) SetPinned(id int64, pinned bool) error {
	_, err := s.db.Exec(`UPDATE context_items SET pinned = ? WHERE id = ?`, b2i(pinned), id)
	return err
}

// MarkSent records what was sent for an item; an empty hash makes it go
// again with the next prompt.
func (s *Store) MarkSent(id int64, hash string) error {
	at := time.Now().Unix()
	if hash == "" {
		at = 0
	}
	_, err := s.db.Exec(`UPDATE context_items SET sent_hash = ?, sent_at = ? WHERE id = ?`, hash, at, id)
	return err
}

func (s *Store) Get(key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	return v
}

func (s *Store) Set(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO kv(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
