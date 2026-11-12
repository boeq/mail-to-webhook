package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type AttachmentInfo struct {
	Name        string `json:"name"`
	ContentType string `json:"type"`
	Size        int    `json:"size"`
}

type Message struct {
	ID          string
	SHA256      string
	ReceivedAt  time.Time
	LastSeenAt  time.Time
	SeenCount   int
	MailFrom    string
	RcptTo      []string
	RemoteAddr  string
	Helo        string
	TLS         bool
	Size        int
	FromEmail   string
	FromName    string
	ToHeader    string
	Subject     string
	MessageID   string
	Attachments []AttachmentInfo
	Notes       []string
	ParseError  string
	RawPath     string
	Status      string
}

// HasRaw reports whether the raw message is still on disk, which is what a
// resend and the payload view need.
func (m *Message) HasRaw() bool { return m.RawPath != "" }

type Delivery struct {
	ID              string
	MessageID       string
	Recipient       string
	Trigger         string
	Event           string
	URL             string
	Attempt         int
	Status          string
	LastStatusCode  int
	LastError       string
	ResponseExcerpt string
	NextAttemptAt   *time.Time
	DeliveredAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Attempt struct {
	Attempt    int
	At         time.Time
	StatusCode int
	Error      string
	DurationMS int
}

// RecordMessage stores a message, or recognises one seen before.
//
// A sending server that got a 451 offers the same bytes to the same recipients
// again. That is the same message, not a new one: it keeps its row, and its
// deliveries keep their ids. The second result is true in that case.
func (s *Store) RecordMessage(m Message, raw []byte) (*Message, bool, error) {
	sum := sha256.Sum256(raw)
	m.SHA256 = hex.EncodeToString(sum[:])
	key := dedupeKey(m.SHA256, m.MailFrom, m.RcptTo)
	now := formatTime(m.ReceivedAt)

	var existing string
	err := s.db.QueryRow(`SELECT id FROM messages WHERE dedupe_key = ?`, key).Scan(&existing)
	switch {
	case err == nil:
		if _, err := s.db.Exec(`UPDATE messages SET last_seen_at = ?, seen_count = seen_count + 1 WHERE id = ?`, now, existing); err != nil {
			return nil, false, err
		}
		stored, err := s.GetMessage(existing)
		return stored, true, err
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, err
	}

	m.ID = newID("msg")
	m.LastSeenAt = m.ReceivedAt
	m.SeenCount = 1
	m.Status = StatusPending
	if s.keepRaw {
		rel, err := s.writeRaw(m.ID, m.ReceivedAt, raw)
		if err != nil {
			return nil, false, err
		}
		m.RawPath = rel
	}

	_, err = s.db.Exec(`INSERT INTO messages
		(id, dedupe_key, sha256, received_at, last_seen_at, seen_count, mail_from, rcpt_to, remote_addr, helo, tls, size,
		 from_email, from_name, to_header, subject, message_id, attachments, notes, parse_error, raw_path, status)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, key, m.SHA256, now, now, m.MailFrom, toJSON(m.RcptTo), m.RemoteAddr, m.Helo, m.TLS, m.Size,
		m.FromEmail, m.FromName, m.ToHeader, m.Subject, m.MessageID, toJSON(m.Attachments), toJSON(m.Notes),
		m.ParseError, m.RawPath, m.Status)
	if err != nil {
		if m.RawPath != "" {
			os.Remove(filepath.Join(s.dir, m.RawPath))
		}
		return nil, false, err
	}
	return &m, false, nil
}

func dedupeKey(sha, mailFrom string, rcpts []string) string {
	sorted := append([]string(nil), rcpts...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(sha + "\x00" + mailFrom + "\x00" + strings.Join(sorted, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (s *Store) writeRaw(id string, at time.Time, raw []byte) (string, error) {
	rel := filepath.Join("spool", at.UTC().Format("2006/01/02"), id+".eml")
	full := filepath.Join(s.dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return "", err
	}
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o640); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return rel, nil
}

// ReadRaw returns the stored message bytes.
func (s *Store) ReadRaw(m *Message) ([]byte, error) {
	if !m.HasRaw() {
		return nil, ErrNotFound
	}
	return os.ReadFile(filepath.Join(s.dir, m.RawPath))
}

const messageColumns = `id, sha256, received_at, last_seen_at, seen_count, mail_from, rcpt_to, remote_addr, helo, tls, size,
	from_email, from_name, to_header, subject, message_id, attachments, notes, parse_error, raw_path, status`

type scanner interface{ Scan(dest ...any) error }

func scanMessage(row scanner, extra ...any) (*Message, error) {
	var m Message
	var received, lastSeen, rcpts, attachments, notes string
	dest := append([]any{&m.ID, &m.SHA256, &received, &lastSeen, &m.SeenCount, &m.MailFrom, &rcpts, &m.RemoteAddr, &m.Helo,
		&m.TLS, &m.Size, &m.FromEmail, &m.FromName, &m.ToHeader, &m.Subject, &m.MessageID, &attachments, &notes,
		&m.ParseError, &m.RawPath, &m.Status}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	m.ReceivedAt = parseTime(received)
	m.LastSeenAt = parseTime(lastSeen)
	_ = json.Unmarshal([]byte(rcpts), &m.RcptTo)
	_ = json.Unmarshal([]byte(attachments), &m.Attachments)
	_ = json.Unmarshal([]byte(notes), &m.Notes)
	return &m, nil
}

func (s *Store) GetMessage(id string) (*Message, error) {
	m, err := scanMessage(s.db.QueryRow(`SELECT `+messageColumns+` FROM messages WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// SetNotes records what was normalised while the payload was built.
func (s *Store) SetNotes(id string, notes []string) error {
	_, err := s.db.Exec(`UPDATE messages SET notes = ? WHERE id = ?`, toJSON(notes), id)
	return err
}

// MessageFilter narrows a message list. Before is the id of the last row of
// the previous page.
type MessageFilter struct {
	Status  string
	From    string
	To      string
	Subject string
	Since   time.Time
	Until   time.Time
	Before  string
	Limit   int
}

// MessageRow is a message with the summary of its latest delivery.
type MessageRow struct {
	Message
	Attempts       int
	LastStatusCode int
}

// ListMessages returns the newest messages first, and whether older ones remain.
func (s *Store) ListMessages(f MessageFilter) ([]MessageRow, bool, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	where := []string{"1 = 1"}
	var args []any
	add := func(clause string, a ...any) {
		where = append(where, clause)
		args = append(args, a...)
	}
	if f.Status != "" {
		add("m.status = ?", f.Status)
	}
	if f.From != "" {
		add(`(m.from_email LIKE ? ESCAPE '\' OR m.mail_from LIKE ? ESCAPE '\' OR m.from_name LIKE ? ESCAPE '\')`, like(f.From), like(f.From), like(f.From))
	}
	if f.To != "" {
		add(`(m.rcpt_to LIKE ? ESCAPE '\' OR m.to_header LIKE ? ESCAPE '\')`, like(f.To), like(f.To))
	}
	if f.Subject != "" {
		add(`m.subject LIKE ? ESCAPE '\'`, like(f.Subject))
	}
	if !f.Since.IsZero() {
		add("m.received_at >= ?", formatTime(f.Since))
	}
	if !f.Until.IsZero() {
		add("m.received_at < ?", formatTime(f.Until))
	}
	if f.Before != "" {
		add("m.id < ?", f.Before)
	}
	args = append(args, f.Limit+1)

	rows, err := s.db.Query(`SELECT `+prefixed("m", messageColumns)+`,
			COALESCE((SELECT d.attempt FROM deliveries d WHERE d.message_id = m.id ORDER BY d.id DESC LIMIT 1), 0),
			COALESCE((SELECT d.last_status_code FROM deliveries d WHERE d.message_id = m.id ORDER BY d.id DESC LIMIT 1), 0)
		FROM messages m WHERE `+strings.Join(where, " AND ")+` ORDER BY m.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		var row MessageRow
		m, err := scanMessage(rows, &row.Attempts, &row.LastStatusCode)
		if err != nil {
			return nil, false, err
		}
		row.Message = *m
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > f.Limit
	if more {
		out = out[:f.Limit]
	}
	return out, more, nil
}

func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

const deliveryColumns = `id, message_id, recipient, trigger, event, url, attempt, status, last_status_code, last_error,
	response_excerpt, next_attempt_at, delivered_at, created_at, updated_at`

func scanDelivery(row scanner) (*Delivery, error) {
	var d Delivery
	var next, delivered sql.NullString
	var created, updated string
	if err := row.Scan(&d.ID, &d.MessageID, &d.Recipient, &d.Trigger, &d.Event, &d.URL, &d.Attempt, &d.Status,
		&d.LastStatusCode, &d.LastError, &d.ResponseExcerpt, &next, &delivered, &created, &updated); err != nil {
		return nil, err
	}
	d.NextAttemptAt = scanTime(next)
	d.DeliveredAt = scanTime(delivered)
	d.CreatedAt = parseTime(created)
	d.UpdatedAt = parseTime(updated)
	return &d, nil
}

func (s *Store) queryDeliveries(query string, args ...any) ([]Delivery, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// DeliveriesFor lists a message's deliveries, oldest first.
func (s *Store) DeliveriesFor(messageID string) ([]Delivery, error) {
	return s.queryDeliveries(`SELECT `+deliveryColumns+` FROM deliveries WHERE message_id = ? ORDER BY id`, messageID)
}

func (s *Store) GetDelivery(id string) (*Delivery, error) {
	d, err := scanDelivery(s.db.QueryRow(`SELECT `+deliveryColumns+` FROM deliveries WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

// Delivered reports whether this recipient already has the message, through
// any delivery. A sender's retry then has nothing left to do for it.
func (s *Store) Delivered(messageID, recipient string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE message_id = ? AND recipient = ? AND status = ?`,
		messageID, recipient, StatusDelivered).Scan(&n)
	return n > 0, err
}

// SMTPDelivery returns the delivery an SMTP transaction works on: the one a
// previous attempt of the same message left unfinished, or a new one. Keeping
// the id across the sender's retries is what lets a receiver deduplicate.
func (s *Store) SMTPDelivery(messageID, recipient, event, url string, now time.Time) (*Delivery, error) {
	d, err := scanDelivery(s.db.QueryRow(`SELECT `+deliveryColumns+` FROM deliveries
		WHERE message_id = ? AND recipient = ? AND trigger = ? AND status != ? ORDER BY id DESC LIMIT 1`,
		messageID, recipient, TriggerSMTP, StatusDelivered))
	if err == nil {
		return d, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return s.createDelivery(messageID, recipient, TriggerSMTP, event, url, now, nil)
}

// CreateResend queues a new delivery of a stored message, due at once.
func (s *Store) CreateResend(messageID, recipient, event, url string, now time.Time) (*Delivery, error) {
	return s.createDelivery(messageID, recipient, TriggerResend, event, url, now, &now)
}

func (s *Store) createDelivery(messageID, recipient, trigger, event, url string, now time.Time, due *time.Time) (*Delivery, error) {
	d := &Delivery{
		ID: newID("whd"), MessageID: messageID, Recipient: recipient, Trigger: trigger, Event: event, URL: url,
		Status: StatusPending, NextAttemptAt: due, CreatedAt: now, UpdatedAt: now,
	}
	_, err := s.db.Exec(`INSERT INTO deliveries (id, message_id, recipient, trigger, event, url, status, next_attempt_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, messageID, recipient, trigger, event, url, d.Status, nullTime(due), formatTime(now), formatTime(now))
	if err != nil {
		return nil, err
	}
	return d, s.refreshStatus(messageID)
}

// RecordAttempt writes one attempt and the state it leaves the delivery in.
func (s *Store) RecordAttempt(deliveryID string, a Attempt, status, excerpt string, next *time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var messageID string
	if err := tx.QueryRow(`SELECT message_id FROM deliveries WHERE id = ?`, deliveryID).Scan(&messageID); err != nil {
		return err
	}
	var delivered any
	if status == StatusDelivered {
		delivered = formatTime(a.At)
	}
	if _, err := tx.Exec(`UPDATE deliveries SET attempt = ?, status = ?, last_status_code = ?, last_error = ?, response_excerpt = ?,
			next_attempt_at = ?, delivered_at = COALESCE(?, delivered_at), updated_at = ? WHERE id = ?`,
		a.Attempt, status, a.StatusCode, a.Error, excerpt, nullTime(next), delivered, formatTime(a.At), deliveryID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO delivery_attempts (delivery_id, attempt, at, status_code, error, duration_ms) VALUES (?, ?, ?, ?, ?, ?)`,
		deliveryID, a.Attempt, formatTime(a.At), a.StatusCode, a.Error, a.DurationMS); err != nil {
		return err
	}
	if err := refreshStatusTx(tx, messageID); err != nil {
		return err
	}
	return tx.Commit()
}

// AttemptsFor lists the attempts of a delivery, oldest first.
func (s *Store) AttemptsFor(deliveryID string) ([]Attempt, error) {
	rows, err := s.db.Query(`SELECT attempt, at, status_code, error, duration_ms FROM delivery_attempts WHERE delivery_id = ? ORDER BY id`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var at string
		if err := rows.Scan(&a.Attempt, &at, &a.StatusCode, &a.Error, &a.DurationMS); err != nil {
			return nil, err
		}
		a.At = parseTime(at)
		out = append(out, a)
	}
	return out, rows.Err()
}

// DueDeliveries returns queued deliveries whose time has come. A message is
// worked on one delivery at a time, oldest first.
func (s *Store) DueDeliveries(now time.Time, limit int) ([]Delivery, error) {
	return s.queryDeliveries(`SELECT `+deliveryColumns+` FROM deliveries d
		WHERE d.status = ? AND d.next_attempt_at IS NOT NULL AND d.next_attempt_at <= ?
		  AND NOT EXISTS (SELECT 1 FROM deliveries o WHERE o.message_id = d.message_id AND o.status = ?
		                  AND o.next_attempt_at IS NOT NULL AND o.id < d.id)
		ORDER BY d.next_attempt_at LIMIT ?`, StatusPending, formatTime(now), StatusPending, limit)
}

// RecoverInterrupted settles deliveries a crash left mid-attempt. Their SMTP
// conversation is gone, and the sender, never having seen a 250, will retry.
func (s *Store) RecoverInterrupted(now time.Time) (int, error) {
	rows, err := s.db.Query(`SELECT DISTINCT message_id FROM deliveries WHERE status = ? AND next_attempt_at IS NULL`, StatusPending)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	res, err := s.db.Exec(`UPDATE deliveries SET status = ?, last_error = ?, updated_at = ? WHERE status = ? AND next_attempt_at IS NULL`,
		StatusDeferred, "interrupted by a restart; the sending server will retry", formatTime(now), StatusPending)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.refreshStatus(id); err != nil {
			return 0, err
		}
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

func (s *Store) refreshStatus(messageID string) error { return refreshStatusTx(s.db, messageID) }

// refreshStatusTx sets a message's status to the worst state among the latest
// delivery of each of its recipients.
func refreshStatusTx(db execer, messageID string) error {
	rows, err := db.Query(`SELECT recipient, status FROM deliveries WHERE message_id = ? ORDER BY id`, messageID)
	if err != nil {
		return err
	}
	latest := map[string]string{}
	for rows.Next() {
		var recipient, status string
		if err := rows.Scan(&recipient, &status); err != nil {
			rows.Close()
			return err
		}
		latest[recipient] = status
	}
	if err := rows.Close(); err != nil {
		return err
	}

	rank := map[string]int{StatusDelivered: 1, StatusPending: 2, StatusDeferred: 3, StatusFailed: 4}
	status := StatusPending
	if len(latest) > 0 {
		status = StatusDelivered
		for _, st := range latest {
			if rank[st] > rank[status] {
				status = st
			}
		}
	}
	_, err = db.Exec(`UPDATE messages SET status = ? WHERE id = ?`, status, messageID)
	return err
}

// SetMessageStatus is for a message that ended before any delivery existed.
func (s *Store) SetMessageStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE messages SET status = ? WHERE id = ?`, status, id)
	return err
}

// Stats are the dashboard's figures over a period.
type Stats struct {
	Received  int
	Delivered int
	Deferred  int
	Failed    int
	Pending   int
	Refused   int
}

func (s *Store) StatsSince(since time.Time) (Stats, error) {
	var st Stats
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM messages WHERE received_at >= ? GROUP BY status`, formatTime(since))
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return st, err
		}
		st.Received += n
		switch status {
		case StatusDelivered:
			st.Delivered = n
		case StatusDeferred:
			st.Deferred = n
		case StatusFailed:
			st.Failed = n
		case StatusPending:
			st.Pending = n
		}
	}
	if err := rows.Close(); err != nil {
		return st, err
	}
	var refused sql.NullInt64
	err = s.db.QueryRow(`SELECT SUM(count) FROM refusals WHERE last_at >= ?`, formatTime(since)).Scan(&refused)
	st.Refused = int(refused.Int64)
	return st, err
}

// HasPending reports whether any delivery is running or queued, which is what
// makes a page keep polling.
func (s *Store) HasPending() bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE status = ?`, StatusPending).Scan(&n)
	return n > 0
}

// Purge removes everything last seen before the cutoff: messages with their
// raw files and deliveries, refusals, and unknown-address answers.
func (s *Store) Purge(before time.Time) (int, error) {
	cutoff := formatTime(before)
	rows, err := s.db.Query(`SELECT id, raw_path FROM messages WHERE last_seen_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	type victim struct{ id, path string }
	var victims []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.path); err != nil {
			rows.Close()
			return 0, err
		}
		victims = append(victims, v)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, v := range victims {
		if v.path != "" {
			os.Remove(filepath.Join(s.dir, v.path))
		}
		if _, err := s.db.Exec(`DELETE FROM messages WHERE id = ?`, v.id); err != nil {
			return 0, err
		}
	}
	for _, q := range []string{
		`DELETE FROM refusals WHERE last_at < ?`,
		`DELETE FROM sink_receipts WHERE received_at < ?`,
		`DELETE FROM recipients WHERE status = 'unknown' AND checked_at < ?`,
	} {
		if _, err := s.db.Exec(q, cutoff); err != nil {
			return len(victims), err
		}
	}
	return len(victims), nil
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return "[]"
	}
	return string(b)
}

// ShortID is the tail of an id, enough to tell rows apart on a page.
func ShortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}
