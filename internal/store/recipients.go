package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Recipient is one entry of the known-address list.
type Recipient struct {
	Address     string
	Status      string
	FirstSeenAt time.Time
	CheckedAt   time.Time
	LastMailAt  *time.Time
	Accepted    int
	Refused     int
}

// GetRecipient returns nil when the address has never been checked.
func (s *Store) GetRecipient(address string) (*Recipient, error) {
	r, err := scanRecipient(s.db.QueryRow(`SELECT address, status, first_seen_at, checked_at, last_mail_at, accepted, refused
		FROM recipients WHERE address = ?`, address))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func scanRecipient(row scanner) (*Recipient, error) {
	var r Recipient
	var first, checked string
	var last sql.NullString
	if err := row.Scan(&r.Address, &r.Status, &first, &checked, &last, &r.Accepted, &r.Refused); err != nil {
		return nil, err
	}
	r.FirstSeenAt = parseTime(first)
	r.CheckedAt = parseTime(checked)
	r.LastMailAt = scanTime(last)
	return &r, nil
}

// PutRecipient records the answer of a check.
func (s *Store) PutRecipient(address, status string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO recipients (address, status, first_seen_at, checked_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (address) DO UPDATE SET status = excluded.status, checked_at = excluded.checked_at`,
		address, status, formatTime(at), formatTime(at))
	return err
}

// CountRecipient counts a mail accepted for, or refused to, a listed address.
func (s *Store) CountRecipient(address string, accepted bool, at time.Time) error {
	if accepted {
		_, err := s.db.Exec(`UPDATE recipients SET accepted = accepted + 1, last_mail_at = ? WHERE address = ?`, formatTime(at), address)
		return err
	}
	_, err := s.db.Exec(`UPDATE recipients SET refused = refused + 1 WHERE address = ?`, address)
	return err
}

func (s *Store) ForgetRecipient(address string) error {
	_, err := s.db.Exec(`DELETE FROM recipients WHERE address = ?`, address)
	return err
}

type RecipientFilter struct {
	Status string
	Query  string
	Limit  int
}

// ListRecipients returns the most recently checked addresses first.
func (s *Store) ListRecipients(f RecipientFilter) ([]Recipient, error) {
	if f.Limit <= 0 {
		f.Limit = 200
	}
	where := []string{"1 = 1"}
	var args []any
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.Query != "" {
		where = append(where, `address LIKE ? ESCAPE '\'`)
		args = append(args, like(f.Query))
	}
	args = append(args, f.Limit)
	rows, err := s.db.Query(`SELECT address, status, first_seen_at, checked_at, last_mail_at, accepted, refused
		FROM recipients WHERE `+strings.Join(where, " AND ")+` ORDER BY checked_at DESC, address LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recipient
	for rows.Next() {
		r, err := scanRecipient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// Refusal reasons.
const (
	ReasonUnknownRecipient = "unknown_recipient"
	ReasonDomainNotAllowed = "domain_not_allowed"
	ReasonTooManyBad       = "too_many_bad_recipients"
	ReasonLookupFailed     = "lookup_unavailable"
	ReasonConnectionLimit  = "connection_limit"
	ReasonBlocked          = "blocked"
)

// Refusal is a mail that was turned away before its content was sent, so the
// attempt is all there is to keep.
type Refusal struct {
	ID         int64
	RemoteAddr string
	Helo       string
	MailFrom   string
	Rcpt       string
	Reason     string
	Count      int
	FirstAt    time.Time
	LastAt     time.Time
}

// RecordRefusal counts repeats within the same hour on one row: a guessing
// run produces thousands of identical attempts.
func (s *Store) RecordRefusal(r Refusal, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO refusals (bucket, remote_addr, helo, mail_from, rcpt, reason, first_at, last_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (bucket, remote_addr, mail_from, rcpt, reason)
		DO UPDATE SET count = count + 1, last_at = excluded.last_at, helo = excluded.helo`,
		at.UTC().Format("2006-01-02T15"), r.RemoteAddr, r.Helo, r.MailFrom, r.Rcpt, r.Reason, formatTime(at), formatTime(at))
	return err
}

type RefusalFilter struct {
	Reason string
	Query  string // matches the recipient, the sender or the address
	Before int64
	Limit  int
}

// ListRefusals returns the latest refusals first, and whether older ones remain.
func (s *Store) ListRefusals(f RefusalFilter) ([]Refusal, bool, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	where := []string{"1 = 1"}
	var args []any
	if f.Reason != "" {
		where = append(where, "reason = ?")
		args = append(args, f.Reason)
	}
	if f.Query != "" {
		where = append(where, `(rcpt LIKE ? ESCAPE '\' OR mail_from LIKE ? ESCAPE '\' OR remote_addr LIKE ? ESCAPE '\')`)
		args = append(args, like(f.Query), like(f.Query), like(f.Query))
	}
	if f.Before > 0 {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}
	args = append(args, f.Limit+1)
	rows, err := s.db.Query(`SELECT id, remote_addr, helo, mail_from, rcpt, reason, count, first_at, last_at
		FROM refusals WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Refusal
	for rows.Next() {
		var r Refusal
		var first, last string
		if err := rows.Scan(&r.ID, &r.RemoteAddr, &r.Helo, &r.MailFrom, &r.Rcpt, &r.Reason, &r.Count, &first, &last); err != nil {
			return nil, false, err
		}
		r.FirstAt = parseTime(first)
		r.LastAt = parseTime(last)
		out = append(out, r)
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

// SinkReceipt is a webhook the built-in test receiver got.
type SinkReceipt struct {
	ReceivedAt      time.Time
	DeliveryID      string
	Event           string
	SignatureStatus string
	Body            string
}

func (s *Store) AddSinkReceipt(r SinkReceipt) error {
	_, err := s.db.Exec(`INSERT INTO sink_receipts (received_at, delivery_id, event, signature_status, body) VALUES (?, ?, ?, ?, ?)`,
		formatTime(r.ReceivedAt), r.DeliveryID, r.Event, r.SignatureStatus, r.Body)
	return err
}

// SinkReceiptsFor returns what the sink received for these deliveries, newest first.
func (s *Store) SinkReceiptsFor(deliveryIDs []string) ([]SinkReceipt, error) {
	if len(deliveryIDs) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(deliveryIDs)), ",")
	args := make([]any, len(deliveryIDs))
	for i, id := range deliveryIDs {
		args[i] = id
	}
	rows, err := s.db.Query(`SELECT received_at, delivery_id, event, signature_status, body FROM sink_receipts
		WHERE delivery_id IN (`+marks+`) ORDER BY id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SinkReceipt
	for rows.Next() {
		var r SinkReceipt
		var at string
		if err := rows.Scan(&at, &r.DeliveryID, &r.Event, &r.SignatureStatus, &r.Body); err != nil {
			return nil, err
		}
		r.ReceivedAt = parseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}
