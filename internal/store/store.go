// Package store keeps the local history: every message, what happened to its
// deliveries, the known-address list and the refused attempts. Rows live in
// SQLite, raw messages in a spool directory next to it.
//
// The history is a record, not a queue: mail is acknowledged only after the
// webhook accepted it, so losing this directory loses nothing but the record.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite"
)

// timeLayout is fixed-width UTC, so stored times sort as text.
const timeLayout = "2006-01-02T15:04:05.000Z"

const (
	StatusPending   = "pending"   // an attempt is running or scheduled here
	StatusDelivered = "delivered" // the webhook answered 2xx
	StatusDeferred  = "deferred"  // answered 451; the sending server will retry
	StatusFailed    = "failed"    // refused for good, or out of attempts

	TriggerSMTP   = "smtp"
	TriggerResend = "resend"

	RecipientKnown   = "known"
	RecipientUnknown = "unknown"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db      *sql.DB
	dir     string
	keepRaw bool
}

const schema = `
CREATE TABLE messages (
	id           TEXT PRIMARY KEY,
	dedupe_key   TEXT NOT NULL UNIQUE,
	sha256       TEXT NOT NULL,
	received_at  TEXT NOT NULL,
	last_seen_at TEXT NOT NULL,
	seen_count   INTEGER NOT NULL DEFAULT 1,
	mail_from    TEXT NOT NULL,
	rcpt_to      TEXT NOT NULL,
	remote_addr  TEXT NOT NULL,
	helo         TEXT NOT NULL,
	tls          INTEGER NOT NULL,
	size         INTEGER NOT NULL,
	from_email   TEXT NOT NULL,
	from_name    TEXT NOT NULL,
	to_header    TEXT NOT NULL,
	subject      TEXT NOT NULL,
	message_id   TEXT NOT NULL,
	attachments  TEXT NOT NULL,
	notes        TEXT NOT NULL,
	parse_error  TEXT NOT NULL,
	raw_path     TEXT NOT NULL,
	status       TEXT NOT NULL
);
CREATE INDEX messages_status ON messages(status, id);
CREATE INDEX messages_last_seen ON messages(last_seen_at);

CREATE TABLE deliveries (
	id               TEXT PRIMARY KEY,
	message_id       TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	recipient        TEXT NOT NULL,
	trigger          TEXT NOT NULL,
	event            TEXT NOT NULL,
	url              TEXT NOT NULL,
	attempt          INTEGER NOT NULL DEFAULT 0,
	status           TEXT NOT NULL,
	last_status_code INTEGER NOT NULL DEFAULT 0,
	last_error       TEXT NOT NULL DEFAULT '',
	response_excerpt TEXT NOT NULL DEFAULT '',
	next_attempt_at  TEXT,
	delivered_at     TEXT,
	created_at       TEXT NOT NULL,
	updated_at       TEXT NOT NULL
);
CREATE INDEX deliveries_message ON deliveries(message_id, id);
CREATE INDEX deliveries_due ON deliveries(status, next_attempt_at);

CREATE TABLE delivery_attempts (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	delivery_id TEXT NOT NULL REFERENCES deliveries(id) ON DELETE CASCADE,
	attempt     INTEGER NOT NULL,
	at          TEXT NOT NULL,
	status_code INTEGER NOT NULL DEFAULT 0,
	error       TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX delivery_attempts_delivery ON delivery_attempts(delivery_id, id);

CREATE TABLE recipients (
	address       TEXT PRIMARY KEY,
	status        TEXT NOT NULL,
	first_seen_at TEXT NOT NULL,
	checked_at    TEXT NOT NULL,
	last_mail_at  TEXT,
	accepted      INTEGER NOT NULL DEFAULT 0,
	refused       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE refusals (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	bucket      TEXT NOT NULL,
	remote_addr TEXT NOT NULL,
	helo        TEXT NOT NULL,
	mail_from   TEXT NOT NULL,
	rcpt        TEXT NOT NULL,
	reason      TEXT NOT NULL,
	count       INTEGER NOT NULL DEFAULT 1,
	first_at    TEXT NOT NULL,
	last_at     TEXT NOT NULL,
	UNIQUE (bucket, remote_addr, mail_from, rcpt, reason)
);
CREATE INDEX refusals_last ON refusals(last_at);

CREATE TABLE sink_receipts (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	received_at      TEXT NOT NULL,
	delivery_id      TEXT NOT NULL,
	event            TEXT NOT NULL,
	signature_status TEXT NOT NULL,
	body             TEXT NOT NULL
);
CREATE INDEX sink_receipts_delivery ON sink_receipts(delivery_id, id);
`

// Open creates the data directory, the database and the spool as needed.
func Open(dir string, keepRaw bool) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "spool"), 0o750); err != nil {
		return nil, fmt.Errorf("data directory: %w", err)
	}
	dsn := "file:" + filepath.Join(dir, "app.db") +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite has one writer anyway, and the volume is a
	// mailbox's, not a web application's.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, dir: dir, keepRaw: keepRaw}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if version >= 1 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := tx.Exec(`PRAGMA user_version = 1`); err != nil {
		return err
	}
	return tx.Commit()
}

// DatabaseBytes and SpoolBytes feed the settings page.
func (s *Store) DatabaseBytes() int64 {
	var total int64
	for _, name := range []string{"app.db", "app.db-wal"} {
		if info, err := os.Stat(filepath.Join(s.dir, name)); err == nil {
			total += info.Size()
		}
	}
	return total
}

func (s *Store) SpoolBytes() int64 {
	var total sql.NullInt64
	_ = s.db.QueryRow(`SELECT SUM(size) FROM messages WHERE raw_path != ''`).Scan(&total)
	return total.Int64
}

func newID(prefix string) string { return prefix + "_" + ulid.Make().String() }

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(v string) time.Time {
	t, _ := time.Parse(timeLayout, v)
	return t
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func scanTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseTime(v.String)
	return &t
}

// like turns user input into a "contains" pattern with its wildcards escaped.
func like(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(v) + "%"
}

// NewDeliveryID is for a delivery that has no row, when the history is unavailable.
func NewDeliveryID() string { return newID("whd") }
