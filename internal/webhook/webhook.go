// Package webhook posts a payload to the configured endpoint and says what
// the answer means for the mail.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	UserAgent       = "mail-to-webhook/1.0"
	EventHeader     = "X-Mail-To-Webhook-Event"
	DeliveryHeader  = "X-Mail-To-Webhook-Delivery"
	TimestampHeader = "X-Mail-To-Webhook-Timestamp"
	SignatureHeader = "X-Mail-To-Webhook-Signature"

	// EventReceived is the only event: a message arrived.
	EventReceived = "message.received"

	// ExcerptLimit is how much of a bad response is kept.
	ExcerptLimit = 500
)

// Outcome is what an answer means for the mail.
type Outcome int

const (
	// Delivered: any 2xx.
	Delivered Outcome = iota
	// Permanent: the endpoint will never take this message. The sender gets a bounce.
	Permanent
	// Transient: everything else, including 401, 403, 404, 429, 3xx, 5xx
	// and network errors. Those are deployment mistakes or outages, not
	// verdicts on the message, and bouncing mail for them destroys it.
	Transient
)

func (o Outcome) String() string {
	switch o {
	case Delivered:
		return "delivered"
	case Permanent:
		return "permanent"
	}
	return "transient"
}

type Options struct {
	URL             string
	Timeout         time.Duration
	Secret          string
	AuthUser        string
	AuthPass        string
	Headers         map[string]string
	PermanentStatus []int
	Transport       http.RoundTripper
}

type Client struct {
	url       string
	secret    string
	authUser  string
	authPass  string
	headers   map[string]string
	permanent map[int]bool
	http      *http.Client
	now       func() time.Time
}

func New(o Options) *Client {
	tr := o.Transport
	if tr == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.MaxIdleConnsPerHost = 8
		t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		tr = t
	}
	permanent := map[int]bool{}
	for _, code := range o.PermanentStatus {
		permanent[code] = true
	}
	return &Client{
		url:       o.URL,
		secret:    o.Secret,
		authUser:  o.AuthUser,
		authPass:  o.AuthPass,
		headers:   o.Headers,
		permanent: permanent,
		now:       time.Now,
		http: &http.Client{
			Timeout:   o.Timeout,
			Transport: tr,
			// A redirect is never followed: the body would be re-posted to a
			// place the operator did not configure.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// URL is the endpoint without credentials, safe to log and show.
func (c *Client) URL() string { return c.url }

// Signed reports whether deliveries carry a signature header.
func (c *Client) Signed() bool { return c.secret != "" }

// Result is one attempt.
type Result struct {
	Outcome    Outcome
	StatusCode int    // 0 when no response arrived
	Error      string // empty on success
	Excerpt    string // start of a non-2xx response body
	Duration   time.Duration
}

// Post makes one attempt. The body is sent exactly as given, and it is the
// body that is signed, so a receiver must verify against the raw bytes.
func (c *Client) Post(ctx context.Context, deliveryID, event string, body []byte) Result {
	return c.post(ctx, c.url, deliveryID, event, body)
}

// PostTo is Post against another URL with the same credentials and signing.
// The recipient check uses it.
func (c *Client) PostTo(ctx context.Context, url, event string, body []byte) (status int, response []byte, err error) {
	req, err := c.request(ctx, url, "", event, body)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	response, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, response, err
}

func (c *Client) request(ctx context.Context, url, deliveryID, event string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	timestamp := c.now().Unix()
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set(EventHeader, event)
	if deliveryID != "" {
		req.Header.Set(DeliveryHeader, deliveryID)
	}
	req.Header.Set(TimestampHeader, strconv.FormatInt(timestamp, 10))
	if c.secret != "" {
		req.Header.Set(SignatureHeader, Sign(c.secret, timestamp, body))
	}
	if c.authUser != "" || c.authPass != "" {
		req.SetBasicAuth(c.authUser, c.authPass)
	}
	return req, nil
}

func (c *Client) post(ctx context.Context, url, deliveryID, event string, body []byte) Result {
	start := c.now()
	req, err := c.request(ctx, url, deliveryID, event, body)
	if err != nil {
		return Result{Outcome: Permanent, Error: "the webhook URL is unusable: " + err.Error()}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{Outcome: Transient, Error: c.scrub(err.Error()), Duration: c.now().Sub(start)}
	}
	defer resp.Body.Close()
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, ExcerptLimit))

	r := Result{StatusCode: resp.StatusCode, Duration: c.now().Sub(start)}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		r.Outcome = Delivered
		return r
	case c.permanent[resp.StatusCode]:
		r.Outcome = Permanent
	default:
		r.Outcome = Transient
	}
	r.Error = "the receiver answered " + strconv.Itoa(resp.StatusCode)
	r.Excerpt = strings.ToValidUTF8(string(bytes.TrimSpace(excerpt)), "�")
	return r
}

// scrub removes the password from a transport error, which may quote the URL.
func (c *Client) scrub(s string) string {
	if c.authPass != "" {
		s = strings.ReplaceAll(s, c.authPass, "***")
	}
	return s
}

// Sign returns "sha256=<hex HMAC-SHA256(secret, "<timestamp>.<body>")>".
// A receiver recomputes it over the raw body, compares in constant time, and
// rejects a timestamp more than a few minutes off.
func Sign(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature the way a receiver should.
func Verify(secret, timestamp, signature string, body []byte, now time.Time, maxSkew time.Duration) string {
	switch {
	case secret == "":
		return "unsigned"
	case signature == "" || timestamp == "":
		return "missing"
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return "invalid"
	}
	if !hmac.Equal([]byte(Sign(secret, ts, body)), []byte(signature)) {
		return "invalid"
	}
	if skew := now.Sub(time.Unix(ts, 0)); skew > maxSkew || skew < -maxSkew {
		return "stale"
	}
	return "valid"
}
