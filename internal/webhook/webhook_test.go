package webhook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A known-good vector for this signature scheme, computed independently.
func TestSignMatchesThePublishedVector(t *testing.T) {
	body := `{"event":"request.completed","sent_at":"2026-02-01T06:00:47Z","delivery_id":"whd_01JEXAMPLE0000000000000000","request":{"id":"req_01JEXAMPLE0000000000000000","status":"completed"}}`
	got := Sign("example-webhook-secret-0000", 1769925647, []byte(body))
	want := "sha256=c5efc669b7d5c7e2fe495f2de94ae5e18f034d6034ed44ea96929f8af31b30c9"
	if got != want {
		t.Errorf("Sign = %s, want %s", got, want)
	}
}

// The vector printed in docs/payload.md.
func TestSignMatchesTheDocumentedVector(t *testing.T) {
	got := Sign("example-secret", 1800000000, []byte(`{"a":1}`))
	if want := "sha256=2ded0f9cf1c6e2fe6053dca0a54e9f11827c3fdbd7e2d4a766001a13cdfb41e9"; got != want {
		t.Errorf("Sign = %s, want %s", got, want)
	}
}

func TestPostSendsTheBodyAsIsWithAuthAndSignature(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(Options{URL: srv.URL, Timeout: time.Second, Secret: "s3cret", AuthUser: "hook", AuthPass: "token",
		Headers: map[string]string{"X-Extra": "1", "User-Agent": "overridden"}})
	body := []byte(`{"Subject":"Café <b>"}`)
	res := c.Post(context.Background(), "whd_1", EventReceived, body)

	if res.Outcome != Delivered || res.StatusCode != 204 || res.Error != "" {
		t.Fatalf("result = %+v", res)
	}
	if string(gotBody) != string(body) {
		t.Errorf("body = %s", gotBody)
	}
	user, pass, ok := got.BasicAuth()
	if !ok || user != "hook" || pass != "token" {
		t.Errorf("basic auth = %q %q %v", user, pass, ok)
	}
	for name, want := range map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   UserAgent, // a static header cannot replace the ones the contract fixes
		"X-Extra":      "1",
		EventHeader:    EventReceived,
		DeliveryHeader: "whd_1",
	} {
		if got.Header.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, got.Header.Get(name), want)
		}
	}
	ts := got.Header.Get(TimestampHeader)
	if status := Verify("s3cret", ts, got.Header.Get(SignatureHeader), gotBody, time.Now(), 5*time.Minute); status != "valid" {
		t.Errorf("signature is %s", status)
	}
}

func TestNoSecretMeansNoSignatureHeader(t *testing.T) {
	var header http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { header = r.Header }))
	defer srv.Close()

	c := New(Options{URL: srv.URL, Timeout: time.Second})
	c.Post(context.Background(), "whd_1", EventReceived, []byte(`{}`))

	if _, ok := header[SignatureHeader]; ok {
		t.Error("signature header sent without a secret")
	}
	if header.Get(TimestampHeader) == "" {
		t.Error("timestamp header missing")
	}
	if c.Signed() {
		t.Error("Signed() = true")
	}
}

// Only a few codes are a verdict on the message. Everything else is a
// deployment mistake or an outage, and must delay the mail, not destroy it.
func TestAnswersAreClassified(t *testing.T) {
	want := map[int]Outcome{
		200: Delivered, 202: Delivered, 204: Delivered,
		400: Permanent, 413: Permanent, 422: Permanent,
		301: Transient, 302: Transient,
		401: Transient, 403: Transient, 404: Transient, 409: Transient, 429: Transient,
		500: Transient, 502: Transient, 503: Transient,
	}
	redirected := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			redirected++
			return
		}
		code, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if code >= 300 && code < 400 {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(code)
		io.WriteString(w, strings.Repeat("x", 2*ExcerptLimit))
	}))
	defer srv.Close()

	for code, outcome := range want {
		c := New(Options{URL: srv.URL + "/" + strconv.Itoa(code), Timeout: time.Second, PermanentStatus: []int{400, 406, 410, 413, 415, 422}})
		res := c.Post(context.Background(), "whd_1", EventReceived, []byte(`{}`))
		if res.Outcome != outcome || res.StatusCode != code {
			t.Errorf("%d: outcome %s (status %d), want %s", code, res.Outcome, res.StatusCode, outcome)
		}
		if outcome != Delivered && (res.Error == "" || len(res.Excerpt) != ExcerptLimit) {
			t.Errorf("%d: error %q, excerpt of %d bytes", code, res.Error, len(res.Excerpt))
		}
	}
	if redirected != 0 {
		t.Errorf("a redirect was followed %d times", redirected)
	}
}

func TestAnUnreachableEndpointIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	c := New(Options{URL: url, Timeout: time.Second, AuthUser: "hook", AuthPass: "hunter2"})
	res := c.Post(context.Background(), "whd_1", EventReceived, []byte(`{}`))
	if res.Outcome != Transient || res.StatusCode != 0 || res.Error == "" {
		t.Errorf("result = %+v", res)
	}
	if strings.Contains(res.Error, "hunter2") {
		t.Errorf("the password leaked into the error: %s", res.Error)
	}
}

func TestVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"a":1}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	good := Sign("secret", now.Unix(), body)
	old := strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10)

	cases := []struct {
		name, secret, ts, sig, want string
		body                        []byte
	}{
		{"valid", "secret", ts, good, "valid", body},
		{"no secret configured", "", ts, good, "unsigned", body},
		{"no signature sent", "secret", ts, "", "missing", body},
		{"no timestamp sent", "secret", "", good, "missing", body},
		{"wrong secret", "other", ts, good, "invalid", body},
		{"changed body", "secret", ts, good, "invalid", []byte(`{"a":2}`)},
		{"timestamp is not a number", "secret", "soon", good, "invalid", body},
		{"too old", "secret", old, Sign("secret", now.Add(-10*time.Minute).Unix(), body), "stale", body},
	}
	for _, tc := range cases {
		if got := Verify(tc.secret, tc.ts, tc.sig, tc.body, now, 5*time.Minute); got != tc.want {
			t.Errorf("%s: Verify = %s, want %s", tc.name, got, tc.want)
		}
	}
}
