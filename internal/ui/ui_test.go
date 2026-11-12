package ui

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/boeq/mail-to-webhook/internal/store"
	"github.com/boeq/mail-to-webhook/internal/webhook"
)

func TestShellOrderAndCurrentNavItem(t *testing.T) {
	e := newEnv(t)
	msg := e.seed(plainMail)

	pages := []struct{ path, current string }{
		{"/ui/", "Dashboard"},
		{"/ui/messages", "Messages"},
		{"/ui/messages/" + msg.ID, "Messages"},
		{"/ui/addresses", "Addresses"},
		{"/ui/refused", "Refused"},
		{"/ui/settings", "Settings"},
	}
	for _, p := range pages {
		t.Run(p.path, func(t *testing.T) {
			res := e.get(p.path)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d", res.Code)
			}
			order := []string{`<a class="skip-link" href="#main">`, `<header class="topbar">`, `<aside class="sidebar" id="sidebar">`, `<main class="content" id="main">`}
			at := -1
			for _, marker := range order {
				i := strings.Index(res.body, marker)
				if i < 0 || i < at {
					t.Fatalf("shell part %q is missing or out of order", marker)
				}
				at = i
			}
			if n := strings.Count(res.body, `aria-current="page"`); n != 1 {
				t.Fatalf("aria-current appears %d times, want 1", n)
			}
			current := regexp.MustCompile(`(?s)nav__item--current.*?<span class="nav__label">([^<]+)</span>`).FindStringSubmatch(res.body)
			if current == nil || current[1] != p.current {
				t.Errorf("current nav item = %v, want %s", current, p.current)
			}
			mustContain(t, res.body,
				`<meta name="color-scheme" content="dark" />`,
				`<span>Mail to Webhook</span>`,
				`Send a test mail`,
				`<script src="/ui/static/htmx.min.js" defer></script>`,
				`<script src="/ui/static/app.js" defer></script>`,
				`id="shell-status"`, `hx-trigger="every 30s"`,
			)
			// Dark only, nothing from another host, no inline script.
			mustNotContain(t, res.body, "data-theme", `src="http`, `href="http`, `src="//`, "<script>")
		})
	}
}

func TestTryPageIsNoNavItem(t *testing.T) {
	e := newEnv(t)
	res := e.get("/ui/try")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	if n := strings.Count(res.body, `aria-current="page"`); n != 0 {
		t.Errorf("aria-current appears %d times on a page outside the nav", n)
	}
	mustContain(t, res.body, `<form id="try-form" method="post" action="/ui/api/try" class="form"`, `Attach a small PDF`, `name="to" type="email" required`)
}

func TestResponseHeaders(t *testing.T) {
	e := newEnv(t)
	res := e.get("/ui/messages")
	h := res.Header()
	if got := h.Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("CSP = %q", got)
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("missing nosniff or referrer policy: %v", h)
	}
	if h.Get("Cache-Control") != "private, no-store" {
		t.Errorf("a page must not be cached: %q", h.Get("Cache-Control"))
	}

	static := e.get("/ui/static/shell.js")
	if static.Code != http.StatusOK || !strings.HasPrefix(static.Header().Get("Cache-Control"), "public") {
		t.Errorf("static file: status %d, cache %q", static.Code, static.Header().Get("Cache-Control"))
	}
	if static.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("static files carry the security headers too")
	}
}

func TestUnknownPathsAndRedirect(t *testing.T) {
	e := newEnv(t)
	if res := e.get("/ui"); res.Code != http.StatusMovedPermanently || res.Header().Get("Location") != "/ui/" {
		t.Errorf("/ui: status %d, location %q", res.Code, res.Header().Get("Location"))
	}
	for _, path := range []string{"/ui/nope", "/ui/messages/msg_UNKNOWN", "/ui/api/nope"} {
		if res := e.get(path); res.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, res.Code)
		}
	}
	if res := e.get("/ui/api/nope"); !strings.Contains(res.body, `"code":"not_found"`) {
		t.Errorf("an unknown endpoint answers with the error envelope: %s", res.body)
	}
}

func TestMessageList(t *testing.T) {
	e := newEnv(t)
	delivered := e.seed(plainMail)
	e.deliver(delivered, store.StatusDelivered, 204, "")
	failed := e.seed(strings.Replace(plainMail, "m1@sender.test", "m2@sender.test", 1))
	e.deliver(failed, store.StatusFailed, 422, `{"message":"no"}`)

	res := e.get("/ui/messages")
	mustContain(t, res.body,
		`<form class="toolbar" method="get" action="/ui/messages">`,
		`<div class="table-wrap panel">`,
		`<th>Id</th><th>Received</th><th>From</th><th>To</th><th>Subject</th>`,
		`<a class="link link--strong mono" href="/ui/messages/`+delivered.ID+`" title="`+delivered.ID+`">`+store.ShortID(delivered.ID)+`</a>`,
		`<span class="badge badge--done">delivered</span>`,
		`<span class="badge badge--danger">failed</span>`,
		`<span class="muted">(1)</span>`,
		`<span class="muted">· 204</span>`,
		`<span class="muted">· 422</span>`,
		`alice@sender.test`, `inbox@example.org`, `March invoice`,
		`<option value="deferred" >deferred</option>`,
	)

	only := e.get("/ui/messages?status=failed")
	mustContain(t, only.body, failed.ID, `<option value="failed" selected>failed</option>`)
	mustNotContain(t, only.body, delivered.ID)

	none := e.get("/ui/messages?subject=nothing-like-this")
	mustContain(t, none.body, `<h3 class="empty__title">No messages match these filters.</h3>`)

	if bad := e.get("/ui/messages?status=exploded"); bad.Code != http.StatusBadRequest {
		t.Errorf("unknown status filter: status %d, want 400", bad.Code)
	}
	if bad := e.get("/ui/messages?since=yesterday"); bad.Code != http.StatusBadRequest {
		t.Errorf("bad date filter: status %d, want 400", bad.Code)
	}
}

func TestListPollsOnlyWhereSomethingCanChange(t *testing.T) {
	e := newEnv(t)
	msg := e.seed(plainMail)
	e.deliver(msg, store.StatusDelivered, 204, "")

	first := e.get("/ui/messages")
	mustContain(t, first.body, `hx-get="/ui/api/messages/table" hx-swap="outerHTML" hx-trigger="every 10s"`)

	filtered := e.get("/ui/messages?status=delivered")
	mustNotContain(t, filtered.body, `every 2s`, `every 10s`, `hx-get="/ui/api/messages/table`)

	pending := e.seed(strings.Replace(plainMail, "m1@sender.test", "m3@sender.test", 1))
	e.deliver(pending, store.StatusPending, 0, "")

	filtered = e.get("/ui/messages?status=delivered")
	mustContain(t, filtered.body, `hx-get="/ui/api/messages/table?status=delivered" hx-swap="outerHTML" hx-trigger="every 2s"`)

	fragment := e.get("/ui/api/messages/table?status=pending")
	mustContain(t, fragment.body, `<div`, `id="messages"`, `<span class="badge badge--neutral badge--pulse">pending</span>`)
	mustNotContain(t, fragment.body, `<html`)

	dashboard := e.get("/ui/")
	mustContain(t, dashboard.body, `hx-get="/ui/api/dashboard/panel" hx-swap="outerHTML" hx-trigger="every 2s"`)
}

func TestOlderPagination(t *testing.T) {
	e := newEnv(t)
	var ids []string
	for i := 0; i < 3; i++ {
		m := e.seed(strings.Replace(plainMail, "m1@sender.test", "page"+strconv.Itoa(i)+"@sender.test", 1))
		ids = append(ids, m.ID)
	}

	first := e.get("/ui/messages?limit=2&subject=March")
	mustContain(t, first.body, ids[2], ids[1])
	mustNotContain(t, first.body, ids[0])
	older := regexp.MustCompile(`<a class="button" href="([^"]+)">Older</a>`).FindStringSubmatch(first.body)
	if older == nil {
		t.Fatal("no Older link on a page with more rows")
	}
	link := strings.ReplaceAll(older[1], "&amp;", "&")
	parsed, err := url.Parse(link)
	if err != nil || parsed.Path != "/ui/messages" {
		t.Fatalf("Older link = %q", link)
	}
	if q := parsed.Query(); q.Get("before") != ids[1] || q.Get("subject") != "March" || q.Get("limit") != "2" {
		t.Errorf("Older link loses the cursor or a filter: %v", q)
	}

	second := e.get(link)
	mustContain(t, second.body, ids[0])
	mustNotContain(t, second.body, ids[1], ids[2], ">Older</a>")
}

func TestDashboard(t *testing.T) {
	e := newEnv(t)
	empty := e.get("/ui/")
	mustContain(t, empty.body,
		`<h3 class="empty__title">Nothing needs attention.</h3>`,
		`<h3 class="empty__title">No messages yet.</h3>`,
		`hx-trigger="every 10s"`,
		`<div class="figures">`,
	)

	ok := e.seed(plainMail)
	e.deliver(ok, store.StatusDelivered, 204, "")
	bad := e.seed(strings.Replace(plainMail, "m1@sender.test", "m2@sender.test", 1))
	e.deliver(bad, store.StatusDeferred, 503, "down")
	if err := e.store.RecordRefusal(store.Refusal{RemoteAddr: "198.51.100.7", Rcpt: "nobody@example.org", Reason: store.ReasonUnknownRecipient}, e.now); err != nil {
		t.Fatal(err)
	}

	res := e.get("/ui/api/dashboard/panel")
	mustContain(t, res.body,
		`<span class="figure__value">2</span>`,
		`<span class="figure__label">received</span>`,
		`<div class="figure figure--done">`,
		`<span class="badge badge--wait">deferred</span>`,
		`<span class="figure__label">refused</span>`,
	)
	attention := res.body[strings.Index(res.body, `id="attention"`):strings.Index(res.body, `id="recent"`)]
	mustContain(t, attention, bad.ID)
	mustNotContain(t, attention, ok.ID)
}

func TestMessageDetail(t *testing.T) {
	e := newEnv(t)
	msg := e.seed(plainMail)
	d := e.deliver(msg, store.StatusFailed, 422, `{"message":"The given data was invalid."}`)
	if err := e.store.SetNotes(msg.ID, []string{"no Date header, used the receive time"}); err != nil {
		t.Fatal(err)
	}

	res := e.get("/ui/messages/" + msg.ID)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", res.Code, res.body)
	}
	mustContain(t, res.body,
		`<div id="action-result" class="notice notice--info" role="status" hidden></div>`,
		`hx-get="/ui/api/messages/`+msg.ID+`/panel"`,
		`hx-trigger="message-changed from:body"`,
		`<a class="page__back" href="/ui/messages">`,
		`alice@sender.test → inbox@example.org`,
		`<span class="badge badge--danger">failed</span>`,
		`hx-post="/ui/api/messages/`+msg.ID+`/webhook/resend" hx-swap="none">Resend webhook</button>`,
		`href="/ui/api/messages/`+msg.ID+`/raw.eml" download>Download .eml</a>`,
		`id="summary"`, `id="envelope"`, `<dt>MAIL FROM</dt>`, `<dt>RCPT TO</dt>`, `192.0.2.10`, `mail.sender.test`,
		`<section id="notes" class="panel panel--warn">`, `no Date header, used the receive time`,
		`<section id="error" class="panel panel--danger">`, `the receiver answered Unprocessable Entity`,
		`{&#34;message&#34;:&#34;The given data was invalid.&#34;}`,
		`<section id="attachments" class="panel">`, `invoice.pdf`, `application/pdf`,
		`href="/ui/api/messages/`+msg.ID+`/attachments/0" download>Download</a>`,
		`<pre class="output output--wrap">Please find the invoice attached.`,
		`<details id="html-source" class="disclosure">`,
		`&lt;p&gt;Please find the &lt;b&gt;invoice&lt;/b&gt; attached.&lt;/p&gt;`,
		`<details id="headers" class="disclosure">`, `Message-ID`,
		`<section id="webhooks" class="panel">`, `title="`+d.ID+`"`, `<code>smtp</code>`, `<summary>attempts</summary>`, `12 ms`,
		`<section id="payload" class="panel">`,
		`&#34;FromFull&#34;: {`,
		`&#34;Content&#34;: &#34;&lt;13 bytes, base64 omitted&gt;&#34;`,
	)
	mustNotContain(t, res.body, `JVBERi0xLjQgZmFrZQ==`, `id="sink-receipts"`, `<p>Please find`)

	fragment := e.get("/ui/api/messages/" + msg.ID + "/panel")
	mustContain(t, fragment.body, `id="message"`, `Webhook deliveries`)
	mustNotContain(t, fragment.body, `<html`, `id="action-result"`)

	pending := e.seed(strings.Replace(plainMail, "m1@sender.test", "m2@sender.test", 1))
	e.deliver(pending, store.StatusPending, 0, "")
	mustContain(t, e.get("/ui/messages/"+pending.ID).body, `hx-trigger="every 2s, message-changed from:body"`)
}

func TestMessageDetailWithoutRaw(t *testing.T) {
	e := newEnv(t, withoutRaw)
	msg := e.seed(plainMail)
	e.deliver(msg, store.StatusDelivered, 204, "")

	res := e.get("/ui/messages/" + msg.ID)
	mustContain(t, res.body,
		`The raw message is not stored, so its content cannot be shown.`,
		`invoice.pdf`,
		`Webhook deliveries`,
	)
	mustNotContain(t, res.body, `Resend webhook`, `Download .eml`, `id="payload"`, `id="text-body"`)

	resend := e.post("/ui/api/messages/"+msg.ID+"/webhook/resend", nil)
	if resend.Code != http.StatusConflict || !strings.Contains(resend.body, `"code":"conflict"`) {
		t.Errorf("resend without raw: status %d, body %s", resend.Code, resend.body)
	}
	if raw := e.get("/ui/api/messages/" + msg.ID + "/raw.eml"); raw.Code != http.StatusNotFound {
		t.Errorf("raw download without raw: status %d", raw.Code)
	}
}

func TestResend(t *testing.T) {
	e := newEnv(t)
	msg := e.seed(plainMail)
	e.deliver(msg, store.StatusDeferred, 503, "")

	res := e.post("/ui/api/messages/"+msg.ID+"/webhook/resend", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.Code, res.body)
	}
	if got := res.Header().Get("HX-Trigger"); got != "message-changed" {
		t.Errorf("HX-Trigger = %q", got)
	}
	var answer struct {
		ID         string
		Status     string
		Message    string
		Deliveries []struct{ ID, Recipient string }
	}
	if err := json.Unmarshal([]byte(res.body), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.ID != msg.ID || answer.Status != "pending" || answer.Message != "Re-send queued." || len(answer.Deliveries) != 1 {
		t.Errorf("answer = %+v", answer)
	}

	deliveries, err := e.store.DeliveriesFor(msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 2 || deliveries[1].Trigger != store.TriggerResend || deliveries[1].ID != answer.Deliveries[0].ID {
		t.Fatalf("deliveries after resend = %+v", deliveries)
	}
	mustContain(t, e.get("/ui/messages/"+msg.ID).body, `<code>resend</code>`, `every 2s`)

	unknown := e.post("/ui/api/messages/msg_UNKNOWN/webhook/resend", nil)
	if unknown.Code != http.StatusNotFound || !strings.Contains(unknown.body, `"details":{}`) {
		t.Errorf("unknown message: status %d, body %s", unknown.Code, unknown.body)
	}
}

func TestActionsMustComeFromTheInterface(t *testing.T) {
	e := newEnv(t)
	msg := e.seed(plainMail)
	path := "/ui/api/messages/" + msg.ID + "/webhook/resend"

	bare := httptest.NewRequest(http.MethodPost, path, nil)
	if res := e.do(bare); res.Code != http.StatusForbidden || !strings.Contains(res.body, `"code":"forbidden"`) {
		t.Errorf("a post from elsewhere: status %d, body %s", res.Code, res.body)
	}

	foreign := httptest.NewRequest(http.MethodPost, path, nil)
	foreign.Header.Set("Origin", "https://evil.example")
	foreign.Header.Set("Sec-Fetch-Site", "cross-site")
	if res := e.do(foreign); res.Code != http.StatusForbidden {
		t.Errorf("a cross-site post: status %d", res.Code)
	}

	allowed := map[string]func(*http.Request){
		"htmx":           func(r *http.Request) { r.Header.Set("HX-Request", "true") },
		"same origin":    func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) },
		"sec-fetch-site": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") },
	}
	for name, mark := range allowed {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		mark(r)
		if res := e.do(r); res.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", name, res.Code)
		}
	}

	if n, _ := e.store.DeliveriesFor(msg.ID); len(n) != len(allowed) {
		t.Errorf("%d deliveries were queued, want %d", len(n), len(allowed))
	}
}

func TestBuiltInLogin(t *testing.T) {
	open := newEnv(t)
	if res := open.get("/ui/"); res.Code != http.StatusOK {
		t.Fatalf("without a configured login: status %d", res.Code)
	}

	e := newEnv(t, withLogin)
	for _, path := range []string{"/ui/", "/ui/messages", "/ui/static/shell.js", "/ui/api/dashboard/panel", "/ui/nope"} {
		res := e.get(path)
		if res.Code != http.StatusUnauthorized || res.Header().Get("WWW-Authenticate") != `Basic realm="mail-to-webhook"` {
			t.Errorf("%s without credentials: status %d, challenge %q", path, res.Code, res.Header().Get("WWW-Authenticate"))
		}
	}

	wrong := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	wrong.SetBasicAuth("operator", "guess")
	if res := e.do(wrong); res.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: status %d", res.Code)
	}

	right := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	right.SetBasicAuth("operator", uiPassword)
	if res := e.do(right); res.Code != http.StatusOK {
		t.Errorf("right password: status %d", res.Code)
	}

	// The sink is called by the service, not by the operator's browser.
	sink := httptest.NewRequest(http.MethodPost, sinkPath, strings.NewReader(`{}`))
	if res := e.do(sink); res.Code == http.StatusUnauthorized || res.Code == http.StatusForbidden {
		t.Errorf("the sink asked for the operator login: status %d", res.Code)
	}
}

func sinkRequest(deliveryID, secret string, body []byte, at time.Time) *http.Request {
	r := httptest.NewRequest(http.MethodPost, sinkPath, strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(webhook.EventHeader, webhook.EventReceived)
	if deliveryID != "" {
		r.Header.Set(webhook.DeliveryHeader, deliveryID)
	}
	r.Header.Set(webhook.TimestampHeader, strconv.FormatInt(at.Unix(), 10))
	if secret != "" {
		r.Header.Set(webhook.SignatureHeader, webhook.Sign(secret, at.Unix(), body))
	}
	return r
}

func TestSinkKeepsOnlyWhatItCanTieToADelivery(t *testing.T) {
	e := newEnv(t)
	msg := e.seed(plainMail)
	d := e.deliver(msg, store.StatusDelivered, 204, "")
	body := []byte(`{"FromFull":{"Email":"alice@sender.test"},"Attachments":[{"Name":"invoice.pdf","Content":"JVBERi0xLjQgZmFrZQ==","ContentType":"application/pdf"}]}`)

	for name, r := range map[string]*http.Request{
		"no delivery header": sinkRequest("", webhookSecret, body, e.now),
		"unknown delivery":   sinkRequest("whd_01UNKNOWN", webhookSecret, body, e.now),
	} {
		if res := e.do(r); res.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, res.Code)
		}
	}
	if stored, _ := e.store.SinkReceiptsFor([]string{d.ID, "whd_01UNKNOWN"}); len(stored) != 0 {
		t.Fatalf("a receipt was stored for a delivery the history does not know: %+v", stored)
	}

	cases := []struct {
		name   string
		r      *http.Request
		status string
	}{
		{"valid", sinkRequest(d.ID, webhookSecret, body, e.now), "valid"},
		{"wrong secret", sinkRequest(d.ID, "another-secret", body, e.now), "invalid"},
		{"no signature", sinkRequest(d.ID, "", body, e.now), "missing"},
		{"old timestamp", sinkRequest(d.ID, webhookSecret, body, e.now.Add(-time.Hour)), "stale"},
	}
	for _, c := range cases {
		if res := e.do(c.r); res.Code != http.StatusNoContent {
			t.Fatalf("%s: status %d, want 204 whatever the signature", c.name, res.Code)
		}
		stored, err := e.store.SinkReceiptsFor([]string{d.ID})
		if err != nil || len(stored) == 0 {
			t.Fatalf("%s: no receipt stored (%v)", c.name, err)
		}
		if stored[0].SignatureStatus != c.status {
			t.Errorf("%s: signature status = %q, want %q", c.name, stored[0].SignatureStatus, c.status)
		}
		if strings.Contains(stored[0].Body, "JVBERi0xLjQgZmFrZQ==") || !strings.Contains(stored[0].Body, "<13 bytes, base64 omitted>") {
			t.Errorf("%s: the stored body still carries the file: %s", c.name, stored[0].Body)
		}
	}

	page := e.get("/ui/messages/" + msg.ID).body
	mustContain(t, page,
		`<section id="sink-receipts" class="panel">`, `Webhook received`,
		`<details class="sink-receipt">`,
		`<span class="badge badge--done">valid</span>`,
		`<span class="badge badge--danger">invalid</span>`,
		`<span class="badge badge--danger">missing</span>`,
		`<span class="badge badge--danger">stale</span>`,
	)
}

func TestSinkWithoutASecretSaysUnsigned(t *testing.T) {
	e := newEnv(t, withoutSecret)
	msg := e.seed(plainMail)
	d := e.deliver(msg, store.StatusDelivered, 204, "")

	if res := e.do(sinkRequest(d.ID, "", []byte(`not json at all`), e.now)); res.Code != http.StatusNoContent {
		t.Fatalf("status = %d", res.Code)
	}
	stored, _ := e.store.SinkReceiptsFor([]string{d.ID})
	if len(stored) != 1 || stored[0].SignatureStatus != "unsigned" || stored[0].Body != "not json at all" {
		t.Errorf("receipt = %+v", stored)
	}
	mustContain(t, e.get("/ui/messages/"+msg.ID).body, `<span class="badge badge--neutral">unsigned</span>`)
}

func TestMailContentIsAlwaysEscaped(t *testing.T) {
	e := newEnv(t)
	msg := e.seed(hostileMail)
	e.deliver(msg, store.StatusFailed, 422, hostile)
	if err := e.store.RecordRefusal(store.Refusal{RemoteAddr: "198.51.100.7", Helo: hostile, MailFrom: hostile, Rcpt: hostile, Reason: store.ReasonUnknownRecipient}, e.now); err != nil {
		t.Fatal(err)
	}
	if err := e.store.PutRecipient(hostile+"@example.org", store.RecipientUnknown, e.now); err != nil {
		t.Fatal(err)
	}

	escaped := `&lt;script&gt;alert(1)&lt;/script&gt;`
	for _, path := range []string{
		"/ui/", "/ui/messages", "/ui/messages/" + msg.ID, "/ui/refused", "/ui/addresses",
		"/ui/api/messages/table", "/ui/api/messages/" + msg.ID + "/panel",
		"/ui/try?error=" + url.QueryEscape(hostile) + "&to=" + url.QueryEscape(hostile),
	} {
		res := e.get(path)
		if res.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, res.Code)
			continue
		}
		if strings.Contains(res.body, hostile) || strings.Contains(res.body, "<script>") {
			t.Errorf("%s shows mail content unescaped", path)
		}
		if !strings.Contains(res.body, escaped) && !strings.Contains(res.body, `&lt;script&gt;alert(1)`) {
			t.Errorf("%s does not show the hostile text at all, so the test proves nothing", path)
		}
	}

	// Subject, body, HTML source, header value, attachment name and response
	// excerpt are each on the detail page, each escaped.
	detail := e.get("/ui/messages/" + msg.ID).body
	if n := strings.Count(detail, `&lt;script&gt;alert(1)`); n < 6 {
		t.Errorf("the hostile text appears escaped %d times on the detail page, want at least 6", n)
	}

	// The attachment comes down as a download of plain bytes under a name
	// without a path, whatever the mail called it.
	file := e.get("/ui/api/messages/" + msg.ID + "/attachments/0")
	if file.Code != http.StatusOK || file.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("attachment: status %d, type %q", file.Code, file.Header().Get("Content-Type"))
	}
	disposition := file.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment;") || strings.ContainsAny(disposition, "/\\") && !strings.Contains(disposition, "%") {
		t.Errorf("Content-Disposition = %q", disposition)
	}
}

func TestDownloads(t *testing.T) {
	e := newEnv(t)
	msg := e.seed(plainMail)

	raw := e.get("/ui/api/messages/" + msg.ID + "/raw.eml")
	if raw.Code != http.StatusOK || raw.body != plainMail {
		t.Fatalf("raw download: status %d, %d bytes", raw.Code, len(raw.body))
	}
	if raw.Header().Get("Content-Type") != "message/rfc822" || raw.Header().Get("Content-Disposition") != `attachment; filename=`+msg.ID+`.eml` {
		t.Errorf("raw download headers: %v", raw.Header())
	}
	if raw.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("a download must not be cached: %q", raw.Header().Get("Cache-Control"))
	}

	file := e.get("/ui/api/messages/" + msg.ID + "/attachments/0")
	if file.Code != http.StatusOK || file.body != "%PDF-1.4 fake" {
		t.Fatalf("attachment: status %d, body %q", file.Code, file.body)
	}
	if file.Header().Get("Content-Disposition") != "attachment; filename=invoice.pdf" {
		t.Errorf("attachment disposition = %q", file.Header().Get("Content-Disposition"))
	}

	for _, n := range []string{"1", "-1", "x"} {
		if res := e.get("/ui/api/messages/" + msg.ID + "/attachments/" + n); res.Code != http.StatusNotFound {
			t.Errorf("attachment %s: status %d, want 404", n, res.Code)
		}
	}
}

func TestNoSecretReachesAPage(t *testing.T) {
	e := newEnv(t, withLogin)
	msg := e.seed(plainMail)
	e.deliver(msg, store.StatusFailed, 500, "boom")

	paths := []string{
		"/ui/", "/ui/messages", "/ui/messages/" + msg.ID, "/ui/addresses", "/ui/refused", "/ui/settings", "/ui/try",
		"/ui/api/shell/status", "/ui/api/dashboard/panel", "/ui/api/messages/table",
		"/ui/api/messages/" + msg.ID + "/panel", "/ui/api/addresses/table", "/ui/api/refused/table",
		"/ui/nope",
	}
	for _, path := range paths {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.SetBasicAuth("operator", uiPassword)
		res := e.do(r)
		if res.Code != http.StatusOK && res.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", path, res.Code)
		}
		mustNotContain(t, res.body, webhookPassword, webhookSecret, uiPassword, headerValue)
	}
}

func TestSettingsPage(t *testing.T) {
	e := newEnv(t, withLogin)
	e.cfg.RecipientCheckURL = "https://checker:" + webhookPassword + "@consumer.example.test/recipients"

	r := httptest.NewRequest(http.MethodGet, "/ui/settings", nil)
	r.SetBasicAuth("operator", uiPassword)
	res := e.do(r)
	mustContain(t, res.body,
		`<h2 class="panel__title">`, `Version and storage`, `1.2.3-test`, `30 days`,
		`<td><code>WEBHOOK_SECRET</code></td>`,
		`<span class="badge badge--done">set</span>`,
		`<span class="badge badge--neutral">not set</span>`,
		`<code>https://hooks.example.test/inbound</code>`,
		`<code>https://consumer.example.test/recipients</code>`,
		`<code>X-Api-Key</code>`,
	)
	mustNotContain(t, res.body, webhookPassword, webhookSecret, uiPassword, headerValue)

	row := regexp.MustCompile(`(?s)<td><code>WEBHOOK_AUTH_PASS</code></td>\s*<td>(.*?)</td>`).FindStringSubmatch(res.body)
	if row == nil || !strings.Contains(row[1], `badge--done">set`) || strings.Contains(row[1], "<code>") {
		t.Errorf("WEBHOOK_AUTH_PASS row = %v", row)
	}
}

func TestAddresses(t *testing.T) {
	e := newEnv(t)
	if err := e.store.PutRecipient("AbC.purchase@example.org", store.RecipientKnown, e.now); err != nil {
		t.Fatal(err)
	}
	if err := e.store.PutRecipient("ghost@example.org", store.RecipientUnknown, e.now); err != nil {
		t.Fatal(err)
	}

	off := e.get("/ui/addresses")
	mustContain(t, off.body,
		`The recipient check is off: RECIPIENT_CHECK_URL is not set, so every address in`,
		`<form class="toolbar" method="get" action="/ui/addresses">`,
		`hx-get="/ui/api/addresses/table"`, `hx-trigger="address-changed from:body"`,
		`<td class="mono">AbC.purchase@example.org</td>`,
		`<span class="badge badge--done">known</span>`,
		`<span class="badge badge--neutral">unknown</span>`,
		`hx-post="/ui/api/addresses/forget"`,
		`hx-vals="{&#34;address&#34;:&#34;ghost@example.org&#34;}"`,
		`hx-confirm="Forget ghost@example.org? It is checked again when mail arrives for it."`,
	)
	mustNotContain(t, off.body, `Check again`)
	if res := e.post("/ui/api/addresses/check", url.Values{"address": {"ghost@example.org"}}); res.Code != http.StatusConflict {
		t.Errorf("check while the check is off: status %d, want 409", res.Code)
	}

	// The consumer knows exactly one address.
	var asked []string
	consumer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Addresses []string }
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &in)
		asked = append(asked, in.Addresses...)
		known := []string{}
		for _, a := range in.Addresses {
			if a == "ghost@example.org" {
				known = append(known, a)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string][]string{"known": known})
	}))
	e.useChecker(consumer.URL)

	on := e.get("/ui/addresses?status=unknown")
	mustContain(t, on.body, `Check again`, `hx-post="/ui/api/addresses/check"`, `hx-get="/ui/api/addresses/table?status=unknown"`)
	mustNotContain(t, on.body, `The recipient check is off`, `AbC.purchase@example.org`)

	check := e.post("/ui/api/addresses/check", url.Values{"address": {"ghost@example.org"}})
	if check.Code != http.StatusOK || check.Header().Get("HX-Trigger") != "address-changed" {
		t.Fatalf("check: status %d, trigger %q, body %s", check.Code, check.Header().Get("HX-Trigger"), check.body)
	}
	mustContain(t, check.body, `"status":"known"`, `"message":"ghost@example.org is known."`)
	if len(asked) != 1 || asked[0] != "ghost@example.org" {
		t.Errorf("the consumer was asked %v", asked)
	}
	if r, _ := e.store.GetRecipient("ghost@example.org"); r == nil || r.Status != store.RecipientKnown {
		t.Errorf("the list was not updated: %+v", r)
	}

	if res := e.post("/ui/api/addresses/check", url.Values{}); res.Code != http.StatusUnprocessableEntity {
		t.Errorf("check without an address: status %d, want 422", res.Code)
	}

	consumer.Close()
	down := e.post("/ui/api/addresses/check", url.Values{"address": {"AbC.purchase@example.org"}})
	if down.Code != http.StatusServiceUnavailable || down.Header().Get("HX-Trigger") != "" {
		t.Errorf("check while the consumer is down: status %d", down.Code)
	}
	if r, _ := e.store.GetRecipient("AbC.purchase@example.org"); r == nil || r.Status != store.RecipientKnown {
		t.Errorf("an unanswered check changed the list: %+v", r)
	}
	mustContain(t, e.get("/ui/api/shell/status").body, `status-row--danger`, `<span class="status-row__value">failing</span>`)

	forget := e.post("/ui/api/addresses/forget", url.Values{"address": {"ghost@example.org"}})
	if forget.Code != http.StatusOK || forget.Header().Get("HX-Trigger") != "address-changed" {
		t.Fatalf("forget: status %d", forget.Code)
	}
	if r, _ := e.store.GetRecipient("ghost@example.org"); r != nil {
		t.Errorf("the address is still on the list: %+v", r)
	}
	mustContain(t, e.get("/ui/api/addresses/table?q=ghost").body, `<h3 class="empty__title">No addresses match.</h3>`)
}

func TestShellStatus(t *testing.T) {
	e := newEnv(t)
	off := e.get("/ui/api/shell/status")
	mustContain(t, off.body,
		`<span class="status-row__label">SMTP</span>`, `127.0.0.1:2525`,
		`<span class="status-row__value status-row__value--clip">hooks.example.test</span>`,
		`<span class="dot dot--neutral"></span>`, `<span class="status-row__value">off</span>`,
	)

	e.useChecker("https://consumer.example.test/recipients")
	mustContain(t, e.get("/ui/api/shell/status").body, `<span class="dot dot--done"></span>`, `<span class="status-row__value">ok</span>`)
}

func TestRefused(t *testing.T) {
	e := newEnv(t)
	mustContain(t, e.get("/ui/refused").body, `<h3 class="empty__title">Nothing has been refused.</h3>`)

	for i := 0; i < pageSize+1; i++ {
		r := store.Refusal{RemoteAddr: "198.51.100.7", MailFrom: "spam@bad.test", Rcpt: "guess" + strconv.Itoa(i) + "@example.org", Reason: store.ReasonUnknownRecipient}
		if err := e.store.RecordRefusal(r, e.now); err != nil {
			t.Fatal(err)
		}
	}
	// The same attempt again within the hour counts on its row.
	again := store.Refusal{RemoteAddr: "203.0.113.9", Rcpt: "", Reason: store.ReasonBlocked}
	for i := 0; i < 3; i++ {
		if err := e.store.RecordRefusal(again, e.now); err != nil {
			t.Fatal(err)
		}
	}

	res := e.get("/ui/refused")
	mustContain(t, res.body,
		`<form class="toolbar" method="get" action="/ui/refused">`,
		`<th>Last seen</th><th>Remote address</th><th>Sender</th><th>Recipient</th>`,
		`<span class="badge badge--wait">blocked</span>`,
		`<span class="badge badge--neutral">unknown_recipient</span>`,
		`<td class="num">3</td>`,
		`hx-get="/ui/api/refused/table" hx-swap="outerHTML" hx-trigger="every 30s"`,
	)
	older := regexp.MustCompile(`<a class="button" href="([^"]+)">Older</a>`).FindStringSubmatch(res.body)
	if older == nil || !strings.HasPrefix(older[1], "/ui/refused?before=") {
		t.Fatalf("Older link = %v", older)
	}
	next := e.get(older[1])
	mustContain(t, next.body, `guess0@example.org`)
	mustNotContain(t, next.body, `>Older</a>`, `hx-get="/ui/api/refused/table`)

	only := e.get("/ui/api/refused/table?reason=blocked")
	mustContain(t, only.body, `203.0.113.9`)
	mustNotContain(t, only.body, `spam@bad.test`, `hx-get`)

	if bad := e.get("/ui/refused?reason=whatever"); bad.Code != http.StatusBadRequest {
		t.Errorf("unknown reason filter: status %d, want 400", bad.Code)
	}
}

func TestSendATestMail(t *testing.T) {
	e := newEnv(t)
	var got struct{ rcpt, raw string }
	e.ui.SMTPAddr = fakeSMTP(t, func(rcpt, raw string) {
		got.rcpt, got.raw = rcpt, raw
		e.seed(raw, rcpt)
	})

	res := e.post("/ui/api/try", url.Values{"to": {"inbox@example.org"}, "subject": {"Café test\r\nBcc: victim@example.org"}, "attach": {"1"}})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.Code, res.body)
	}
	location := res.Header().Get("HX-Redirect")
	if !strings.HasPrefix(location, "/ui/messages/msg_") {
		t.Fatalf("HX-Redirect = %q", location)
	}
	if got.rcpt != "inbox@example.org" {
		t.Errorf("recipient = %q", got.rcpt)
	}
	mustContain(t, got.raw,
		`From: "mail-to-webhook test" <mailer-daemon@mx.example.test>`,
		"To: <inbox@example.org>",
		"Subject: =?utf-8?q?Caf=C3=A9_testBcc:_victim@example.org?=",
		"Content-Type: multipart/mixed; boundary=",
		`Content-Disposition: attachment; filename="test.pdf"`,
	)
	if strings.Contains(got.raw, "\r\nBcc:") {
		t.Error("a line break in the subject started a header of its own")
	}

	detail := e.get(location)
	mustContain(t, detail.body, "Café testBcc: victim@example.org", "test.pdf", defaultTestBody)

	// Without the script the same post ends in a redirect.
	plain := httptest.NewRequest(http.MethodPost, "/ui/api/try", strings.NewReader(url.Values{"to": {"inbox@example.org"}}.Encode()))
	plain.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	plain.Header.Set("Sec-Fetch-Site", "same-origin")
	if res := e.do(plain); res.Code != http.StatusSeeOther || !strings.HasPrefix(res.Header().Get("Location"), "/ui/messages/msg_") {
		t.Errorf("plain form post: status %d, location %q", res.Code, res.Header().Get("Location"))
	}

	refused := e.post("/ui/api/try", url.Values{"to": {"nobody@example.org"}})
	if refused.Code != http.StatusUnprocessableEntity {
		t.Fatalf("refused recipient: status %d, body %s", refused.Code, refused.body)
	}
	mustContain(t, refused.body, `"code":"validation_error"`, `the mail was not accepted: 550 5.1.1 No such mailbox here`)

	plainRefused := httptest.NewRequest(http.MethodPost, "/ui/api/try", strings.NewReader(url.Values{"to": {"nobody@example.org"}}.Encode()))
	plainRefused.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	plainRefused.Header.Set("Origin", "http://"+plainRefused.Host)
	back := e.do(plainRefused)
	if back.Code != http.StatusSeeOther || !strings.HasPrefix(back.Header().Get("Location"), "/ui/try?error=") {
		t.Fatalf("plain refused post: status %d, location %q", back.Code, back.Header().Get("Location"))
	}
	mustContain(t, e.get(back.Header().Get("Location")).body,
		`<div id="try-error" class="notice notice--error" role="alert">`,
		`the mail was not accepted: 550 5.1.1 No such mailbox here`,
		`value="nobody@example.org"`,
	)

	for _, to := range []string{"", "no-at-sign", "a b@example.org"} {
		if res := e.post("/ui/api/try", url.Values{"to": {to}}); res.Code != http.StatusUnprocessableEntity {
			t.Errorf("to=%q: status %d, want 422", to, res.Code)
		}
	}
}

func TestSendATestMailWhenTheListenerIsDown(t *testing.T) {
	e := newEnv(t)
	e.ui.SMTPAddr = "127.0.0.1:1"
	res := e.post("/ui/api/try", url.Values{"to": {"inbox@example.org"}})
	if res.Code != http.StatusUnprocessableEntity || !strings.Contains(res.body, "the mail was not accepted: ") {
		t.Errorf("status %d, body %s", res.Code, res.body)
	}
}

func TestEveryIconUsedExists(t *testing.T) {
	used := regexp.MustCompile(`\{\{\s*icon "([^"]+)"`)
	names := map[string]bool{}
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	for _, file := range files {
		data, err := fs.ReadFile(templateFS, file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range used.FindAllStringSubmatch(string(data), -1) {
			names[m[1]] = true
		}
	}
	for _, item := range nav("/ui/") {
		names[item.Icon] = true
	}
	if len(names) < 8 {
		t.Fatalf("only %d icon names found; the scan is broken", len(names))
	}
	for name := range names {
		if _, ok := iconPaths[name]; !ok {
			t.Errorf("icon %q is used but not in the set", name)
		}
	}
	if _, err := icon("no-such-icon"); err == nil {
		t.Error("an unknown icon must be an error, not a blank space")
	}
	if len(iconPaths) != 21 {
		t.Errorf("the icon set has %d icons, want the 21 of the source", len(iconPaths))
	}
}

func TestBadgeAndTones(t *testing.T) {
	cases := map[[2]string]string{
		{"webhook", "delivered"}:              `<span class="badge badge--done">delivered</span>`,
		{"webhook", "pending"}:                `<span class="badge badge--neutral badge--pulse">pending</span>`,
		{"message", "deferred"}:               `<span class="badge badge--wait">deferred</span>`,
		{"message", "failed"}:                 `<span class="badge badge--danger">failed</span>`,
		{"recipient", "known"}:                `<span class="badge badge--done">known</span>`,
		{"recipient", "pending"}:              `<span class="badge badge--neutral">pending</span>`,
		{"signature", "stale"}:                `<span class="badge badge--danger">stale</span>`,
		{"setting", "not set"}:                `<span class="badge badge--neutral">not set</span>`,
		{"reason", "too_many_bad_recipients"}: `<span class="badge badge--wait">too_many_bad_recipients</span>`,
		{"reason", "unknown_recipient"}:       `<span class="badge badge--neutral">unknown_recipient</span>`,
		{"nonsense", "<b>x</b>"}:              `<span class="badge badge--neutral">&lt;b&gt;x&lt;/b&gt;</span>`,
	}
	for in, want := range cases {
		if got := string(badge(in[0], in[1])); got != want {
			t.Errorf("badge(%q, %q) = %s, want %s", in[0], in[1], got, want)
		}
	}
	for _, status := range messageStatuses {
		if _, ok := tones["webhook"][status]; !ok {
			t.Errorf("status %q has no tone of its own", status)
		}
	}
}

func TestFormatting(t *testing.T) {
	sizes := map[int]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 3 << 30: "3.0 GiB"}
	for in, want := range sizes {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
	if got := humanBytes(int64(2048)); got != "2.0 KiB" {
		t.Errorf("humanBytes(int64) = %q", got)
	}
	at := time.Date(2026, 11, 12, 10, 30, 5, 0, time.FixedZone("CET", 3600))
	if got := timestamp(at); got != "2026-11-12T09:30:05Z" {
		t.Errorf("timestamp = %q", got)
	}
	var none *time.Time
	if timestamp(none) != "—" || timestamp(time.Time{}) != "—" || dash("") != "—" || dash("x") != "x" {
		t.Error("a missing value is shown as a dash")
	}
}

func TestPrettyJSONKeepsOrderAndDropsFiles(t *testing.T) {
	body := []byte(`{"Subject":"x","Attachments":[{"Name":"a.pdf","Content":"JVBERi0xLjQgZmFrZQ==","ContentType":"application/pdf"}],"Envelope":{"Size":1},"RawEmail":"QUJD"}`)
	got := prettyJSON(body)
	want := `{
  "Subject": "x",
  "Attachments": [
    {
      "Name": "a.pdf",
      "Content": "<13 bytes, base64 omitted>",
      "ContentType": "application/pdf"
    }
  ],
  "Envelope": {
    "Size": 1
  },
  "RawEmail": "<3 bytes, base64 omitted>"
}`
	if got != want {
		t.Errorf("prettyJSON =\n%s\nwant\n%s", got, want)
	}
	if prettyJSON([]byte("plain \xff text")) != "plain � text" {
		t.Error("a body that is not JSON is kept as text, made valid UTF-8")
	}
}
