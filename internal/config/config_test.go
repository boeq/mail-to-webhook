package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "https://app.example.org/hook")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SMTPAddr != ":2525" || c.HTTPAddr != ":8080" || c.MaxMessageBytes != 25<<20 || c.MaxLineBytes != 1<<20 {
		t.Errorf("listener defaults: %+v", c)
	}
	// One POST per message, and no attachment is dropped for its size.
	if c.PerRecipient || c.MaxAttachBytes != 0 || !c.StrictCompat {
		t.Errorf("payload defaults: per-recipient=%v max-attachment=%d strict=%v", c.PerRecipient, c.MaxAttachBytes, c.StrictCompat)
	}
	if !c.EnsureRecipientInTo || !c.SynthesizeMessageID || !c.TextFromHTML || !c.ExpandAttachedMessages || !c.ExpandTNEF {
		t.Error("every normalisation is on by default")
	}
	if c.WebhookMaxAttempts != 6 || c.WebhookSweep != 15*time.Second || c.RetentionDays != 30 || !c.StoreRaw {
		t.Errorf("delivery defaults: %+v", c)
	}
	if c.RecipientCheckURL != "" || c.RecipientRecheck != 24*time.Hour || c.RecipientUnknownTTL != 5*time.Minute {
		t.Errorf("recipient check defaults: %+v", c)
	}
	if c.FallbackSender != "mailer-daemon@inbound.local" {
		t.Errorf("fallback sender = %q", c.FallbackSender)
	}
	if got := c.PermanentStatus; len(got) != 6 || got[5] != 422 {
		t.Errorf("permanent status codes = %v", got)
	}
}

// Credentials in the URL are taken out of it, so the URL can be shown.
func TestCredentialsInTheWebhookURL(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "https://hook:s3cr3t-token@app.example.org/inbound/mail")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.WebhookURL != "https://app.example.org/inbound/mail" {
		t.Errorf("url = %q", c.WebhookURL)
	}
	if c.WebhookAuthUser != "hook" || c.WebhookAuthPass != "s3cr3t-token" {
		t.Errorf("auth = %q / %q", c.WebhookAuthUser, c.WebhookAuthPass)
	}

	t.Setenv("WEBHOOK_AUTH_USER", "explicit")
	t.Setenv("WEBHOOK_AUTH_PASS", "wins")
	c, _ = Load()
	if c.WebhookAuthUser != "explicit" || strings.Contains(c.WebhookURL, "@") {
		t.Errorf("explicit credentials: %q, url %q", c.WebhookAuthUser, c.WebhookURL)
	}
}

func TestCredentialsInTheRecipientCheckURL(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "https://app.example.org/hook")
	t.Setenv("RECIPIENT_CHECK_URL", "https://hook:check-token@app.example.org/recipients")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.RecipientCheckURL != "https://app.example.org/recipients" {
		t.Errorf("url = %q", c.RecipientCheckURL)
	}
	if c.WebhookAuthUser != "hook" || c.WebhookAuthPass != "check-token" {
		t.Errorf("auth = %q / %q", c.WebhookAuthUser, c.WebhookAuthPass)
	}
	for _, s := range c.Settings() {
		if strings.Contains(s.Value, "check-token") {
			t.Errorf("%s shows the credential", s.Key)
		}
	}
}

// A server can be known by more than one hostname. The first is the one it
// announces and makes its own addresses with.
func TestSeveralHostnames(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "https://app.example.org/hook")
	t.Setenv("SMTP_DOMAIN", " MX.Example.org , mx.example.net.,mx.example.org,")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.SMTPDomains) != 2 || c.SMTPDomains[0] != "mx.example.org" || c.SMTPDomains[1] != "mx.example.net" {
		t.Errorf("hostnames = %q", c.SMTPDomains)
	}
	if c.SMTPDomain != "mx.example.org" || c.FallbackSender != "mailer-daemon@mx.example.org" {
		t.Errorf("primary = %q, fallback sender = %q", c.SMTPDomain, c.FallbackSender)
	}
	for _, s := range c.Settings() {
		if s.Key == "SMTP_DOMAIN" && s.Value != "mx.example.org,mx.example.net" {
			t.Errorf("SMTP_DOMAIN setting = %q", s.Value)
		}
	}

	// With ACME every one of them must be a name a CA can issue for.
	t.Setenv("ACME_ENABLED", "true")
	if _, err := Load(); err != nil {
		t.Errorf("two public hostnames: %v", err)
	}
	t.Setenv("SMTP_DOMAIN", "mx.example.org,mail.local")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), `not "mail.local"`) {
		t.Errorf("one private hostname in the list: %v", err)
	}
}

// A CA only issues for a public hostname, so anything else is refused at
// startup rather than failing an order later.
func TestACMESettings(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "https://app.example.org/hook")
	t.Setenv("ACME_ENABLED", "true")

	for _, host := range []string{"", "inbound.local", "mx", "192.0.2.10", "mail.localhost", "server.local"} {
		t.Setenv("SMTP_DOMAIN", host)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ACME_ENABLED needs SMTP_DOMAIN") {
			t.Errorf("SMTP_DOMAIN %q: %v", host, err)
		}
	}

	t.Setenv("SMTP_DOMAIN", "mx.example.org")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.ACMEEnabled || c.ACMEHTTPAddr != ":8089" || c.ACMEDirectoryURL != "" {
		t.Errorf("acme defaults: %+v", c)
	}

	t.Setenv("ACME_DIRECTORY_URL", "acme-staging.example")
	t.Setenv("ACME_EMAIL", "not an address")
	t.Setenv("TLS_CERT", "/certs/fullchain.pem")
	t.Setenv("TLS_KEY", "/certs/privkey.pem")
	_, err = Load()
	for _, want := range []string{"ACME_DIRECTORY_URL", "ACME_EMAIL", "ACME_ENABLED and TLS_CERT"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestEveryMistakeIsReported(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "ftp://nope")
	t.Setenv("MAX_RECIPIENTS", "many")
	t.Setenv("WEBHOOK_TIMEOUT", "20")
	t.Setenv("PER_RECIPIENT", "perhaps")
	t.Setenv("OVERSIZE_ATTACHMENT_POLICY", "shrink")
	t.Setenv("TLS_CERT", "/certs/fullchain.pem")
	t.Setenv("UI_AUTH_USER", "admin")
	t.Setenv("SMTP_DOMAIN", "localhost")
	t.Setenv("PERMANENT_STATUS_CODES", "400,teapot")
	t.Setenv("WEBHOOK_HEADERS", "NoEquals")

	_, err := Load()
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"WEBHOOK_URL must be an http(s) URL", "MAX_RECIPIENTS", "WEBHOOK_TIMEOUT", "PER_RECIPIENT",
		"OVERSIZE_ATTACHMENT_POLICY", "TLS_CERT and TLS_KEY", "UI_AUTH_USER and UI_AUTH_PASS",
		"FALLBACK_SENDER", "PERMANENT_STATUS_CODES", "WEBHOOK_HEADERS",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}

	t.Setenv("WEBHOOK_URL", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "WEBHOOK_URL is required") {
		t.Errorf("missing URL: %v", err)
	}
}

func TestListsAreNormalised(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "http://127.0.0.1:8000/inbound")
	t.Setenv("ALLOWED_DOMAINS", " Example.ORG , @inbound.example.org ,")
	t.Setenv("ALLOWED_MAILBOXES", "Inbound")
	t.Setenv("WEBHOOK_HEADERS", "X-Api-Key=abc, X-Env = prod")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedDomains) != 2 || c.AllowedDomains[0] != "example.org" || c.AllowedDomains[1] != "inbound.example.org" {
		t.Errorf("domains = %q", c.AllowedDomains)
	}
	if c.AllowedMailboxes[0] != "inbound" {
		t.Errorf("mailboxes = %q", c.AllowedMailboxes)
	}
	if c.WebhookHeaders["X-Api-Key"] != "abc" || c.WebhookHeaders["X-Env"] != "prod" {
		t.Errorf("headers = %v", c.WebhookHeaders)
	}
}

func TestSettingsNeverCarryASecret(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "https://hook:url-token@app.example.org/hook")
	t.Setenv("WEBHOOK_SECRET", "signing-secret")
	t.Setenv("UI_AUTH_USER", "admin")
	t.Setenv("UI_AUTH_PASS", "ui-password")
	t.Setenv("WEBHOOK_HEADERS", "X-Api-Key=header-secret")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]Setting{}
	for _, s := range c.Settings() {
		seen[s.Key] = s
		for _, secret := range []string{"url-token", "signing-secret", "ui-password", "header-secret"} {
			if strings.Contains(s.Value, secret) {
				t.Errorf("%s shows a secret: %q", s.Key, s.Value)
			}
		}
	}
	for _, key := range []string{"WEBHOOK_SECRET", "WEBHOOK_AUTH_PASS", "UI_AUTH_PASS"} {
		if s := seen[key]; !s.Set || s.Value != "" {
			t.Errorf("%s = %+v, want set and hidden", key, s)
		}
	}
	if s := seen["WEBHOOK_HEADERS"]; s.Value != "X-Api-Key" {
		t.Errorf("WEBHOOK_HEADERS = %q, want the names only", s.Value)
	}
	if s := seen["WEBHOOK_URL"]; s.Value != "https://app.example.org/hook" {
		t.Errorf("WEBHOOK_URL = %q", s.Value)
	}
	if s := seen["TLS_KEY"]; s.Set {
		t.Errorf("TLS_KEY = %+v, want not set", s)
	}
}

// Every setting is in .env.example and in docs/configuration.md, and neither
// names one that does not exist.
func TestSettingsAreDocumented(t *testing.T) {
	t.Setenv("WEBHOOK_URL", "https://app.example.org/hook")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, s := range c.Settings() {
		known[s.Key] = true
	}

	for file, pattern := range map[string]string{
		"../../.env.example":          `(?m)^([A-Z][A-Z0-9_]+)=`,
		"../../docs/configuration.md": "`([A-Z][A-Z0-9_]{3,})`",
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		documented := map[string]bool{}
		for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(string(raw), -1) {
			documented[m[1]] = true
		}
		for key := range known {
			if !documented[key] {
				t.Errorf("%s does not mention %s", file, key)
			}
		}
		for key := range documented {
			if !known[key] {
				t.Errorf("%s mentions %s, which is not a setting", file, key)
			}
		}
	}
}
