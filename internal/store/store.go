// Package store implements the SQLite backed persistence layer of the event
// center.
//
// # Ordering contract
//
// SQLite has a single writer. Store serializes every write with writeMu and
// wraps ingestion in one transaction, so the order in which events receive
// their global sequence is exactly the order in which they commit. A consumer
// reading "seq > cursor" therefore can never observe a hole caused by a
// long-running transaction committing later with a lower sequence.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kaulie/event-center/internal/idgen"

	_ "modernc.org/sqlite"
)

// Store is the persistence facade.
type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
}

// Open opens (and migrates) the SQLite database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn, err := dsnFor(path)
	if err != nil {
		return nil, err
	}
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create db dir: %w", err)
			}
		}
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One writer is enforced in Go (writeMu); allow a few connections so
	// concurrent reads and long-poll polls do not queue behind each other.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func dsnFor(path string) (string, error) {
	pragmas := "_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		return "file:eventd_mem?mode=memory&cache=shared&" + pragmas, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return "file:" + url.PathEscape(abs) + "?" + pragmas, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle (used by health checks).
func (s *Store) DB() *sql.DB { return s.db }

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS streams (
		name       TEXT PRIMARY KEY,
		last_seq   INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS events (
		seq         INTEGER PRIMARY KEY AUTOINCREMENT,
		stream_seq  INTEGER NOT NULL,
		id          TEXT NOT NULL UNIQUE,
		stream      TEXT NOT NULL,
		provider    TEXT NOT NULL,
		type        TEXT NOT NULL,
		subject     TEXT NOT NULL DEFAULT '',
		source_time TEXT,
		received_at TEXT NOT NULL,
		dedupe_key  TEXT NOT NULL DEFAULT '',
		headers     TEXT NOT NULL DEFAULT '{}',
		data_type   TEXT NOT NULL DEFAULT 'application/json',
		data        TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_events_stream_seq ON events (stream, stream_seq)`,
	`CREATE INDEX IF NOT EXISTS idx_events_provider_type ON events (provider, type, seq)`,
	`CREATE INDEX IF NOT EXISTS idx_events_received_at ON events (received_at)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_events_dedupe ON events (provider, dedupe_key) WHERE dedupe_key <> ''`,
	`CREATE TABLE IF NOT EXISTS sources (
		id             TEXT PRIMARY KEY,
		kind           TEXT NOT NULL,
		secret         TEXT NOT NULL DEFAULT '',
		verify_mode    TEXT NOT NULL,
		type_prefix    TEXT NOT NULL DEFAULT '',
		default_stream TEXT NOT NULL,
		enabled        INTEGER NOT NULL DEFAULT 1,
		created_at     TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS subscriptions (
		id              TEXT PRIMARY KEY,
		name            TEXT NOT NULL,
		stream          TEXT NOT NULL,
		type_filters    TEXT NOT NULL DEFAULT '[]',
		provider_filters TEXT NOT NULL DEFAULT '[]',
		subject_pattern TEXT NOT NULL DEFAULT '',
		delivery        TEXT NOT NULL,
		endpoint        TEXT NOT NULL DEFAULT '',
		secret          TEXT NOT NULL DEFAULT '',
		status          TEXT NOT NULL DEFAULT 'active',
		cursor          INTEGER NOT NULL DEFAULT 0,
		created_at      TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_subscriptions_status ON subscriptions (status, stream)`,
	`CREATE TABLE IF NOT EXISTS deliveries (
		subscription_id TEXT NOT NULL,
		event_seq       INTEGER NOT NULL,
		attempt         INTEGER NOT NULL DEFAULT 0,
		status          TEXT NOT NULL DEFAULT 'pending',
		last_error      TEXT NOT NULL DEFAULT '',
		next_retry_at   TEXT,
		updated_at      TEXT NOT NULL,
		PRIMARY KEY (subscription_id, event_seq)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_deliveries_due ON deliveries (status, next_retry_at)`,
}

func (s *Store) migrate(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	for _, stmt := range migrations {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration failed: %w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

// NewID returns a prefixed, sortable identifier, e.g. "evt_01J8Q...".
func NewID(prefix string) string { return idgen.NewID(prefix) }

// timeLayout is a fixed width UTC timestamp. A fixed width matters: timestamps
// are compared lexicographically in SQL (retention purges, ordering), and
// time.RFC3339Nano trims trailing zeros which would make ".1Z" sort after
// ".15Z".
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(v string) (time.Time, error) { return time.Parse(timeLayout, v) }

func nullTime(v string) *time.Time {
	if v == "" {
		return nil
	}
	t, err := parseTime(v)
	if err != nil {
		return nil
	}
	return &t
}
