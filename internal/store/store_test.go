package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func record(t *testing.T, s *Store, raw string, rcpts ...string) *Message {
	t.Helper()
	m, _, err := s.RecordMessage(Message{ReceivedAt: t0, MailFrom: "a@sender.test", RcptTo: rcpts, Subject: raw, Size: len(raw)}, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func attempt(t *testing.T, s *Store, d *Delivery, status string, next *time.Time) {
	t.Helper()
	d.Attempt++
	if err := s.RecordAttempt(d.ID, Attempt{Attempt: d.Attempt, At: t0, StatusCode: 503, Error: "the receiver answered 503"}, status, "busy", next); err != nil {
		t.Fatal(err)
	}
}

// A sender that got a 451 offers the same mail again. It is the same message.
func TestASenderRetryIsTheSameMessage(t *testing.T) {
	s := open(t)
	first := record(t, s, "mail one", "box@example.org")

	again, seenBefore, err := s.RecordMessage(Message{ReceivedAt: t0.Add(time.Hour), MailFrom: "a@sender.test", RcptTo: []string{"box@example.org"}}, []byte("mail one"))
	if err != nil {
		t.Fatal(err)
	}
	if !seenBefore || again.ID != first.ID || again.SeenCount != 2 {
		t.Errorf("retry: seenBefore=%v id=%s (first %s) seen=%d", seenBefore, again.ID, first.ID, again.SeenCount)
	}
	if !again.ReceivedAt.Equal(t0) || !again.LastSeenAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("times: received %s, last seen %s", again.ReceivedAt, again.LastSeenAt)
	}

	for name, other := range map[string]*Message{
		"other content":   record(t, s, "mail two", "box@example.org"),
		"other recipient": record(t, s, "mail one", "other@example.org"),
	} {
		if other.ID == first.ID {
			t.Errorf("%s was taken for the same message", name)
		}
	}

	raw, err := s.ReadRaw(first)
	if err != nil || string(raw) != "mail one" {
		t.Errorf("raw = %q, %v", raw, err)
	}
}

func TestDeliveryKeepsItsIDAcrossSenderRetries(t *testing.T) {
	s := open(t)
	m := record(t, s, "mail", "box@example.org")

	d, err := s.SMTPDelivery(m.ID, "box@example.org", "message.received", "https://app.test/hook", t0)
	if err != nil {
		t.Fatal(err)
	}
	attempt(t, s, d, StatusDeferred, nil)

	again, _ := s.SMTPDelivery(m.ID, "box@example.org", "message.received", "https://app.test/hook", t0)
	if again.ID != d.ID || again.Attempt != 1 || again.Status != StatusDeferred {
		t.Errorf("second transaction got %+v, want the deferred delivery %s", again, d.ID)
	}

	attempt(t, s, again, StatusDelivered, nil)
	if done, _ := s.Delivered(m.ID, "box@example.org"); !done {
		t.Error("Delivered = false after a delivered attempt")
	}
	if done, _ := s.Delivered(m.ID, "other@example.org"); done {
		t.Error("Delivered = true for a recipient that has no delivery")
	}

	attempts, _ := s.AttemptsFor(d.ID)
	if len(attempts) != 2 || attempts[1].Attempt != 2 || attempts[0].StatusCode != 503 {
		t.Errorf("attempts = %+v", attempts)
	}
	stored, _ := s.GetDelivery(d.ID)
	if stored.DeliveredAt == nil || stored.ResponseExcerpt != "busy" {
		t.Errorf("delivery = %+v", stored)
	}
}

// The message shows the worst state among the latest delivery of each recipient.
func TestMessageStatusFollowsItsDeliveries(t *testing.T) {
	s := open(t)
	m := record(t, s, "mail", "a@example.org", "b@example.org")
	status := func() string {
		got, _ := s.GetMessage(m.ID)
		return got.Status
	}

	a, _ := s.SMTPDelivery(m.ID, "a@example.org", "e", "u", t0)
	if status() != StatusPending {
		t.Errorf("with a delivery in progress: %s", status())
	}
	attempt(t, s, a, StatusDelivered, nil)
	if status() != StatusDelivered {
		t.Errorf("one delivered: %s", status())
	}

	b, _ := s.SMTPDelivery(m.ID, "b@example.org", "e", "u", t0)
	attempt(t, s, b, StatusFailed, nil)
	if status() != StatusFailed {
		t.Errorf("one delivered, one failed: %s", status())
	}

	// A resend that succeeds supersedes the failure for that recipient.
	resend, _ := s.CreateResend(m.ID, "b@example.org", "e", "u", t0)
	if status() != StatusPending {
		t.Errorf("resend queued: %s", status())
	}
	attempt(t, s, resend, StatusDelivered, nil)
	if status() != StatusDelivered {
		t.Errorf("resend delivered: %s", status())
	}
}

func TestDueDeliveriesOneAtATimePerMessage(t *testing.T) {
	s := open(t)
	m := record(t, s, "mail", "box@example.org")
	other := record(t, s, "other mail", "box@example.org")

	first, _ := s.CreateResend(m.ID, "box@example.org", "e", "u", t0)
	second, _ := s.CreateResend(m.ID, "box@example.org", "e", "u", t0.Add(time.Second))
	third, _ := s.CreateResend(other.ID, "box@example.org", "e", "u", t0.Add(2*time.Second))
	inSMTP, _ := s.SMTPDelivery(other.ID, "x@example.org", "e", "u", t0) // not the sweeper's business

	ids := func(at time.Time) map[string]bool {
		due, err := s.DueDeliveries(at, 10)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, d := range due {
			out[d.ID] = true
		}
		return out
	}

	due := ids(t0.Add(time.Minute))
	if !due[first.ID] || !due[third.ID] || due[second.ID] || due[inSMTP.ID] || len(due) != 2 {
		t.Errorf("due = %v", due)
	}

	later := t0.Add(time.Hour)
	attempt(t, s, first, StatusPending, &later)
	if due := ids(t0.Add(time.Minute)); due[first.ID] || due[second.ID] {
		t.Errorf("a rescheduled delivery must keep the next one of its message waiting: %v", due)
	}
	if due := ids(later); !due[first.ID] {
		t.Errorf("not due at its time: %v", due)
	}
}

func TestRecoverInterrupted(t *testing.T) {
	s := open(t)
	m := record(t, s, "mail", "box@example.org")
	d, _ := s.SMTPDelivery(m.ID, "box@example.org", "e", "u", t0)
	queued, _ := s.CreateResend(m.ID, "box@example.org", "e", "u", t0)

	n, err := s.RecoverInterrupted(t0)
	if err != nil || n != 1 {
		t.Fatalf("recovered %d, %v", n, err)
	}
	got, _ := s.GetDelivery(d.ID)
	if got.Status != StatusDeferred || got.LastError == "" {
		t.Errorf("interrupted delivery = %+v", got)
	}
	if still, _ := s.GetDelivery(queued.ID); still.Status != StatusPending {
		t.Errorf("a queued delivery must stay queued, got %s", still.Status)
	}
}

func TestListMessages(t *testing.T) {
	s := open(t)
	for i, subject := range []string{"alpha", "beta 100%", "gamma"} {
		m, _, err := s.RecordMessage(Message{
			ReceivedAt: t0.Add(time.Duration(i) * time.Hour), MailFrom: "a@sender.test", RcptTo: []string{"box@example.org"},
			FromEmail: "billing@shop.test", Subject: subject,
		}, []byte(subject))
		if err != nil {
			t.Fatal(err)
		}
		if subject == "gamma" {
			d, _ := s.SMTPDelivery(m.ID, "box@example.org", "e", "u", t0)
			attempt(t, s, d, StatusFailed, nil)
		}
	}
	subjects := func(f MessageFilter) (out []string, more bool) {
		rows, more, err := s.ListMessages(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			out = append(out, r.Subject)
		}
		return out, more
	}
	eq := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	if got, more := subjects(MessageFilter{}); !eq(got, "gamma", "beta 100%", "alpha") || more {
		t.Errorf("all = %v more=%v", got, more)
	}
	if got, _ := subjects(MessageFilter{Status: StatusFailed}); !eq(got, "gamma") {
		t.Errorf("failed = %v", got)
	}
	if got, _ := subjects(MessageFilter{Subject: "100%"}); !eq(got, "beta 100%") {
		t.Errorf("a %% in the search must be literal: %v", got)
	}
	if got, _ := subjects(MessageFilter{From: "shop.test", To: "box@"}); len(got) != 3 {
		t.Errorf("from/to = %v", got)
	}
	if got, _ := subjects(MessageFilter{Since: t0.Add(30 * time.Minute), Until: t0.Add(90 * time.Minute)}); !eq(got, "beta 100%") {
		t.Errorf("date range = %v", got)
	}

	rows, more, _ := s.ListMessages(MessageFilter{Limit: 2})
	if len(rows) != 2 || !more {
		t.Fatalf("first page = %d rows, more=%v", len(rows), more)
	}
	if rows[0].Attempts != 1 || rows[0].LastStatusCode != 503 {
		t.Errorf("latest delivery summary = %d attempts, code %d", rows[0].Attempts, rows[0].LastStatusCode)
	}
	if got, more := subjects(MessageFilter{Limit: 2, Before: rows[1].ID}); !eq(got, "alpha") || more {
		t.Errorf("second page = %v more=%v", got, more)
	}
}

func TestPurge(t *testing.T) {
	s := open(t)
	old := record(t, s, "old mail", "box@example.org")
	d, _ := s.SMTPDelivery(old.ID, "box@example.org", "e", "u", t0)
	attempt(t, s, d, StatusDelivered, nil)
	fresh, _, _ := s.RecordMessage(Message{ReceivedAt: t0.Add(48 * time.Hour), MailFrom: "a@sender.test", RcptTo: []string{"box@example.org"}}, []byte("fresh"))

	_ = s.PutRecipient("gone@example.org", RecipientUnknown, t0)
	_ = s.PutRecipient("box@example.org", RecipientKnown, t0)
	_ = s.RecordRefusal(Refusal{RemoteAddr: "192.0.2.1", Rcpt: "gone@example.org", Reason: ReasonUnknownRecipient}, t0)

	n, err := s.Purge(t0.Add(24 * time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purged %d, %v", n, err)
	}
	if _, err := s.GetMessage(old.ID); err != ErrNotFound {
		t.Errorf("old message: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, old.RawPath)); !os.IsNotExist(err) {
		t.Errorf("old raw file still there: %v", err)
	}
	if deliveries, _ := s.DeliveriesFor(old.ID); len(deliveries) != 0 {
		t.Errorf("deliveries of a purged message remain: %d", len(deliveries))
	}
	if _, err := s.GetMessage(fresh.ID); err != nil {
		t.Errorf("fresh message: %v", err)
	}
	if r, _ := s.GetRecipient("gone@example.org"); r != nil {
		t.Error("an old unknown answer must be forgotten")
	}
	if r, _ := s.GetRecipient("box@example.org"); r == nil {
		t.Error("a known address must be kept")
	}
	if refusals, _, _ := s.ListRefusals(RefusalFilter{}); len(refusals) != 0 {
		t.Errorf("old refusals remain: %d", len(refusals))
	}
}

func TestWithoutRawStorage(t *testing.T) {
	s, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := record(t, s, "mail", "box@example.org")
	if m.HasRaw() {
		t.Error("raw kept although STORE_RAW is off")
	}
	if _, err := s.ReadRaw(m); err != ErrNotFound {
		t.Errorf("ReadRaw: %v", err)
	}
}

func TestRecipientsAndRefusals(t *testing.T) {
	s := open(t)
	if r, err := s.GetRecipient("box@example.org"); r != nil || err != nil {
		t.Fatalf("unknown address: %+v, %v", r, err)
	}

	_ = s.PutRecipient("box@example.org", RecipientUnknown, t0)
	_ = s.PutRecipient("box@example.org", RecipientKnown, t0.Add(time.Hour))
	_ = s.CountRecipient("box@example.org", true, t0.Add(2*time.Hour))
	_ = s.CountRecipient("box@example.org", false, t0)

	r, _ := s.GetRecipient("box@example.org")
	if r.Status != RecipientKnown || !r.FirstSeenAt.Equal(t0) || !r.CheckedAt.Equal(t0.Add(time.Hour)) ||
		r.Accepted != 1 || r.Refused != 1 || r.LastMailAt == nil {
		t.Errorf("recipient = %+v", r)
	}

	_ = s.PutRecipient("Box@example.org", RecipientUnknown, t0)
	if list, _ := s.ListRecipients(RecipientFilter{}); len(list) != 2 {
		t.Errorf("addresses differing in case are different addresses: %d", len(list))
	}
	if list, _ := s.ListRecipients(RecipientFilter{Status: RecipientKnown, Query: "box@"}); len(list) != 1 {
		t.Errorf("filtered = %d", len(list))
	}
	_ = s.ForgetRecipient("box@example.org")
	if r, _ := s.GetRecipient("box@example.org"); r != nil {
		t.Error("forgotten address still listed")
	}

	// Repeats within the hour are one row with a count.
	guess := Refusal{RemoteAddr: "192.0.2.1", Helo: "bot", MailFrom: "x@spam.test", Rcpt: "guess@example.org", Reason: ReasonUnknownRecipient}
	for i := 0; i < 3; i++ {
		if err := s.RecordRefusal(guess, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.RecordRefusal(guess, t0.Add(2*time.Hour))
	_ = s.RecordRefusal(Refusal{RemoteAddr: "192.0.2.9", Reason: ReasonConnectionLimit}, t0)

	all, more, _ := s.ListRefusals(RefusalFilter{})
	if len(all) != 3 || more {
		t.Fatalf("refusals = %d", len(all))
	}
	byReason, _, _ := s.ListRefusals(RefusalFilter{Reason: ReasonUnknownRecipient, Query: "guess@"})
	if len(byReason) != 2 || byReason[1].Count != 3 || !byReason[1].LastAt.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("unknown-recipient refusals = %+v", byReason)
	}
	page, more, _ := s.ListRefusals(RefusalFilter{Limit: 1})
	if len(page) != 1 || !more {
		t.Errorf("paging: %d rows, more=%v", len(page), more)
	}

	stats, _ := s.StatsSince(t0.Add(-time.Hour))
	if stats.Refused != 5 {
		t.Errorf("refused = %d, want 5", stats.Refused)
	}
}

func TestSinkReceipts(t *testing.T) {
	s := open(t)
	_ = s.AddSinkReceipt(SinkReceipt{ReceivedAt: t0, DeliveryID: "whd_1", Event: "message.received", SignatureStatus: "valid", Body: "{}"})
	_ = s.AddSinkReceipt(SinkReceipt{ReceivedAt: t0.Add(time.Minute), DeliveryID: "whd_1", Event: "message.received", SignatureStatus: "invalid", Body: "{}"})
	_ = s.AddSinkReceipt(SinkReceipt{ReceivedAt: t0, DeliveryID: "whd_2", Event: "message.received", SignatureStatus: "valid", Body: "{}"})

	got, err := s.SinkReceiptsFor([]string{"whd_1"})
	if err != nil || len(got) != 2 || got[0].SignatureStatus != "invalid" {
		t.Errorf("receipts = %+v, %v", got, err)
	}
	if none, _ := s.SinkReceiptsFor(nil); none != nil {
		t.Errorf("no deliveries: %+v", none)
	}
}
