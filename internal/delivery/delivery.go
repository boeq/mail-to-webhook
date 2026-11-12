// Package delivery takes a message to the webhook and records how it went.
//
// There are two ways in. During an SMTP conversation a delivery is attempted
// at once, a few times in a row, and the result decides the SMTP reply: the
// sending server keeps the mail until the webhook has accepted it. A delivery
// queued afterwards (a resend from the UI) has no sender waiting, so it is
// retried from here on a schedule.
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/boeq/mail-to-webhook/internal/config"
	"github.com/boeq/mail-to-webhook/internal/consumer"
	"github.com/boeq/mail-to-webhook/internal/mailparse"
	"github.com/boeq/mail-to-webhook/internal/payload"
	"github.com/boeq/mail-to-webhook/internal/store"
	"github.com/boeq/mail-to-webhook/internal/webhook"
)

// RetryDelays are the waits after attempts 1 to 5 of a queued delivery. A
// higher attempt limit keeps using the last one.
var RetryDelays = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour}

var (
	// ErrNoRaw: the raw message is not stored (any more), so there is nothing to send again.
	ErrNoRaw = errors.New("the raw message is no longer stored")
	// ErrIncompatible: the payload would fail the consumer's validation.
	ErrIncompatible = errors.New("payload fails the consumer's rules")
)

type Engine struct {
	Cfg     *config.Config
	Store   *store.Store // nil runs without history
	Webhook *webhook.Client
	Log     *slog.Logger

	Now   func() time.Time
	Sleep func(context.Context, time.Duration)
}

func New(cfg *config.Config, st *store.Store, wh *webhook.Client, log *slog.Logger) *Engine {
	return &Engine{Cfg: cfg, Store: st, Webhook: wh, Log: log, Now: time.Now, Sleep: sleep}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Parse reads a raw message with the configured options.
func (e *Engine) Parse(raw []byte) (*mailparse.Message, error) {
	return mailparse.ParseBytes(raw, mailparse.Options{
		TextFromHTML:           e.Cfg.TextFromHTML,
		ExpandAttachedMessages: e.Cfg.ExpandAttachedMessages,
		ExpandTNEF:             e.Cfg.ExpandTNEF,
	})
}

// Targets are the envelope recipients that each get a POST.
func (e *Engine) Targets(rcpts []string) []string {
	if e.Cfg.PerRecipient || len(rcpts) <= 1 {
		return rcpts
	}
	return rcpts[:1]
}

// Build renders the payload for one target. The same message, envelope and
// settings always give the same bytes, which is what makes a retry or a
// resend identical to the first attempt.
func (e *Engine) Build(msg *mailparse.Message, env payload.Envelope, recipient string) (*payload.Inbound, []byte, error) {
	opt := payload.Options{
		IncludeRaw:            e.Cfg.IncludeRawEmail,
		IncludeAttachments:    e.Cfg.IncludeAttachments,
		MaxAttachBytes:        e.Cfg.MaxAttachBytes,
		FallbackSender:        e.Cfg.FallbackSender,
		OversizePolicy:        e.Cfg.OversizePolicy,
		DedupeAttachmentNames: e.Cfg.DedupeAttachNames,
		EnsureRecipientInTo:   e.Cfg.EnsureRecipientInTo,
		SynthesizeMessageID:   e.Cfg.SynthesizeMessageID,
		Domain:                e.Cfg.SMTPDomain,
	}
	if !e.Cfg.PerRecipient {
		opt.AllRecipients = env.RcptTo
	}
	p, err := payload.Build(msg, env, recipient, opt)
	if err != nil {
		return nil, nil, err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return nil, nil, err
	}
	// Posting something the endpoint answers with 422 costs the message: 422
	// is a permanent failure and the sender bounces it. Catching it here
	// turns that into a deferral instead.
	if violations := consumer.Validate(body); len(violations) > 0 {
		e.Log.Error("payload would fail consumer validation", "recipient", recipient, "violations", consumer.Violations(violations))
		if e.Cfg.StrictCompat {
			return p, body, fmt.Errorf("%w: %s", ErrIncompatible, consumer.Violations(violations))
		}
	}
	return p, body, nil
}

// DeliverNow attempts a delivery right away, with a few quick retries, and
// returns the last result. d may be nil when there is no history.
func (e *Engine) DeliverNow(ctx context.Context, d *store.Delivery, body []byte) webhook.Result {
	id, attempt := store.NewDeliveryID(), 0
	if d != nil {
		id, attempt = d.ID, d.Attempt
	}
	var res webhook.Result
	for try := 0; try <= e.Cfg.WebhookRetries; try++ {
		if try > 0 {
			e.Sleep(ctx, backoff(try))
			if ctx.Err() != nil {
				break
			}
		}
		attempt++
		res = e.Webhook.Post(ctx, id, webhook.EventReceived, body)

		status := store.StatusDelivered
		switch {
		case res.Outcome == webhook.Permanent:
			status = store.StatusFailed
		case res.Outcome == webhook.Transient && try < e.Cfg.WebhookRetries:
			status = store.StatusPending
		case res.Outcome == webhook.Transient:
			status = store.StatusDeferred
		}
		e.record(d, attempt, res, status, nil)
		if res.Outcome != webhook.Transient {
			break
		}
	}
	return res
}

// Defer records that a delivery could not even be attempted.
func (e *Engine) Defer(d *store.Delivery, reason string) {
	if d == nil {
		return
	}
	e.record(d, d.Attempt+1, webhook.Result{Outcome: webhook.Transient, Error: reason}, store.StatusDeferred, nil)
}

// Fail records that a delivery will never succeed, without an attempt.
func (e *Engine) Fail(d *store.Delivery, reason string) {
	if d == nil {
		return
	}
	e.record(d, d.Attempt+1, webhook.Result{Outcome: webhook.Permanent, Error: reason}, store.StatusFailed, nil)
}

func (e *Engine) record(d *store.Delivery, attempt int, res webhook.Result, status string, next *time.Time) {
	if d == nil || e.Store == nil {
		return
	}
	d.Attempt, d.Status = attempt, status
	err := e.Store.RecordAttempt(d.ID, store.Attempt{
		Attempt:    attempt,
		At:         e.Now(),
		StatusCode: res.StatusCode,
		Error:      res.Error,
		DurationMS: int(res.Duration / time.Millisecond),
	}, status, res.Excerpt, next)
	if err != nil {
		// The history is a record, not a dependency: mail goes on without it.
		e.Log.Error("could not record the delivery attempt", "delivery", d.ID, "err", err)
	}
}

func backoff(try int) time.Duration {
	base := time.Duration(1<<uint(try-1)) * time.Second
	if base > 8*time.Second {
		base = 8 * time.Second
	}
	return base + time.Duration(rand.Int63n(int64(500*time.Millisecond)))
}

// Resend queues a fresh delivery of a stored message for each of its targets.
func (e *Engine) Resend(messageID string) ([]store.Delivery, error) {
	if e.Store == nil {
		return nil, store.ErrNotFound
	}
	msg, err := e.Store.GetMessage(messageID)
	if err != nil {
		return nil, err
	}
	if !msg.HasRaw() {
		return nil, ErrNoRaw
	}
	var out []store.Delivery
	for _, rcpt := range e.Targets(msg.RcptTo) {
		d, err := e.Store.CreateResend(msg.ID, rcpt, webhook.EventReceived, e.Webhook.URL(), e.Now())
		if err != nil {
			return out, err
		}
		out = append(out, *d)
	}
	return out, nil
}

// Envelope rebuilds the SMTP facts of a stored message.
func Envelope(m *store.Message) payload.Envelope {
	return payload.Envelope{
		MailFrom:   m.MailFrom,
		RcptTo:     m.RcptTo,
		RemoteAddr: m.RemoteAddr,
		Helo:       m.Helo,
		TLS:        m.TLS,
		ReceivedAt: m.ReceivedAt,
		Size:       m.Size,
	}
}

// BodyFor rebuilds the payload of a stored message for one recipient.
func (e *Engine) BodyFor(m *store.Message, recipient string) (*payload.Inbound, []byte, error) {
	raw, err := e.Store.ReadRaw(m)
	if err != nil {
		return nil, nil, ErrNoRaw
	}
	msg, _ := e.Parse(raw)
	return e.Build(msg, Envelope(m), recipient)
}

// Sweep makes one attempt at every queued delivery that is due.
func (e *Engine) Sweep(ctx context.Context) int {
	if e.Store == nil {
		return 0
	}
	due, err := e.Store.DueDeliveries(e.Now(), 20)
	if err != nil {
		e.Log.Error("could not read the due deliveries", "err", err)
		return 0
	}
	for i := range due {
		if ctx.Err() != nil {
			return i
		}
		e.attemptQueued(ctx, &due[i])
	}
	return len(due)
}

func (e *Engine) attemptQueued(ctx context.Context, d *store.Delivery) {
	attempt := d.Attempt + 1
	msg, err := e.Store.GetMessage(d.MessageID)
	if err != nil {
		e.record(d, attempt, webhook.Result{Outcome: webhook.Permanent, Error: "the message is gone"}, store.StatusFailed, nil)
		return
	}
	_, body, err := e.BodyFor(msg, d.Recipient)
	if err != nil {
		// Neither a missing raw file nor a payload that cannot be built gets
		// better by waiting.
		e.record(d, attempt, webhook.Result{Outcome: webhook.Permanent, Error: err.Error()}, store.StatusFailed, nil)
		return
	}

	res := e.Webhook.Post(ctx, d.ID, d.Event, body)
	switch res.Outcome {
	case webhook.Delivered:
		e.record(d, attempt, res, store.StatusDelivered, nil)
	case webhook.Permanent:
		e.record(d, attempt, res, store.StatusFailed, nil)
	default:
		if attempt >= e.Cfg.WebhookMaxAttempts {
			e.record(d, attempt, res, store.StatusFailed, nil)
			break
		}
		next := e.Now().Add(RetryDelays[min(attempt, len(RetryDelays))-1])
		e.record(d, attempt, res, store.StatusPending, &next)
	}
	e.Log.Info("queued delivery attempted", "delivery", d.ID, "message", d.MessageID,
		"attempt", attempt, "status", d.Status, "status_code", res.StatusCode)
}

// Run sweeps the queue and prunes old history until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	if e.Store == nil {
		return
	}
	sweep := time.NewTicker(e.Cfg.WebhookSweep)
	prune := time.NewTicker(time.Hour)
	defer sweep.Stop()
	defer prune.Stop()
	e.prune()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			e.Sweep(ctx)
		case <-prune.C:
			e.prune()
		}
	}
}

func (e *Engine) prune() {
	cutoff := e.Now().Add(-time.Duration(e.Cfg.RetentionDays) * 24 * time.Hour)
	n, err := e.Store.Purge(cutoff)
	if err != nil {
		e.Log.Error("could not prune the history", "err", err)
		return
	}
	if n > 0 {
		e.Log.Info("pruned old messages", "count", n, "older_than_days", e.Cfg.RetentionDays)
	}
}
