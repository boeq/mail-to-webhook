package ui

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/boeq/mail-to-webhook/internal/store"
)

const (
	defaultTestSubject = "Test mail from mail-to-webhook"
	defaultTestBody    = "This is a test mail sent from the mail-to-webhook operator interface."

	// testPDF is the smallest file a PDF reader opens: one empty page.
	testPDF = "%PDF-1.4\n" +
		"1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n" +
		"2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n" +
		"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj\n" +
		"trailer<</Root 1 0 R>>\n" +
		"%%EOF\n"
)

type tryDoc struct {
	SendURL string
	Error   string
	To      string
	Subject string
}

func (u *UI) tryPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u.renderPage(w, r, http.StatusOK, "try", tryDoc{
		SendURL: prefix + "/api/try",
		Error:   q.Get("error"),
		To:      q.Get("to"),
		Subject: firstNonEmpty(q.Get("subject"), defaultTestSubject),
	})
}

// trySend sends one mail through the service's own SMTP listener, so a test
// takes exactly the path a real mail takes: recipient check, parsing, payload,
// webhook.
func (u *UI) trySend(w http.ResponseWriter, r *http.Request) {
	to := oneLine(r.FormValue("to"))
	subject := firstNonEmpty(oneLine(r.FormValue("subject")), defaultTestSubject)
	body := firstNonEmpty(strings.TrimSpace(r.FormValue("body")), defaultTestBody)
	attach := r.FormValue("attach") != ""

	if to == "" || !strings.Contains(to, "@") || strings.ContainsAny(to, " <>") {
		u.tryFailed(w, r, to, subject, "give the address to send the test mail to", "")
		return
	}

	messageID := "try." + strings.ToLower(strings.TrimPrefix(store.NewDeliveryID(), "whd_")) + "@" + u.Cfg.SMTPDomain
	sendErr := u.sendTestMail(r.Context(), to, subject, body, attach, messageID)

	location := ""
	if id := u.findByMessageID(messageID); id != "" {
		location = prefix + "/messages/" + id
	}
	if sendErr != nil {
		// The reply is the server's own, e.g. "550 5.1.1 No such mailbox here".
		u.tryFailed(w, r, to, subject, "the mail was not accepted: "+smtpReply(sendErr), location)
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, firstNonEmpty(location, prefix+"/messages"), http.StatusSeeOther)
		return
	}
	if location != "" {
		w.Header().Set("HX-Redirect", location)
	}
	u.writeJSON(w, http.StatusOK, map[string]any{"message": "Sent.", "message_url": location})
}

// smtpReply is the server's reply as it was sent, code first.
func smtpReply(err error) string {
	var reply *textproto.Error
	if errors.As(err, &reply) {
		return strconv.Itoa(reply.Code) + " " + reply.Msg
	}
	return err.Error()
}

// tryFailed answers a failed test: the error envelope for the script, a
// redirect back to the form for a plain form post.
func (u *UI) tryFailed(w http.ResponseWriter, r *http.Request, to, subject, message, location string) {
	if r.Header.Get("HX-Request") == "true" {
		details := map[string]any{}
		if location != "" {
			details["message_url"] = location
		}
		u.failWith(w, http.StatusUnprocessableEntity, "validation_error", message, details)
		return
	}
	q := url.Values{"error": {message}, "to": {to}, "subject": {subject}}
	http.Redirect(w, r, prefix+"/try?"+q.Encode(), http.StatusSeeOther)
}

// findByMessageID looks for the test mail among the newest messages.
func (u *UI) findByMessageID(messageID string) string {
	rows, _, err := u.Store.ListMessages(store.MessageFilter{Limit: 50})
	if err != nil {
		return ""
	}
	for _, m := range rows {
		if m.MessageID == messageID {
			return m.ID
		}
	}
	return ""
}

// smtpTarget is where the listener can be reached from this process.
func (u *UI) smtpTarget() string {
	host, port, err := net.SplitHostPort(u.SMTPAddr)
	if err != nil {
		return u.SMTPAddr
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func (u *UI) sendTestMail(ctx context.Context, to, subject, body string, attach bool, messageID string) error {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", u.smtpTarget())
	if err != nil {
		return err
	}
	defer conn.Close()
	// The DATA reply only comes once the webhook has answered, retries included.
	wait := u.Cfg.WebhookTimeout*time.Duration(u.Cfg.WebhookRetries+1) + 45*time.Second
	// The wall clock, not u.Now: a socket deadline is not history.
	_ = conn.SetDeadline(time.Now().Add(wait))

	client, err := smtp.NewClient(conn, "127.0.0.1")
	if err != nil {
		return err
	}
	defer client.Close()

	from := u.Cfg.FallbackSender
	if err := client.Hello("localhost"); err != nil {
		return err
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	data, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := data.Write(testMessage(from, to, subject, body, attach, messageID, u.Now())); err != nil {
		return err
	}
	if err := data.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func testMessage(from, to, subject, body string, attach bool, messageID string, now time.Time) []byte {
	var msg bytes.Buffer
	header := func(name, value string) { msg.WriteString(name + ": " + value + "\r\n") }
	header("From", (&mailAddress{Name: "mail-to-webhook test", Address: from}).String())
	header("To", "<"+to+">")
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", now.UTC().Format(time.RFC1123Z))
	header("Message-ID", "<"+messageID+">")
	header("MIME-Version", "1.0")

	if !attach {
		header("Content-Type", "text/plain; charset=utf-8")
		header("Content-Transfer-Encoding", "quoted-printable")
		msg.WriteString("\r\n")
		writeQuotedPrintable(&msg, body)
		return msg.Bytes()
	}

	parts := multipart.NewWriter(&msg)
	header("Content-Type", `multipart/mixed; boundary="`+parts.Boundary()+`"`)
	msg.WriteString("\r\n")

	text, _ := parts.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	writeQuotedPrintable(text, body)

	file, _ := parts.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {`application/pdf; name="test.pdf"`},
		"Content-Disposition":       {`attachment; filename="test.pdf"`},
		"Content-Transfer-Encoding": {"base64"},
	})
	encoded := base64.StdEncoding.EncodeToString([]byte(testPDF))
	for len(encoded) > 76 {
		_, _ = file.Write([]byte(encoded[:76] + "\r\n"))
		encoded = encoded[76:]
	}
	_, _ = file.Write([]byte(encoded + "\r\n"))
	_ = parts.Close()
	return msg.Bytes()
}

type writer interface{ Write([]byte) (int, error) }

func writeQuotedPrintable(w writer, text string) {
	qp := quotedprintable.NewWriter(w)
	_, _ = qp.Write([]byte(text + "\n"))
	_ = qp.Close()
}

// mailAddress formats a display name with an address, the name quoted.
type mailAddress struct {
	Name    string
	Address string
}

func (a *mailAddress) String() string {
	return `"` + strings.NewReplacer(`\`, ``, `"`, ``).Replace(a.Name) + `" <` + a.Address + `>`
}

// oneLine makes form input safe to put in a header: no line breaks, no
// control characters, so a value cannot start a header of its own.
func oneLine(v string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F {
			return -1
		}
		return r
	}, v))
}
