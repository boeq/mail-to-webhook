// Package smtpsrv wires the SMTP conversation to the recipient check, the
// history and the webhook.
//
// Delivery is synchronous with the DATA command: the transaction does not
// complete until the webhook has answered. If the endpoint is down the sender
// gets a 451 and keeps the mail in its own queue, which every mail server
// already has and tends for days.
package smtpsrv

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/boeq/mail-to-webhook/internal/config"
	"github.com/boeq/mail-to-webhook/internal/delivery"
	"github.com/boeq/mail-to-webhook/internal/limits"
	"github.com/boeq/mail-to-webhook/internal/mailparse"
	"github.com/boeq/mail-to-webhook/internal/payload"
	"github.com/boeq/mail-to-webhook/internal/recipients"
	"github.com/boeq/mail-to-webhook/internal/store"
	"github.com/boeq/mail-to-webhook/internal/webhook"
)

type Backend struct {
	Cfg     *config.Config
	Engine  *delivery.Engine
	Store   *store.Store // nil runs without history
	Checker *recipients.Checker
	Limiter *limits.Limiter
	Log     *slog.Logger
	Now     func() time.Time
}

// NewServer returns a go-smtp server set up from the configuration.
func NewServer(b *Backend) (*smtp.Server, error) {
	srv := smtp.NewServer(b)
	srv.Addr = b.Cfg.SMTPAddr
	srv.Domain = b.Cfg.SMTPDomain
	srv.MaxMessageBytes = b.Cfg.MaxMessageBytes
	srv.MaxRecipients = b.Cfg.MaxRecipients
	srv.MaxLineLength = b.Cfg.MaxLineBytes
	srv.ReadTimeout = b.Cfg.ReadTimeout
	srv.WriteTimeout = b.Cfg.WriteTimeout
	// Without SMTPUTF8 a sender with an internationalised address cannot
	// deliver at all. BINARYMIME stays off: senders encode instead, and
	// nothing is gained by taking unencoded binary.
	srv.EnableSMTPUTF8 = true
	srv.ErrorLog = logAdapter{b.Log}

	if b.Cfg.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(b.Cfg.TLSCert, b.Cfg.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("TLS_CERT/TLS_KEY: %w", err)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	return srv, nil
}

// Listen opens the SMTP port behind the connection limits.
func Listen(b *Backend) (net.Listener, error) {
	l, err := net.Listen("tcp", b.Cfg.SMTPAddr)
	if err != nil {
		return nil, err
	}
	return &limits.Listener{
		Listener: l,
		Limiter:  b.Limiter,
		OnRefuse: func(ip, reason string) {
			b.Log.Info("connection refused", "remote", ip, "reason", reason)
			b.recordRefusal(store.Refusal{RemoteAddr: ip, Reason: reason})
		},
	}, nil
}

type logAdapter struct{ log *slog.Logger }

func (l logAdapter) Printf(format string, v ...any) { l.log.Warn(fmt.Sprintf(format, v...)) }
func (l logAdapter) Println(v ...any)               { l.log.Warn(strings.TrimSpace(fmt.Sprintln(v...))) }

func (b *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &session{b: b, conn: c, id: store.ShortID(store.NewDeliveryID())}, nil
}

func (b *Backend) recordRefusal(r store.Refusal) {
	if b.Store == nil {
		return
	}
	if err := b.Store.RecordRefusal(r, b.Now()); err != nil {
		b.Log.Error("could not record the refusal", "err", err)
	}
}

type session struct {
	b       *Backend
	conn    *smtp.Conn
	id      string
	from    string
	rcpts   []string
	bad     int  // unknown recipients offered on this connection
	dropped bool // the connection is being closed for offering too many
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if s.dropped {
		return tooManyBadRecipients()
	}
	rcpt := NormaliseRecipient(to)

	// We are a final destination for the configured domains only. Without
	// this the server is an open relay target, and it is found within hours
	// of an MX record pointing at it.
	if !s.b.accepts(rcpt) {
		s.refuse(rcpt, store.ReasonDomainNotAllowed)
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Relay access denied"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.b.Cfg.RecipientCheckTimeout+time.Second)
	defer cancel()
	switch s.b.Checker.Check(ctx, rcpt) {
	case recipients.Unavailable:
		// The consumer cannot be asked, which also means it could not take
		// the mail right now. The sender keeps it and tries again.
		s.refuse(rcpt, store.ReasonLookupFailed)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 4, 3}, Message: "Recipient check unavailable, try again later"}
	case recipients.Unknown:
		s.bad++
		blocked := s.b.Limiter.BadRecipient(s.remoteIP())
		if s.b.Store != nil {
			_ = s.b.Store.CountRecipient(rcpt, false, s.b.Now())
		}
		if blocked || (s.b.Cfg.MaxBadRecipients > 0 && s.bad > s.b.Cfg.MaxBadRecipients) {
			s.refuse(rcpt, store.ReasonTooManyBad)
			s.drop()
			return tooManyBadRecipients()
		}
		s.refuse(rcpt, store.ReasonUnknownRecipient)
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "No such mailbox here"}
	}

	s.rcpts = append(s.rcpts, rcpt)
	return nil
}

func tooManyBadRecipients() error {
	return &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Too many unknown recipients, closing connection"}
}

// drop closes the connection shortly after the 421 has gone out.
func (s *session) drop() {
	s.dropped = true
	time.AfterFunc(250*time.Millisecond, func() { _ = s.conn.Close() })
}

func (s *session) refuse(rcpt, reason string) {
	s.b.Log.Info("recipient refused", "session", s.id, "rcpt", rcpt, "from", s.from, "remote", s.remoteIP(), "reason", reason)
	s.b.recordRefusal(store.Refusal{
		RemoteAddr: s.remoteIP(),
		Helo:       s.conn.Hostname(),
		MailFrom:   s.from,
		Rcpt:       rcpt,
		Reason:     reason,
	})
}

func (s *session) Data(r io.Reader) error {
	start := s.b.Now()
	raw, serr := s.read(r)
	if serr != nil {
		return serr
	}

	msg, perr := s.b.Engine.Parse(raw)
	if perr != nil {
		// Deliver what could be read rather than bounce: the message goes
		// out with the whole original attached.
		s.b.Log.Warn("message only partly parsed", "session", s.id, "err", perr)
	}

	_, tlsOK := s.conn.TLSConnectionState()
	env := payload.Envelope{
		MailFrom:   s.from,
		RcptTo:     append([]string(nil), s.rcpts...),
		RemoteAddr: s.remoteIP(),
		Helo:       s.conn.Hostname(),
		TLS:        tlsOK,
		ReceivedAt: start.UTC().Truncate(time.Millisecond),
		Size:       len(raw),
	}

	stored := s.record(msg, perr, env, raw)
	if stored != nil {
		// A retry is sent with the time of the first receipt, so its payload
		// is the same bytes as before.
		env.ReceivedAt = stored.ReceivedAt
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		s.b.Cfg.WebhookTimeout*time.Duration(s.b.Cfg.WebhookRetries+1)+30*time.Second)
	defer cancel()

	for _, rcpt := range s.b.Engine.Targets(s.rcpts) {
		if err := s.deliver(ctx, stored, msg, perr, env, rcpt); err != nil {
			return err
		}
	}

	if s.b.Store != nil {
		for _, rcpt := range s.rcpts {
			_ = s.b.Store.CountRecipient(rcpt, true, s.b.Now())
		}
	}
	s.b.Log.Info("message accepted", "session", s.id, "from", s.from, "rcpts", len(s.rcpts),
		"bytes", len(raw), "took", s.b.Now().Sub(start).String())
	return nil
}

// read takes the message off the wire and maps what can go wrong to a reply.
func (s *session) read(r io.Reader) ([]byte, error) {
	limit := s.b.Cfg.MaxMessageBytes
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	switch {
	case errors.Is(err, smtp.ErrTooLongLine):
		// A line over the limit will never succeed on retry, so it must not
		// be reported as transient or the sender loops until its queue expires.
		s.b.Log.Warn("line length exceeded", "session", s.id, "remote", s.remoteIP())
		return nil, &smtp.SMTPError{Code: 500, EnhancedCode: smtp.EnhancedCode{5, 5, 6}, Message: "Line too long"}
	case errors.Is(err, smtp.ErrDataTooLarge):
		return nil, tooLarge("Message too large")
	case err != nil:
		s.b.Log.Warn("read failed", "session", s.id, "err", err)
		return nil, tempError("Failed to read message")
	case limit > 0 && int64(len(raw)) > limit:
		return nil, tooLarge("Message too large")
	}
	return raw, nil
}

// record writes the message to the history. A failure here is logged and the
// mail goes on without a record.
func (s *session) record(msg *mailparse.Message, perr error, env payload.Envelope, raw []byte) *store.Message {
	if s.b.Store == nil {
		return nil
	}
	sum := payload.Summarise(msg)
	row := store.Message{
		ReceivedAt: env.ReceivedAt,
		MailFrom:   env.MailFrom,
		RcptTo:     env.RcptTo,
		RemoteAddr: env.RemoteAddr,
		Helo:       env.Helo,
		TLS:        env.TLS,
		Size:       env.Size,
		FromEmail:  sum.FromEmail,
		FromName:   sum.FromName,
		ToHeader:   sum.To,
		Subject:    sum.Subject,
		MessageID:  sum.MessageID,
	}
	for _, a := range sum.Attachments {
		row.Attachments = append(row.Attachments, store.AttachmentInfo{Name: a.Name, ContentType: a.ContentType, Size: a.Size})
	}
	if perr != nil {
		row.ParseError = perr.Error()
	}
	stored, again, err := s.b.Store.RecordMessage(row, raw)
	if err != nil {
		s.b.Log.Error("could not record the message; delivering without history", "session", s.id, "err", err)
		return nil
	}
	if again {
		s.b.Log.Info("sender retry of a known message", "session", s.id, "message", stored.ID, "seen", stored.SeenCount)
	}
	return stored
}

// deliver takes the message to the webhook for one target and returns the
// SMTP error to answer with, or nil.
func (s *session) deliver(ctx context.Context, stored *store.Message, msg *mailparse.Message, perr error, env payload.Envelope, rcpt string) error {
	var d *store.Delivery
	if stored != nil {
		if done, err := s.b.Store.Delivered(stored.ID, rcpt); err == nil && done {
			// The webhook has this one; the sender just never saw our 250, or
			// another recipient of the same mail failed last time.
			s.b.Log.Info("already delivered, nothing to do", "session", s.id, "message", stored.ID, "rcpt", rcpt)
			return nil
		}
		var err error
		d, err = s.b.Store.SMTPDelivery(stored.ID, rcpt, webhook.EventReceived, s.b.Engine.Webhook.URL(), s.b.Now())
		if err != nil {
			s.b.Log.Error("could not record the delivery; continuing without", "session", s.id, "err", err)
			d = nil
		}
	}

	p, body, err := s.b.Engine.Build(msg, env, rcpt)
	if p != nil && stored != nil && len(p.Notes) > 0 {
		_ = s.b.Store.SetNotes(stored.ID, p.Notes)
	}
	switch {
	case errors.Is(err, payload.ErrAttachmentTooLarge):
		s.b.Log.Warn("rejecting oversize attachment", "session", s.id, "rcpt", rcpt, "err", err)
		s.b.Engine.Fail(d, err.Error())
		return tooLarge("Attachment too large")
	case errors.Is(err, delivery.ErrIncompatible):
		s.b.Engine.Defer(d, err.Error())
		return tempError("Message could not be normalised for the delivery endpoint")
	case err != nil:
		s.b.Log.Error("payload build failed", "session", s.id, "rcpt", rcpt, "err", err)
		s.b.Engine.Defer(d, "payload build failed: "+err.Error())
		return tempError("Internal error")
	}
	for _, n := range p.Notes {
		s.b.Log.Info("normalised", "session", s.id, "rcpt", rcpt, "note", n)
	}

	res := s.b.Engine.DeliverNow(ctx, d, body)
	logArgs := []any{"session", s.id, "rcpt", rcpt, "from", s.from, "bytes", len(body),
		"attachments", len(p.Attachments), "status_code", res.StatusCode}
	if d != nil {
		logArgs = append(logArgs, "delivery", d.ID, "message", d.MessageID)
	}
	switch res.Outcome {
	case webhook.Permanent:
		s.b.Log.Error("delivery refused by the endpoint", append(logArgs, "err", res.Error, "response", res.Excerpt)...)
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Message rejected by delivery endpoint"}
	case webhook.Transient:
		s.b.Log.Error("delivery failed, deferring to the sender", append(logArgs, "err", res.Error, "response", res.Excerpt)...)
		return tempError("Delivery endpoint unavailable, try again later")
	}
	s.b.Log.Info("delivered", logArgs...)
	return nil
}

func (s *session) Reset()        { s.from, s.rcpts = "", nil }
func (s *session) Logout() error { return nil }

func (s *session) remoteIP() string {
	if s.conn == nil || s.conn.Conn() == nil {
		return ""
	}
	return limits.HostOf(s.conn.Conn().RemoteAddr())
}

// accepts implements the relay policy.
func (b *Backend) accepts(addr string) bool {
	local, domain, ok := strings.Cut(strings.ToLower(addr), "@")
	if !ok || local == "" || domain == "" {
		return false
	}
	if len(b.Cfg.AllowedDomains) > 0 && !contains(b.Cfg.AllowedDomains, domain) {
		return false
	}
	if len(b.Cfg.AllowedMailboxes) > 0 {
		base, _, _ := strings.Cut(local, "+") // ignore the plus-address hash
		if !contains(b.Cfg.AllowedMailboxes, base) {
			return false
		}
	}
	return true
}

// NormaliseRecipient lower-cases the domain, which is case-insensitive, and
// leaves the local part alone, which may not be.
func NormaliseRecipient(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return addr
	}
	return addr[:at+1] + strings.ToLower(addr[at+1:])
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func tempError(msg string) error {
	return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: msg}
}

func tooLarge(msg string) error {
	return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: msg}
}
