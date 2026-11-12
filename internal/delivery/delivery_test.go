package delivery

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/boeq/mail-to-webhook/internal/config"
	"github.com/boeq/mail-to-webhook/internal/payload"
	"github.com/boeq/mail-to-webhook/internal/store"
	"github.com/boeq/mail-to-webhook/internal/webhook"
)

const mail = "From: Alice <alice@sender.test>\r\nTo: box@example.org\r\nSubject: invoice\r\n" +
	"Message-ID: <m1@sender.test>\r\nDate: Sun, 16 Aug 2026 09:20:00 +0000\r\n\r\nbody\r\n"

var t0 = time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC)

// receiver answers with the scripted status codes, then 200, and keeps what it got.
type receiver struct {
	mu     sync.Mutex
	script []int
	bodies [][]byte
	ids    []string
	server *httptest.Server
}

func newReceiver(t *testing.T, script ...int) *receiver {
	r := &receiver{script: script}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.bodies = append(r.bodies, body)
		r.ids = append(r.ids, req.Header.Get(webhook.DeliveryHeader))
		code := http.StatusOK
		if len(r.script) > 0 {
			code, r.script = r.script[0], r.script[1:]
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(r.server.Close)
	return r
}

type fixture struct {
	engine *Engine
	store  *store.Store
	rcv    *receiver
	now    time.Time
}

func setup(t *testing.T, env map[string]string, script ...int) *fixture {
	t.Helper()
	rcv := newReceiver(t, script...)
	t.Setenv("WEBHOOK_URL", rcv.server.URL)
	t.Setenv("SMTP_DOMAIN", "mx.example.org")
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fixture{store: st, rcv: rcv, now: t0}
	wh := webhook.New(webhook.Options{URL: cfg.WebhookURL, Timeout: 5 * time.Second, PermanentStatus: cfg.PermanentStatus})
	f.engine = New(cfg, st, wh, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.engine.Now = func() time.Time { return f.now }
	f.engine.Sleep = func(context.Context, time.Duration) {}
	return f
}

// receive stores the mail the way the SMTP session does and returns its
// delivery and payload.
func (f *fixture) receive(t *testing.T, raw string) (*store.Message, *store.Delivery, []byte) {
	t.Helper()
	env := payload.Envelope{MailFrom: "alice@sender.test", RcptTo: []string{"box@example.org"}, ReceivedAt: t0, Size: len(raw)}
	m, _, err := f.store.RecordMessage(store.Message{ReceivedAt: t0, MailFrom: env.MailFrom, RcptTo: env.RcptTo, Size: len(raw)}, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.store.SMTPDelivery(m.ID, "box@example.org", webhook.EventReceived, f.engine.Webhook.URL(), t0)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := f.engine.Parse([]byte(raw))
	_, body, err := f.engine.Build(msg, env, "box@example.org")
	if err != nil {
		t.Fatal(err)
	}
	return m, d, body
}

func (f *fixture) delivery(t *testing.T, id string) *store.Delivery {
	t.Helper()
	d, err := f.store.GetDelivery(id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDeliverNowRetriesInsideTheConversation(t *testing.T) {
	cases := []struct {
		name     string
		script   []int
		outcome  webhook.Outcome
		status   string
		attempts int
	}{
		{"accepted at once", nil, webhook.Delivered, store.StatusDelivered, 1},
		{"accepted on the third try", []int{500, 503}, webhook.Delivered, store.StatusDelivered, 3},
		{"down for every try: the sender keeps the mail", []int{500, 500, 500}, webhook.Transient, store.StatusDeferred, 3},
		{"a login problem is not a verdict on the mail", []int{401, 401, 401}, webhook.Transient, store.StatusDeferred, 3},
		{"refused for good: no further tries", []int{422, 200}, webhook.Permanent, store.StatusFailed, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, nil, tc.script...)
			m, d, body := f.receive(t, mail)

			res := f.engine.DeliverNow(context.Background(), d, body)

			if res.Outcome != tc.outcome {
				t.Errorf("outcome = %s, want %s", res.Outcome, tc.outcome)
			}
			got := f.delivery(t, d.ID)
			if got.Status != tc.status || got.Attempt != tc.attempts {
				t.Errorf("delivery: status %s after %d attempts, want %s after %d", got.Status, got.Attempt, tc.status, tc.attempts)
			}
			if attempts, _ := f.store.AttemptsFor(d.ID); len(attempts) != tc.attempts {
				t.Errorf("%d attempts recorded, want %d", len(attempts), tc.attempts)
			}
			if stored, _ := f.store.GetMessage(m.ID); stored.Status != tc.status {
				t.Errorf("message status = %s, want %s", stored.Status, tc.status)
			}
			for _, id := range f.rcv.ids {
				if id != d.ID {
					t.Errorf("delivery header %q, want %q on every try", id, d.ID)
				}
			}
		})
	}
}

func TestDeliverNowWithoutHistory(t *testing.T) {
	f := setup(t, nil)
	_, _, body := f.receive(t, mail)
	f.engine.Store = nil

	if res := f.engine.DeliverNow(context.Background(), nil, body); res.Outcome != webhook.Delivered {
		t.Errorf("outcome = %s", res.Outcome)
	}
	if len(f.rcv.ids) != 1 || f.rcv.ids[0] == "" {
		t.Errorf("delivery ids = %q, want one generated id", f.rcv.ids)
	}
}

// A resend has no sender waiting, so it is retried from here: after 1 min,
// 5 min, 15 min, 1 h and 6 h, and then it has failed.
func TestAResendFollowsTheRetrySchedule(t *testing.T) {
	f := setup(t, nil, 200, 500, 500, 500, 500, 500, 500)
	m, first, body := f.receive(t, mail)
	f.engine.DeliverNow(context.Background(), first, body)

	queued, err := f.engine.Resend(m.ID)
	if err != nil || len(queued) != 1 {
		t.Fatalf("resend: %v, %d deliveries", err, len(queued))
	}
	id := queued[0].ID
	if id == first.ID {
		t.Fatal("a resend must be a new delivery")
	}

	for attempt, wait := range RetryDelays {
		if n := f.engine.Sweep(context.Background()); n != 1 {
			t.Fatalf("attempt %d: swept %d deliveries", attempt+1, n)
		}
		d := f.delivery(t, id)
		if d.Status != store.StatusPending || d.NextAttemptAt == nil || !d.NextAttemptAt.Equal(f.now.Add(wait)) {
			t.Fatalf("attempt %d: status %s, next %v, want pending in %s", attempt+1, d.Status, d.NextAttemptAt, wait)
		}
		if n := f.engine.Sweep(context.Background()); n != 0 {
			t.Fatalf("attempt %d: swept again before its time", attempt+1)
		}
		f.now = f.now.Add(wait)
	}

	f.engine.Sweep(context.Background())
	d := f.delivery(t, id)
	if d.Status != store.StatusFailed || d.Attempt != 6 || d.NextAttemptAt != nil {
		t.Errorf("after the last attempt: %+v", d)
	}

	// Every attempt, first delivery and resend alike, carried the same bytes.
	for i, got := range f.rcv.bodies {
		if !bytes.Equal(got, body) {
			t.Errorf("request %d differs from the first payload", i)
		}
	}
}

func TestAResendThatIsAcceptedOrRefused(t *testing.T) {
	for code, status := range map[int]string{204: store.StatusDelivered, 422: store.StatusFailed} {
		f := setup(t, nil, 500, 500, 500, code)
		m, first, body := f.receive(t, mail)
		f.engine.DeliverNow(context.Background(), first, body)

		queued, _ := f.engine.Resend(m.ID)
		f.engine.Sweep(context.Background())

		if d := f.delivery(t, queued[0].ID); d.Status != status || d.Attempt != 1 {
			t.Errorf("%d: status %s after %d attempts, want %s after 1", code, d.Status, d.Attempt, status)
		}
		if stored, _ := f.store.GetMessage(m.ID); stored.Status != status {
			t.Errorf("%d: message status = %s", code, stored.Status)
		}
	}
}

func TestResendNeedsTheStoredMessage(t *testing.T) {
	f := setup(t, nil)
	if _, err := f.engine.Resend("msg_unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown message: %v", err)
	}

	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f.engine.Store = st
	m, _, _ := st.RecordMessage(store.Message{ReceivedAt: t0, RcptTo: []string{"box@example.org"}}, []byte(mail))
	if _, err := f.engine.Resend(m.ID); !errors.Is(err, ErrNoRaw) {
		t.Errorf("no raw message: %v", err)
	}
}

func TestOnePostPerMessageOrPerRecipient(t *testing.T) {
	rcpts := []string{"a@example.org", "b@example.org"}

	once := setup(t, nil)
	if got := once.engine.Targets(rcpts); len(got) != 1 || got[0] != "a@example.org" {
		t.Errorf("default targets = %v", got)
	}
	msg, _ := once.engine.Parse([]byte(mail))
	p, _, err := once.engine.Build(msg, payload.Envelope{RcptTo: rcpts, ReceivedAt: t0}, "a@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ToFull) != 3 {
		t.Errorf("ToFull = %+v, want the header address and both envelope recipients", p.ToFull)
	}

	each := setup(t, map[string]string{"PER_RECIPIENT": "true"})
	if got := each.engine.Targets(rcpts); len(got) != 2 {
		t.Errorf("per-recipient targets = %v", got)
	}
}

// A payload the consumer would answer with 422 is never posted: 422 bounces
// the mail, a deferral keeps it.
func TestAPayloadThatWouldBeRejectedIsNotBuilt(t *testing.T) {
	f := setup(t, nil)
	f.engine.Cfg.FallbackSender = "not an address"
	msg, _ := f.engine.Parse([]byte("From: Mailer Daemon\r\nTo: box@example.org\r\n\r\nbody\r\n"))

	_, _, err := f.engine.Build(msg, payload.Envelope{ReceivedAt: t0}, "box@example.org")
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("err = %v, want ErrIncompatible", err)
	}

	f.engine.Cfg.StrictCompat = false
	if _, body, err := f.engine.Build(msg, payload.Envelope{ReceivedAt: t0}, "box@example.org"); err != nil || body == nil {
		t.Errorf("with STRICT_COMPAT off the payload is posted anyway: %v", err)
	}
}
