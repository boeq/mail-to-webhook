package payload

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/boeq/mail-to-webhook/internal/consumer"
	"github.com/boeq/mail-to-webhook/internal/mailparse"
)

func buildJSON(t *testing.T, raw, recipient, mailFrom string, opt Options) (*Inbound, []byte) {
	t.Helper()
	if opt.FallbackSender == "" {
		opt.FallbackSender = "mailer-daemon@mx.example.org"
	}
	opt.IncludeAttachments = true
	m, _ := mailparse.Parse(strings.NewReader(raw))
	p, err := Build(m, Envelope{MailFrom: mailFrom}, recipient, opt)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if v := consumer.Validate(b); len(v) > 0 {
		t.Fatalf("payload violates consumer rules: %s\n%s", consumer.Violations(v), b)
	}
	return p, b
}

// TestConsumerRulesHoldForHostileMessages walks the cases that would otherwise
// produce a 422 and therefore a bounced message.
func TestConsumerRulesHoldForHostileMessages(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		mailFrom string
		check    func(*testing.T, *Inbound)
	}{
		{
			name:     "bcc only, no To header",
			raw:      "From: a@sender.test\r\nSubject: x\r\n\r\nbody\r\n",
			mailFrom: "a@sender.test",
			check: func(t *testing.T, p *Inbound) {
				if len(p.ToFull) != 1 || p.ToFull[0].Email != "inbound@example.org" {
					t.Errorf("ToFull = %+v, want envelope recipient", p.ToFull)
				}
			},
		},
		{
			name:     "undisclosed recipients group syntax",
			raw:      "From: a@sender.test\r\nTo: undisclosed-recipients:;\r\n\r\nbody\r\n",
			mailFrom: "a@sender.test",
			check: func(t *testing.T, p *Inbound) {
				if len(p.ToFull) != 1 {
					t.Errorf("ToFull = %+v", p.ToFull)
				}
			},
		},
		{
			name:     "From header with no address",
			raw:      "From: Mailer Daemon\r\nTo: c@example.org\r\n\r\nbody\r\n",
			mailFrom: "bounce@sender.test",
			check: func(t *testing.T, p *Inbound) {
				if p.FromFull.Email != "bounce@sender.test" {
					t.Errorf("From = %q, want envelope sender", p.FromFull.Email)
				}
				if p.FromName != "Mailer Daemon" {
					t.Errorf("FromName = %q, display name should survive", p.FromName)
				}
			},
		},
		{
			name:     "null sender bounce, no usable From",
			raw:      "From: <>\r\nTo: c@example.org\r\nSubject: Undelivered\r\n\r\nbody\r\n",
			mailFrom: "",
			check: func(t *testing.T, p *Inbound) {
				if p.FromFull.Email != "mailer-daemon@mx.example.org" {
					t.Errorf("From = %q, want configured fallback", p.FromFull.Email)
				}
			},
		},
		{
			name:     "orphan header line",
			raw:      ": orphan\r\nFrom: a@sender.test\r\nTo: c@example.org\r\n\r\nbody\r\n",
			mailFrom: "a@sender.test",
			check: func(t *testing.T, p *Inbound) {
				for _, h := range p.Headers {
					if h.Name == "" {
						t.Error("header with empty Name survived")
					}
				}
			},
		},
		{
			name:     "unicode SMTPUTF8 address the email rule rejects",
			raw:      "From: пользователь@почта.рф\r\nTo: c@example.org\r\n\r\nbody\r\n",
			mailFrom: "a@sender.test",
			check: func(t *testing.T, p *Inbound) {
				if !consumer.IsEmail(p.FromFull.Email) {
					t.Errorf("From = %q is not acceptable to the consumer", p.FromFull.Email)
				}
			},
		},
		{
			name:     "control characters and NUL in subject",
			raw:      "From: a@sender.test\r\nTo: c@example.org\r\nSubject: bad\x00sub\x07ject\r\n\r\nbody\r\n",
			mailFrom: "a@sender.test",
			check: func(t *testing.T, p *Inbound) {
				if strings.ContainsAny(p.Subject, "\x00\x07") {
					t.Errorf("Subject = %q retains control characters", p.Subject)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := buildJSON(t, tc.raw, "inbound@example.org", tc.mailFrom, Options{})
			tc.check(t, p)
		})
	}
}

const multipartWithEdgeCaseAttachments = "From: a@sender.test\r\n" +
	"To: c@example.org\r\n" +
	"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
	"--B\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
	// zero bytes -> Content would be "" -> fails `required`
	"--B\r\nContent-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"empty.dat\"\r\n\r\n" +
	// no filename anywhere -> Name would be "" -> fails `required`
	"--B\r\nContent-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment\r\nContent-Transfer-Encoding: base64\r\n\r\nZGF0YQ==\r\n" +
	// duplicate names
	"--B\r\nContent-Type: application/pdf; name=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\nb25l\r\n" +
	"--B\r\nContent-Type: application/pdf; name=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\ndHdv\r\n" +
	// RFC 2231 encoded filename with a traversal attempt
	"--B\r\nContent-Type: image/png\r\n" +
	"Content-Disposition: attachment; filename*=utf-8''%2E%2E%2F%2E%2E%2Fcaf%C3%A9.png\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\naW1n\r\n" +
	"--B--\r\n"

func TestAttachmentHandling(t *testing.T) {
	p, _ := buildJSON(t, multipartWithEdgeCaseAttachments, "inbound@example.org", "a@sender.test",
		Options{DedupeAttachmentNames: true})

	if len(p.Attachments) != 4 {
		t.Fatalf("got %d attachments, want 4 (zero-byte one dropped): %+v", len(p.Attachments), names(p))
	}
	got := names(p)
	want := []string{"attachment.bin", "report.pdf", "report-2.pdf", "café.png"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("attachment %d name = %q, want %q", i, got[i], want[i])
		}
	}

	for _, a := range p.Attachments {
		if a.Name == "" || a.Content == "" || a.ContentType == "" {
			t.Errorf("attachment %+v has an empty required field", a)
		}
		if strings.ContainsAny(a.Name, "/\\") {
			t.Errorf("attachment name %q contains a path separator", a.Name)
		}
		if strings.ContainsAny(a.Content, "\r\n ") {
			t.Errorf("attachment %q Content is not single-line base64", a.Name)
		}
		raw, err := base64.StdEncoding.DecodeString(a.Content)
		if err != nil {
			t.Errorf("attachment %q Content is not decodable: %v", a.Name, err)
		}
		if len(raw) != a.ContentLength {
			t.Errorf("attachment %q ContentLength = %d, decoded %d", a.Name, a.ContentLength, len(raw))
		}
	}

	// Round-trip a payload byte-for-byte, the way the consumer will.
	if got := decode(t, p.Attachments[1].Content); got != "one" {
		t.Errorf("report.pdf = %q", got)
	}
	if got := decode(t, p.Attachments[2].Content); got != "two" {
		t.Errorf("report-2.pdf = %q, duplicates must not collide", got)
	}
}

func TestOversizeAttachmentPolicies(t *testing.T) {
	raw := "From: a@sender.test\r\nTo: c@example.org\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--B\r\nContent-Type: application/pdf; name=\"big.pdf\"\r\n\r\n" +
		strings.Repeat("x", 5000) + "\r\n--B--\r\n"

	t.Run("skip", func(t *testing.T) {
		p, _ := buildJSON(t, raw, "inbound@example.org", "a@sender.test",
			Options{MaxAttachBytes: 1000, OversizePolicy: OversizeSkip})
		if len(p.Attachments) != 0 {
			t.Errorf("oversize attachment was not skipped: %+v", names(p))
		}
	})

	t.Run("reject", func(t *testing.T) {
		m, _ := mailparse.Parse(strings.NewReader(raw))
		_, err := Build(m, Envelope{MailFrom: "a@sender.test"}, "inbound@example.org", Options{
			IncludeAttachments: true, MaxAttachBytes: 1000,
			OversizePolicy: OversizeReject, FallbackSender: "mailer-daemon@mx.example.org",
		})
		if err == nil {
			t.Fatal("expected an error so the sender is told")
		}
	})
}

// TestInlineImagesArePreserved covers cid: references in HTML bodies.
func TestInlineImagesArePreserved(t *testing.T) {
	raw := "From: a@sender.test\r\nTo: c@example.org\r\n" +
		"Content-Type: multipart/related; boundary=R\r\n\r\n" +
		"--R\r\nContent-Type: text/html\r\n\r\n<img src=\"cid:logo123\">\r\n" +
		"--R\r\nContent-Type: image/png\r\nContent-ID: <logo123>\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\naW1n\r\n--R--\r\n"

	p, _ := buildJSON(t, raw, "inbound@example.org", "a@sender.test", Options{})
	if len(p.Attachments) != 1 {
		t.Fatalf("attachments = %d", len(p.Attachments))
	}
	if p.Attachments[0].ContentID != "logo123" {
		t.Errorf("ContentID = %q, cannot match cid: reference", p.Attachments[0].ContentID)
	}
}

func names(p *Inbound) []string {
	out := make([]string, 0, len(p.Attachments))
	for _, a := range p.Attachments {
		out = append(out, a.Name)
	}
	return out
}

func decode(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
