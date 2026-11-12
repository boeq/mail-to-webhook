package payload

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/boeq/mail-to-webhook/internal/consumer"
	"github.com/boeq/mail-to-webhook/internal/mailparse"
)

var received = time.Date(2026, 11, 12, 17, 4, 5, 0, time.UTC)

func build(t *testing.T, raw, recipient string, opt Options) *Inbound {
	t.Helper()
	if opt.FallbackSender == "" {
		opt.FallbackSender = "mailer-daemon@mx.example.org"
	}
	m, _ := mailparse.Parse(strings.NewReader(raw))
	p, err := Build(m, Envelope{MailFrom: "a@sender.test", ReceivedAt: received}, recipient, opt)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if v := consumer.Validate(body); len(v) > 0 {
		t.Fatalf("payload violates consumer rules: %s", consumer.Violations(v))
	}
	return p
}

func emails(list []Address) string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Email)
	}
	return strings.Join(out, ",")
}

// A consumer that routes on To and Cc only recognises mail that names one of
// its addresses there. A blind copy and an auto-forward do not.
func TestEnvelopeRecipientIsAddedToToFull(t *testing.T) {
	const box = "AbC123.purchase@inbound.example.org"
	cases := []struct {
		name    string
		headers string
		opt     Options
		wantTo  string
	}{
		{"forwarded: To names someone else", "To: invoices@customer.test\r\n", Options{EnsureRecipientInTo: true}, "invoices@customer.test," + box},
		{"blind copy: no To at all", "", Options{EnsureRecipientInTo: true}, box},
		{"already in To", "To: Inbox <" + box + ">\r\n", Options{EnsureRecipientInTo: true}, box},
		{"already in Cc", "To: x@customer.test\r\nCc: " + box + "\r\n", Options{EnsureRecipientInTo: true}, "x@customer.test"},
		{"another case is another address", "To: abc123.purchase@inbound.example.org\r\n", Options{EnsureRecipientInTo: true}, "abc123.purchase@inbound.example.org," + box},
		{"one post for several recipients", "To: x@customer.test\r\n",
			Options{EnsureRecipientInTo: true, AllRecipients: []string{box, "second@inbound.example.org"}},
			"x@customer.test," + box + ",second@inbound.example.org"},
		{"switched off", "To: invoices@customer.test\r\n", Options{}, "invoices@customer.test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := build(t, "From: a@sender.test\r\n"+tc.headers+"Subject: x\r\n\r\nbody\r\n", box, tc.opt)
			if got := emails(p.ToFull); got != tc.wantTo {
				t.Errorf("ToFull = %s, want %s", got, tc.wantTo)
			}
			if p.OriginalRecipient != box {
				t.Errorf("OriginalRecipient = %q", p.OriginalRecipient)
			}
		})
	}
}

// The consumer hands Date to a date parser; what it cannot read is a server
// error there, and the sender retries a server error for days.
func TestDateIsAlwaysReadable(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
		noted  bool
	}{
		{"rfc 2822", "Date: Sun, 16 Aug 2026 09:20:00 +0200\r\n", "Sun, 16 Aug 2026 09:20:00 +0200", false},
		{"with a comment", "Date: Sun, 16 Aug 2026 09:20:00 +0000 (UTC)\r\n", "Sun, 16 Aug 2026 09:20:00 +0000", false},
		{"without the weekday", "Date: 16 Aug 2026 09:20:00 -0500\r\n", "Sun, 16 Aug 2026 09:20:00 -0500", false},
		{"missing", "", "Thu, 12 Nov 2026 17:04:05 +0000", true},
		{"free text", "Date: yesterday afternoon\r\n", "Thu, 12 Nov 2026 17:04:05 +0000", true},
		{"a date that does not exist", "Date: Mon, 32 Foo 2026 25:61:00 +0000\r\n", "Thu, 12 Nov 2026 17:04:05 +0000", true},
		{"empty", "Date:\r\n", "Thu, 12 Nov 2026 17:04:05 +0000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := build(t, "From: a@sender.test\r\nTo: c@example.org\r\n"+tc.header+"\r\nbody\r\n", "c@example.org", Options{})
			if p.Date != tc.want {
				t.Errorf("Date = %q, want %q", p.Date, tc.want)
			}
			if _, err := time.Parse(time.RFC1123Z, p.Date); err != nil {
				t.Errorf("Date %q is not RFC 2822: %v", p.Date, err)
			}
			noted := strings.Contains(strings.Join(p.Notes, "|"), "Date header")
			if noted != tc.noted {
				t.Errorf("notes = %q, noted = %v", p.Notes, noted)
			}
		})
	}

	// The header itself is passed on untouched.
	p := build(t, "From: a@sender.test\r\nTo: c@example.org\r\nDate: yesterday afternoon\r\n\r\nbody\r\n", "c@example.org", Options{})
	for _, h := range p.Headers {
		if h.Name == "Date" && h.Value != "yesterday afternoon" {
			t.Errorf("Date header was rewritten to %q", h.Value)
		}
	}
}

// A consumer that deduplicates on Message-ID stores a repeated attempt twice
// when the mail has none.
func TestMessageIDIsNeverMissing(t *testing.T) {
	opt := Options{SynthesizeMessageID: true, Domain: "mx.example.org"}
	messageIDs := func(p *Inbound) []string {
		var out []string
		for _, h := range p.Headers {
			if strings.EqualFold(h.Name, "Message-ID") {
				out = append(out, h.Value)
			}
		}
		return out
	}
	const mail = "From: a@sender.test\r\nTo: c@example.org\r\nSubject: one\r\n\r\nbody\r\n"

	first := build(t, mail, "c@example.org", opt)
	again := build(t, mail, "c@example.org", opt)
	other := build(t, strings.Replace(mail, "one", "two", 1), "c@example.org", opt)

	if !strings.HasPrefix(first.MessageID, "m2w.") || !strings.HasSuffix(first.MessageID, "@mx.example.org") {
		t.Errorf("MessageID = %q", first.MessageID)
	}
	if first.MessageID != again.MessageID {
		t.Error("the same message must get the same id on every attempt")
	}
	if first.MessageID == other.MessageID {
		t.Error("different messages must get different ids")
	}
	if got := messageIDs(first); len(got) != 1 || got[0] != "<"+first.MessageID+">" {
		t.Errorf("Message-ID headers = %q", got)
	}

	// An empty header would be read before the one that is added.
	empty := build(t, "From: a@sender.test\r\nTo: c@example.org\r\nMessage-ID: <>\r\n\r\nbody\r\n", "c@example.org", opt)
	if got := messageIDs(empty); len(got) != 1 || got[0] != "<"+empty.MessageID+">" {
		t.Errorf("Message-ID headers = %q", got)
	}

	kept := build(t, "From: a@sender.test\r\nTo: c@example.org\r\nMessage-Id: <real@sender.test>\r\n\r\nbody\r\n", "c@example.org", opt)
	if kept.MessageID != "real@sender.test" || len(kept.Notes) != 1 {
		t.Errorf("MessageID = %q, notes = %q", kept.MessageID, kept.Notes)
	}

	off := build(t, mail, "c@example.org", Options{})
	if off.MessageID != "" || len(messageIDs(off)) != 0 {
		t.Errorf("switched off: MessageID = %q", off.MessageID)
	}
}

func TestSummarise(t *testing.T) {
	raw := "From: =?utf-8?q?Andr=C3=A9?= <andre@sender.test>\r\nTo: Inbox <box@example.org>\r\n" +
		"Subject: =?utf-8?q?Caf=C3=A9?= invoice\r\nMessage-ID: <id-1@sender.test>\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--B\r\nContent-Type: application/pdf; name=\"../invoice.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQ=\r\n" +
		"--B\r\nContent-Type: application/pdf; name=\"empty.pdf\"\r\n\r\n" +
		"--B--\r\n"
	m, _ := mailparse.Parse(strings.NewReader(raw))
	s := Summarise(m)

	if s.FromEmail != "andre@sender.test" || s.FromName != "André" || s.Subject != "Café invoice" || s.MessageID != "id-1@sender.test" {
		t.Errorf("summary = %+v", s)
	}
	if len(s.Attachments) != 1 || s.Attachments[0].Name != "invoice.pdf" || s.Attachments[0].Size != 8 {
		t.Errorf("attachments = %+v", s.Attachments)
	}
}
