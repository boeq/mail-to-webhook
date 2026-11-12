package ui

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/boeq/mail-to-webhook/internal/config"
	"github.com/boeq/mail-to-webhook/internal/delivery"
	"github.com/boeq/mail-to-webhook/internal/payload"
	"github.com/boeq/mail-to-webhook/internal/recipients"
	"github.com/boeq/mail-to-webhook/internal/store"
	"github.com/boeq/mail-to-webhook/internal/webhook"
)

// Values no page may ever show.
const (
	webhookPassword = "hook-password-never-shown"
	webhookSecret   = "signing-secret-never-shown"
	uiPassword      = "ui-password-never-shown"
	headerValue     = "header-value-never-shown"
)

const plainMail = "From: Alice Sender <alice@sender.test>\r\n" +
	"To: Inbox <inbox@example.org>\r\n" +
	"Subject: March invoice\r\n" +
	"Message-ID: <m1@sender.test>\r\n" +
	"Date: Sat, 27 Jun 2026 10:00:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=B\r\n" +
	"\r\n" +
	"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPlease find the invoice attached.\r\n" +
	"--B\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Please find the <b>invoice</b> attached.</p>\r\n" +
	"--B\r\nContent-Type: application/pdf; name=\"invoice.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"invoice.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQgZmFrZQ==\r\n" +
	"--B--\r\n"

const hostile = "<script>alert(1)</script>"

const hostileMail = "From: Mallory <mallory@sender.test>\r\n" +
	"To: inbox@example.org\r\n" +
	"Subject: " + hostile + "\r\n" +
	"X-Note: " + hostile + "\r\n" +
	"Message-ID: <evil@sender.test>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=B\r\n" +
	"\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\nbody " + hostile + "\r\n" +
	"--B\r\nContent-Type: text/html\r\n\r\n<p>html " + hostile + "</p>\r\n" +
	"--B\r\nContent-Type: application/pdf\r\n" +
	"Content-Disposition: attachment; filename=\"" + hostile + ".pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQgZmFrZQ==\r\n" +
	"--B--\r\n"

type env struct {
	t       *testing.T
	cfg     *config.Config
	store   *store.Store
	engine  *delivery.Engine
	ui      *UI
	handler http.Handler
	now     time.Time
}

type option func(*env)

// withoutRaw keeps no raw messages, as STORE_RAW=false does.
func withoutRaw(e *env) { e.cfg.StoreRaw = false }

func withLogin(e *env) {
	e.cfg.UIAuthUser = "operator"
	e.cfg.UIAuthPass = uiPassword
}

func withoutSecret(e *env) { e.cfg.WebhookSecret = "" }

func newEnv(t *testing.T, options ...option) *env {
	t.Helper()
	e := &env{t: t, now: time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)}
	e.cfg = &config.Config{
		SMTPAddr:               ":2525",
		SMTPDomain:             "mx.example.test",
		WebhookURL:             "https://hooks.example.test/inbound",
		WebhookTimeout:         2 * time.Second,
		WebhookSecret:          webhookSecret,
		WebhookAuthUser:        "hook",
		WebhookAuthPass:        webhookPassword,
		WebhookHeaders:         map[string]string{"X-Api-Key": headerValue},
		WebhookMaxAttempts:     6,
		WebhookSweep:           15 * time.Second,
		PermanentStatus:        []int{400, 422},
		IncludeAttachments:     true,
		FallbackSender:         "mailer-daemon@mx.example.test",
		OversizePolicy:         "skip",
		DedupeAttachNames:      true,
		StrictCompat:           true,
		EnsureRecipientInTo:    true,
		SynthesizeMessageID:    true,
		TextFromHTML:           true,
		ExpandAttachedMessages: true,
		ExpandTNEF:             true,
		DataDir:                t.TempDir(),
		RetentionDays:          30,
		StoreRaw:               true,
		HTTPAddr:               ":8080",
		UIEnabled:              true,
		LogLevel:               "info",
	}
	for _, o := range options {
		o(e)
	}

	st, err := store.Open(e.cfg.DataDir, e.cfg.StoreRaw)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	e.store = st

	log := slog.New(slog.DiscardHandler)
	wh := webhook.New(webhook.Options{
		URL:             e.cfg.WebhookURL,
		Timeout:         e.cfg.WebhookTimeout,
		Secret:          e.cfg.WebhookSecret,
		AuthUser:        e.cfg.WebhookAuthUser,
		AuthPass:        e.cfg.WebhookAuthPass,
		Headers:         e.cfg.WebhookHeaders,
		PermanentStatus: e.cfg.PermanentStatus,
	})
	e.engine = delivery.New(e.cfg, st, wh, log)
	e.engine.Now = func() time.Time { return e.now }

	e.ui = &UI{
		Cfg:      e.cfg,
		Store:    st,
		Engine:   e.engine,
		Log:      log,
		Version:  "1.2.3-test",
		SMTPAddr: "127.0.0.1:2525",
		Now:      func() time.Time { return e.now },
	}
	e.handler = e.ui.Handler()
	return e
}

// useChecker points the address check at a test server.
func (e *env) useChecker(checkURL string) {
	e.cfg.RecipientCheckURL = checkURL
	e.ui.Checker = &recipients.Checker{
		URL:        checkURL,
		Timeout:    2 * time.Second,
		Recheck:    24 * time.Hour,
		UnknownTTL: 5 * time.Minute,
		Store:      e.store,
		Client:     e.engine.Webhook,
		Log:        slog.New(slog.DiscardHandler),
		Now:        func() time.Time { return e.now },
	}
}

// seed records a message the way the SMTP session does.
func (e *env) seed(raw string, rcpts ...string) *store.Message {
	e.t.Helper()
	if len(rcpts) == 0 {
		rcpts = []string{"inbox@example.org"}
	}
	parsed, perr := e.engine.Parse([]byte(raw))
	sum := payload.Summarise(parsed)
	row := store.Message{
		ReceivedAt: e.now,
		MailFrom:   "bounce@sender.test",
		RcptTo:     rcpts,
		RemoteAddr: "192.0.2.10",
		Helo:       "mail.sender.test",
		Size:       len(raw),
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
	stored, _, err := e.store.RecordMessage(row, []byte(raw))
	if err != nil {
		e.t.Fatalf("record message: %v", err)
	}
	// Later messages sort after earlier ones, whatever the clock resolution.
	e.now = e.now.Add(time.Second)
	return stored
}

// deliver gives a message one SMTP delivery in the given state.
func (e *env) deliver(m *store.Message, status string, code int, excerpt string) *store.Delivery {
	e.t.Helper()
	d, err := e.store.SMTPDelivery(m.ID, m.RcptTo[0], webhook.EventReceived, e.cfg.WebhookURL, e.now)
	if err != nil {
		e.t.Fatalf("create delivery: %v", err)
	}
	if status == store.StatusPending {
		return d
	}
	attempt := store.Attempt{Attempt: 1, At: e.now, StatusCode: code, DurationMS: 12}
	if status != store.StatusDelivered {
		attempt.Error = "the receiver answered " + http.StatusText(code)
	}
	if err := e.store.RecordAttempt(d.ID, attempt, status, excerpt, nil); err != nil {
		e.t.Fatalf("record attempt: %v", err)
	}
	return d
}

type response struct {
	*httptest.ResponseRecorder
	body string
}

func (e *env) do(r *http.Request) response {
	e.t.Helper()
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	return response{ResponseRecorder: w, body: w.Body.String()}
}

func (e *env) get(path string) response {
	return e.do(httptest.NewRequest(http.MethodGet, path, nil))
}

// post sends a form the way htmx does.
func (e *env) post(path string, form url.Values) response {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	return e.do(r)
}

func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func mustNotContain(t *testing.T, body string, unwanted ...string) {
	t.Helper()
	for _, s := range unwanted {
		if strings.Contains(body, s) {
			t.Errorf("page contains %q", s)
		}
	}
}

// fakeSMTP is the listener the test-mail page talks to. It refuses a recipient
// that starts with "nobody" and hands every accepted message to onData.
func fakeSMTP(t *testing.T, onData func(rcpt, raw string)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go serveSMTP(conn, onData)
		}
	}()
	return l.Addr().String()
}

func serveSMTP(conn net.Conn, onData func(rcpt, raw string)) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	say := func(line string) { _, _ = io.WriteString(conn, line+"\r\n") }
	say("220 fake.test ESMTP")
	rcpt := ""
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(verb, "EHLO"), strings.HasPrefix(verb, "HELO"):
			say("250 fake.test")
		case strings.HasPrefix(verb, "MAIL FROM"):
			say("250 2.1.0 ok")
		case strings.HasPrefix(verb, "RCPT TO"):
			rcpt = strings.Trim(strings.TrimSpace(line[len("RCPT TO:"):]), "<>")
			if strings.HasPrefix(rcpt, "nobody") {
				say("550 5.1.1 No such mailbox here")
				continue
			}
			say("250 2.1.5 ok")
		case verb == "DATA":
			say("354 go ahead")
			var raw strings.Builder
			for {
				part, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if part == ".\r\n" {
					break
				}
				raw.WriteString(strings.TrimPrefix(part, "."))
			}
			onData(rcpt, raw.String())
			say("250 2.0.0 accepted")
		case verb == "QUIT":
			say("221 2.0.0 bye")
			return
		default:
			say("250 ok")
		}
	}
}
