// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/boeq/mail-to-webhook/internal/consumer"
)

type Config struct {
	// SMTP listener
	SMTPAddr        string
	SMTPDomain      string // hostname announced in the 220 greeting / EHLO response
	MaxMessageBytes int64
	MaxRecipients   int
	MaxLineBytes    int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration

	// STARTTLS (optional). Both must be set to enable.
	TLSCert string
	TLSKey  string

	// Recipient policy. Empty AllowedDomains == accept every domain.
	AllowedDomains   []string
	AllowedMailboxes []string // optional local-part allowlist, e.g. "inbound,support"

	// Recipient check: ask the consumer whether an address exists before
	// accepting mail for it. Empty URL turns the check off.
	RecipientCheckURL     string
	RecipientCheckTimeout time.Duration
	RecipientRecheck      time.Duration // how long a known address is trusted
	RecipientUnknownTTL   time.Duration // how long an unknown answer is remembered

	// Connection and abuse limits
	MaxConnections         int
	MaxConnectionsPerIP    int
	MaxConnectionsPerIPMin int // new connections per IP per minute
	MaxBadRecipients       int // unknown recipients per session before disconnect
	BlockAfterBadRcpts     int // unknown recipients per IP per 10 minutes before a block
	BlockDuration          time.Duration

	// Webhook delivery
	WebhookURL         string // never carries credentials; see Load
	WebhookTimeout     time.Duration
	WebhookRetries     int // extra attempts inside the SMTP conversation
	WebhookSecret      string
	WebhookAuthUser    string
	WebhookAuthPass    string
	WebhookHeaders     map[string]string
	WebhookMaxAttempts int // attempts of a queued (re-sent) delivery
	WebhookSweep       time.Duration
	PermanentStatus    []int // response codes that bounce rather than retry

	// Payload shaping
	PerRecipient       bool  // one POST per accepted RCPT TO
	IncludeRawEmail    bool  // attach the full RFC822 source as base64
	IncludeAttachments bool  // include attachment bytes in the JSON
	MaxAttachBytes     int64 // largest attachment to forward; 0 = no limit
	FallbackSender     string
	OversizePolicy     string // "skip" or "reject"
	DedupeAttachNames  bool
	StrictCompat       bool // refuse to post a payload that fails the consumer rules

	// Normalisations that make every mail acceptable to a strict consumer.
	EnsureRecipientInTo    bool
	SynthesizeMessageID    bool
	TextFromHTML           bool
	ExpandAttachedMessages bool
	ExpandTNEF             bool

	// History
	DataDir       string
	RetentionDays int
	StoreRaw      bool

	// HTTP: health endpoints and the operator UI
	HTTPAddr   string
	UIEnabled  bool
	UIAuthUser string
	UIAuthPass string

	LogLevel string
}

// Load reads the environment. Every invalid value is reported, not just the first.
func Load() (*Config, error) {
	e := &env{}
	c := &Config{
		SMTPAddr:        e.str("SMTP_ADDR", ":2525"),
		SMTPDomain:      e.str("SMTP_DOMAIN", "inbound.local"),
		MaxMessageBytes: e.int64("MAX_MESSAGE_BYTES", 25<<20),
		MaxRecipients:   e.int("MAX_RECIPIENTS", 50),
		MaxLineBytes:    e.int("MAX_LINE_BYTES", 1<<20),
		ReadTimeout:     e.dur("READ_TIMEOUT", 60*time.Second),
		WriteTimeout:    e.dur("WRITE_TIMEOUT", 60*time.Second),
		TLSCert:         e.str("TLS_CERT", ""),
		TLSKey:          e.str("TLS_KEY", ""),

		AllowedDomains:   e.list("ALLOWED_DOMAINS"),
		AllowedMailboxes: e.list("ALLOWED_MAILBOXES"),

		RecipientCheckURL:     e.str("RECIPIENT_CHECK_URL", ""),
		RecipientCheckTimeout: e.dur("RECIPIENT_CHECK_TIMEOUT", 5*time.Second),
		RecipientRecheck:      e.dur("RECIPIENT_RECHECK", 24*time.Hour),
		RecipientUnknownTTL:   e.dur("RECIPIENT_UNKNOWN_TTL", 5*time.Minute),

		MaxConnections:         e.int("MAX_CONNECTIONS", 50),
		MaxConnectionsPerIP:    e.int("MAX_CONNECTIONS_PER_IP", 10),
		MaxConnectionsPerIPMin: e.int("MAX_CONNECTIONS_PER_IP_PER_MINUTE", 60),
		MaxBadRecipients:       e.int("MAX_BAD_RECIPIENTS", 3),
		BlockAfterBadRcpts:     e.int("BLOCK_AFTER_BAD_RECIPIENTS", 20),
		BlockDuration:          e.dur("BLOCK_DURATION", 15*time.Minute),

		WebhookURL:         e.str("WEBHOOK_URL", ""),
		WebhookTimeout:     e.dur("WEBHOOK_TIMEOUT", 20*time.Second),
		WebhookRetries:     e.int("WEBHOOK_RETRIES", 2),
		WebhookSecret:      e.str("WEBHOOK_SECRET", ""),
		WebhookAuthUser:    e.str("WEBHOOK_AUTH_USER", ""),
		WebhookAuthPass:    e.str("WEBHOOK_AUTH_PASS", ""),
		WebhookHeaders:     e.pairs("WEBHOOK_HEADERS"),
		WebhookMaxAttempts: e.int("WEBHOOK_MAX_ATTEMPTS", 6),
		WebhookSweep:       time.Duration(e.int("WEBHOOK_SWEEP_SECONDS", 15)) * time.Second,
		PermanentStatus:    e.ints("PERMANENT_STATUS_CODES", []int{400, 406, 410, 413, 415, 422}),

		PerRecipient:       e.bool("PER_RECIPIENT", false),
		IncludeRawEmail:    e.bool("INCLUDE_RAW_EMAIL", false),
		IncludeAttachments: e.bool("INCLUDE_ATTACHMENTS", true),
		MaxAttachBytes:     e.int64("MAX_ATTACHMENT_BYTES", 0),
		FallbackSender:     e.str("FALLBACK_SENDER", ""),
		OversizePolicy:     strings.ToLower(e.str("OVERSIZE_ATTACHMENT_POLICY", "skip")),
		DedupeAttachNames:  e.bool("DEDUPE_ATTACHMENT_NAMES", true),
		StrictCompat:       e.bool("STRICT_COMPAT", true),

		EnsureRecipientInTo:    e.bool("ENSURE_RECIPIENT_IN_TO", true),
		SynthesizeMessageID:    e.bool("SYNTHESIZE_MESSAGE_ID", true),
		TextFromHTML:           e.bool("TEXT_FROM_HTML", true),
		ExpandAttachedMessages: e.bool("EXPAND_ATTACHED_MESSAGES", true),
		ExpandTNEF:             e.bool("EXPAND_TNEF", true),

		DataDir:       e.str("DATA_DIR", "/data"),
		RetentionDays: e.int("RETENTION_DAYS", 30),
		StoreRaw:      e.bool("STORE_RAW", true),

		HTTPAddr:   e.str("HTTP_ADDR", ":8080"),
		UIEnabled:  e.bool("UI_ENABLED", true),
		UIAuthUser: e.str("UI_AUTH_USER", ""),
		UIAuthPass: e.str("UI_AUTH_PASS", ""),

		LogLevel: strings.ToLower(e.str("LOG_LEVEL", "info")),
	}

	c.splitWebhookCredentials(e)
	c.RecipientCheckURL = c.withoutCredentials(c.RecipientCheckURL)

	if c.WebhookURL == "" {
		e.fail("WEBHOOK_URL is required")
	} else if !isHTTP(c.WebhookURL) {
		e.fail("WEBHOOK_URL must be an http(s) URL")
	}
	if c.RecipientCheckURL != "" && !isHTTP(c.RecipientCheckURL) {
		e.fail("RECIPIENT_CHECK_URL must be an http(s) URL")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		e.fail("TLS_CERT and TLS_KEY must be set together")
	}
	if c.FallbackSender == "" {
		c.FallbackSender = "mailer-daemon@" + c.SMTPDomain
	}
	if !consumer.IsEmail(c.FallbackSender) {
		e.fail(fmt.Sprintf("FALLBACK_SENDER %q is not a valid address; set it explicitly (SMTP_DOMAIN %q does not produce one)",
			c.FallbackSender, c.SMTPDomain))
	}
	if c.OversizePolicy != "skip" && c.OversizePolicy != "reject" {
		e.fail(`OVERSIZE_ATTACHMENT_POLICY must be "skip" or "reject"`)
	}
	if (c.UIAuthUser == "") != (c.UIAuthPass == "") {
		e.fail("UI_AUTH_USER and UI_AUTH_PASS must be set together")
	}
	if c.WebhookMaxAttempts < 1 {
		e.fail("WEBHOOK_MAX_ATTEMPTS must be at least 1")
	}
	if c.WebhookSweep < time.Second {
		e.fail("WEBHOOK_SWEEP_SECONDS must be at least 1")
	}
	if c.RetentionDays < 1 {
		e.fail("RETENTION_DAYS must be at least 1")
	}
	for i, d := range c.AllowedDomains {
		c.AllowedDomains[i] = strings.ToLower(strings.TrimPrefix(d, "@"))
	}
	for i, m := range c.AllowedMailboxes {
		c.AllowedMailboxes[i] = strings.ToLower(m)
	}

	if len(e.errs) > 0 {
		return nil, errors.New(strings.Join(e.errs, "; "))
	}
	return c, nil
}

// splitWebhookCredentials moves "https://user:pass@host/path" credentials out
// of the URL, so the URL can be logged and shown without leaking them.
func (c *Config) splitWebhookCredentials(e *env) {
	if c.WebhookURL == "" {
		return
	}
	u, err := url.Parse(c.WebhookURL)
	if err != nil {
		e.fail("WEBHOOK_URL is not a valid URL")
		return
	}
	if u.User == nil {
		return
	}
	if c.WebhookAuthUser == "" && c.WebhookAuthPass == "" {
		c.WebhookAuthUser = u.User.Username()
		c.WebhookAuthPass, _ = u.User.Password()
	}
	u.User = nil
	c.WebhookURL = u.String()
}

// withoutCredentials strips "user:pass@" from the recipient check URL. The
// check is made with the webhook's credentials; when none were given, the
// ones written here are used for both.
func (c *Config) withoutCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if c.WebhookAuthUser == "" && c.WebhookAuthPass == "" {
		c.WebhookAuthUser = u.User.Username()
		c.WebhookAuthPass, _ = u.User.Password()
	}
	u.User = nil
	return u.String()
}

// Secret reports whether a setting holds a credential that must never be shown.
func Secret(key string) bool {
	for _, suffix := range []string{"_SECRET", "_PASS", "_TOKEN", "_KEY"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

// Setting is one line of the read-only settings page.
type Setting struct {
	Key   string
	Value string // empty for a secret
	Set   bool
}

// Settings lists the effective configuration. Secrets only say whether they are set.
func (c *Config) Settings() []Setting {
	values := map[string]string{
		"SMTP_ADDR":                         c.SMTPAddr,
		"SMTP_DOMAIN":                       c.SMTPDomain,
		"MAX_MESSAGE_BYTES":                 strconv.FormatInt(c.MaxMessageBytes, 10),
		"MAX_RECIPIENTS":                    strconv.Itoa(c.MaxRecipients),
		"MAX_LINE_BYTES":                    strconv.Itoa(c.MaxLineBytes),
		"READ_TIMEOUT":                      c.ReadTimeout.String(),
		"WRITE_TIMEOUT":                     c.WriteTimeout.String(),
		"TLS_CERT":                          c.TLSCert,
		"TLS_KEY":                           c.TLSKey,
		"ALLOWED_DOMAINS":                   strings.Join(c.AllowedDomains, ","),
		"ALLOWED_MAILBOXES":                 strings.Join(c.AllowedMailboxes, ","),
		"RECIPIENT_CHECK_URL":               c.RecipientCheckURL,
		"RECIPIENT_CHECK_TIMEOUT":           c.RecipientCheckTimeout.String(),
		"RECIPIENT_RECHECK":                 c.RecipientRecheck.String(),
		"RECIPIENT_UNKNOWN_TTL":             c.RecipientUnknownTTL.String(),
		"MAX_CONNECTIONS":                   strconv.Itoa(c.MaxConnections),
		"MAX_CONNECTIONS_PER_IP":            strconv.Itoa(c.MaxConnectionsPerIP),
		"MAX_CONNECTIONS_PER_IP_PER_MINUTE": strconv.Itoa(c.MaxConnectionsPerIPMin),
		"MAX_BAD_RECIPIENTS":                strconv.Itoa(c.MaxBadRecipients),
		"BLOCK_AFTER_BAD_RECIPIENTS":        strconv.Itoa(c.BlockAfterBadRcpts),
		"BLOCK_DURATION":                    c.BlockDuration.String(),
		"WEBHOOK_URL":                       c.WebhookURL,
		"WEBHOOK_TIMEOUT":                   c.WebhookTimeout.String(),
		"WEBHOOK_RETRIES":                   strconv.Itoa(c.WebhookRetries),
		"WEBHOOK_SECRET":                    c.WebhookSecret,
		"WEBHOOK_AUTH_USER":                 c.WebhookAuthUser,
		"WEBHOOK_AUTH_PASS":                 c.WebhookAuthPass,
		"WEBHOOK_HEADERS":                   headerNames(c.WebhookHeaders),
		"WEBHOOK_MAX_ATTEMPTS":              strconv.Itoa(c.WebhookMaxAttempts),
		"WEBHOOK_SWEEP_SECONDS":             strconv.Itoa(int(c.WebhookSweep / time.Second)),
		"PERMANENT_STATUS_CODES":            joinInts(c.PermanentStatus),
		"PER_RECIPIENT":                     strconv.FormatBool(c.PerRecipient),
		"INCLUDE_RAW_EMAIL":                 strconv.FormatBool(c.IncludeRawEmail),
		"INCLUDE_ATTACHMENTS":               strconv.FormatBool(c.IncludeAttachments),
		"MAX_ATTACHMENT_BYTES":              strconv.FormatInt(c.MaxAttachBytes, 10),
		"FALLBACK_SENDER":                   c.FallbackSender,
		"OVERSIZE_ATTACHMENT_POLICY":        c.OversizePolicy,
		"DEDUPE_ATTACHMENT_NAMES":           strconv.FormatBool(c.DedupeAttachNames),
		"STRICT_COMPAT":                     strconv.FormatBool(c.StrictCompat),
		"ENSURE_RECIPIENT_IN_TO":            strconv.FormatBool(c.EnsureRecipientInTo),
		"SYNTHESIZE_MESSAGE_ID":             strconv.FormatBool(c.SynthesizeMessageID),
		"TEXT_FROM_HTML":                    strconv.FormatBool(c.TextFromHTML),
		"EXPAND_ATTACHED_MESSAGES":          strconv.FormatBool(c.ExpandAttachedMessages),
		"EXPAND_TNEF":                       strconv.FormatBool(c.ExpandTNEF),
		"DATA_DIR":                          c.DataDir,
		"RETENTION_DAYS":                    strconv.Itoa(c.RetentionDays),
		"STORE_RAW":                         strconv.FormatBool(c.StoreRaw),
		"HTTP_ADDR":                         c.HTTPAddr,
		"UI_ENABLED":                        strconv.FormatBool(c.UIEnabled),
		"UI_AUTH_USER":                      c.UIAuthUser,
		"UI_AUTH_PASS":                      c.UIAuthPass,
		"LOG_LEVEL":                         c.LogLevel,
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]Setting, 0, len(keys))
	for _, k := range keys {
		s := Setting{Key: k, Set: values[k] != ""}
		if !Secret(k) {
			s.Value = values[k]
		}
		out = append(out, s)
	}
	return out
}

// headerNames lists the names only: a static header is usually a credential.
func headerNames(h map[string]string) string {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func joinInts(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

func isHTTP(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// env reads variables and remembers every value it could not parse.
type env struct{ errs []string }

func (e *env) fail(msg string) { e.errs = append(e.errs, msg) }

func (e *env) raw(k string) (string, bool) {
	v, ok := os.LookupEnv(k)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (e *env) str(k, def string) string {
	if v, ok := e.raw(k); ok {
		return v
	}
	return def
}

func (e *env) int64(k string, def int64) int64 {
	v, ok := e.raw(k)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		e.fail(k + " must be a non-negative whole number")
		return def
	}
	return n
}

func (e *env) int(k string, def int) int { return int(e.int64(k, int64(def))) }

func (e *env) bool(k string, def bool) bool {
	v, ok := e.raw(k)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.fail(k + " must be true or false")
		return def
	}
	return b
}

func (e *env) dur(k string, def time.Duration) time.Duration {
	v, ok := e.raw(k)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		e.fail(k + ` must be a duration such as "30s" or "5m"`)
		return def
	}
	return d
}

func (e *env) list(k string) []string {
	v, ok := e.raw(k)
	if !ok {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (e *env) ints(k string, def []int) []int {
	list := e.list(k)
	if len(list) == 0 {
		return def
	}
	out := make([]int, 0, len(list))
	for _, v := range list {
		n, err := strconv.Atoi(v)
		if err != nil {
			e.fail(k + " must be a comma-separated list of status codes")
			return def
		}
		out = append(out, n)
	}
	return out
}

func (e *env) pairs(k string) map[string]string {
	out := map[string]string{}
	for _, kv := range e.list(k) {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(name) == "" {
			e.fail(k + ` must look like "Name=value,Other=value"`)
			continue
		}
		out[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return out
}
