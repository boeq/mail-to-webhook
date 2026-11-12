// Package payload converts a parsed message into the JSON body posted to the
// webhook. The field names mirror Postmark's inbound payload so existing
// handlers keep working; anything additional lives under "Envelope".
//
// Every field a strict consumer constrains is normalised here, so that no
// mail is refused for the way it was written.
package payload

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/boeq/mail-to-webhook/internal/consumer"
	"github.com/boeq/mail-to-webhook/internal/mailparse"
)

type Address struct {
	Email       string `json:"Email"`
	Name        string `json:"Name"`
	MailboxHash string `json:"MailboxHash"`
}

type Header struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

type Attachment struct {
	Name          string `json:"Name"`
	Content       string `json:"Content"` // base64, empty if truncated
	ContentType   string `json:"ContentType"`
	ContentLength int    `json:"ContentLength"`
	ContentID     string `json:"ContentID,omitempty"`
}

// Envelope carries SMTP-level facts the message body cannot be trusted for.
type Envelope struct {
	MailFrom   string    `json:"MailFrom"`
	RcptTo     []string  `json:"RcptTo"`
	RemoteAddr string    `json:"RemoteAddr"`
	Helo       string    `json:"Helo"`
	TLS        bool      `json:"TLS"`
	ReceivedAt time.Time `json:"ReceivedAt"`
	Size       int       `json:"Size"`
}

type Inbound struct {
	FromName          string       `json:"FromName"`
	From              string       `json:"From"`
	FromFull          Address      `json:"FromFull"`
	To                string       `json:"To"`
	ToFull            []Address    `json:"ToFull"`
	Cc                string       `json:"Cc"`
	CcFull            []Address    `json:"CcFull"`
	Bcc               string       `json:"Bcc"`
	BccFull           []Address    `json:"BccFull"`
	OriginalRecipient string       `json:"OriginalRecipient"`
	Subject           string       `json:"Subject"`
	MessageID         string       `json:"MessageID"`
	ReplyTo           string       `json:"ReplyTo"`
	MailboxHash       string       `json:"MailboxHash"`
	Date              string       `json:"Date"`
	TextBody          string       `json:"TextBody"`
	HtmlBody          string       `json:"HtmlBody"`
	StrippedTextReply string       `json:"StrippedTextReply"`
	Tag               string       `json:"Tag"`
	Headers           []Header     `json:"Headers"`
	Attachments       []Attachment `json:"Attachments"`

	// Extensions beyond the Postmark shape.
	Envelope Envelope `json:"Envelope"`
	RawEmail string   `json:"RawEmail,omitempty"` // base64 of the full RFC822 source

	// Notes records normalisation decisions for logging. Not serialised.
	Notes []string `json:"-"`
}

type Options struct {
	IncludeRaw         bool
	IncludeAttachments bool
	MaxAttachBytes     int64

	// FallbackSender is used for FromFull.Email when the message has no
	// parseable From header and the envelope sender is empty or invalid
	// (null-sender bounces, "From: Mailer Daemon", spoofed junk). The
	// consuming validator requires a valid address here, so there must
	// always be one.
	FallbackSender string

	// OversizePolicy decides what happens to an attachment larger than
	// MaxAttachBytes: OversizeSkip drops it from the payload, OversizeReject
	// fails the whole message so the sender is told.
	OversizePolicy string

	// DedupeAttachmentNames appends -2, -3 ... to repeated filenames so a
	// consumer that stores by name cannot silently overwrite.
	DedupeAttachmentNames bool

	// EnsureRecipientInTo adds the envelope recipient to ToFull when the
	// headers do not name it: a blind copy, an auto-forward or an alias. A
	// consumer that routes on To and Cc would otherwise not recognise mail
	// that was delivered to one of its own addresses.
	EnsureRecipientInTo bool

	// AllRecipients are the other envelope recipients of the transaction.
	// With one POST per message they get the same treatment as the recipient.
	AllRecipients []string

	// SynthesizeMessageID gives a message without a Message-ID a stable one
	// derived from its content, so a consumer that deduplicates on the header
	// does not store a repeated attempt twice.
	SynthesizeMessageID bool

	// Domain is the right-hand side of a synthesized Message-ID.
	Domain string
}

const (
	OversizeSkip   = "skip"
	OversizeReject = "reject"
)

// ErrAttachmentTooLarge is returned when OversizePolicy is "reject".
var ErrAttachmentTooLarge = errors.New("attachment exceeds size limit")

// Build renders the payload for one recipient.
//
// Every field the consuming validator constrains is normalised here so the
// result always satisfies the rules: FromFull.Email and at least one
// ToFull[].Email are guaranteed valid addresses, header entries always carry a
// non-empty Name, and attachments always carry a non-empty Name, Content and
// ContentType.
func Build(m *mailparse.Message, env Envelope, recipient string, opt Options) (*Inbound, error) {
	p := &Inbound{
		Subject:           clean(m.GetDecoded("Subject")),
		MessageID:         clean(trimAngles(m.Get("Message-ID"))),
		ReplyTo:           clean(m.GetDecoded("Reply-To")),
		TextBody:          cleanBody(m.Text),
		HtmlBody:          cleanBody(m.HTML),
		StrippedTextReply: cleanBody(StripReply(m.Text)),
		OriginalRecipient: recipient,
		MailboxHash:       mailboxHash(recipient),
		Envelope:          env,
		Headers:           make([]Header, 0, len(m.Headers)),
		Attachments:       []Attachment{},
		ToFull:            []Address{},
		CcFull:            []Address{},
		BccFull:           []Address{},
	}

	// Headers.*.Name is required: a line like ": orphan" yields an empty name,
	// so drop it rather than fail the whole message downstream.
	for _, h := range m.Headers {
		name := clean(h.Name)
		if name == "" {
			p.note("dropped header with empty name")
			continue
		}
		p.Headers = append(p.Headers, Header{Name: name, Value: clean(h.Value)})
	}

	// A consumer hands Date to a date parser, and one it cannot read is a
	// server error there: the sender would retry for days and then bounce.
	// The header itself stays as it was in Headers.
	date, parsed := normaliseDate(m.Get("Date"), env.ReceivedAt)
	p.Date = date
	switch {
	case parsed:
	case strings.TrimSpace(m.Get("Date")) == "":
		p.note("no Date header, used the receive time")
	default:
		p.note("unreadable Date header, used the receive time")
	}

	if opt.SynthesizeMessageID && p.MessageID == "" {
		p.MessageID = syntheticMessageID(m.Raw, opt.Domain)
		// An empty Message-ID header would be read before the one added here.
		p.Headers = withoutHeader(p.Headers, "Message-ID")
		p.Headers = append(p.Headers, Header{Name: "Message-ID", Value: "<" + p.MessageID + ">"})
		p.note("no Message-ID header, added " + p.MessageID)
	}

	// FromFull.Email is required and must be a valid address. Try the From
	// header, then the envelope sender, then the configured fallback.
	p.FromFull = firstValidAddress(
		parseList(m.Get("From")),
		[]Address{{Email: env.MailFrom}},
		[]Address{{Email: opt.FallbackSender}},
	)
	if fromHdr := parseList(m.Get("From")); len(fromHdr) > 0 && !consumer.IsEmail(fromHdr[0].Email) {
		// Keep the display name even when the address itself was unusable.
		if p.FromFull.Name == "" {
			p.FromFull.Name = clean(orAddr(fromHdr[0].Name, fromHdr[0].Email))
		}
		p.note("From header unusable, substituted " + p.FromFull.Email)
	}
	p.From = p.FromFull.Email
	p.FromName = p.FromFull.Name

	p.ToFull = validAddresses(parseList(m.Get("To")))
	p.CcFull = validAddresses(parseList(m.Get("Cc")))
	p.BccFull = validAddresses(parseList(m.Get("Bcc")))

	if opt.EnsureRecipientInTo {
		for _, rcpt := range append([]string{recipient}, opt.AllRecipients...) {
			if !consumer.IsEmail(rcpt) || hasAddress(p.ToFull, rcpt) || hasAddress(p.CcFull, rcpt) {
				continue
			}
			p.ToFull = append(p.ToFull, Address{Email: rcpt, MailboxHash: mailboxHash(rcpt)})
			p.note("envelope recipient " + rcpt + " was not in To or Cc, added to ToFull")
		}
	}

	// ToFull requires min:1. Bcc-only delivery and "To: undisclosed-recipients:;"
	// both legitimately produce no To header, so fall back to the envelope
	// recipient, which is the address this delivery is actually for.
	if len(p.ToFull) == 0 {
		if consumer.IsEmail(recipient) {
			p.ToFull = []Address{{Email: recipient, MailboxHash: mailboxHash(recipient)}}
			p.note("no usable To header, used envelope recipient")
		} else {
			return nil, fmt.Errorf("no valid recipient address available (envelope %q)", recipient)
		}
	}

	p.To = joinAddrs(p.ToFull)
	p.Cc = joinAddrs(p.CcFull)
	p.Bcc = joinAddrs(p.BccFull)

	if opt.IncludeAttachments {
		atts, err := buildAttachments(m.Attachments, opt, p)
		if err != nil {
			return nil, err
		}
		p.Attachments = atts
	}

	if opt.IncludeRaw {
		p.RawEmail = base64.StdEncoding.EncodeToString(m.Raw)
	}
	return p, nil
}

// buildAttachments enforces the three "required string" rules on every entry.
func buildAttachments(src []mailparse.Attachment, opt Options, p *Inbound) ([]Attachment, error) {
	out := make([]Attachment, 0, len(src))
	seen := map[string]int{}

	for _, a := range src {
		// Content is a required string, and base64 of zero bytes is "".
		// An empty part is not a usable attachment, so drop it.
		if len(a.Data) == 0 {
			p.note("dropped zero-byte attachment " + quote(a.Name))
			continue
		}
		if opt.MaxAttachBytes > 0 && int64(len(a.Data)) > opt.MaxAttachBytes {
			if opt.OversizePolicy == OversizeReject {
				return nil, fmt.Errorf("%w: %s is %d bytes", ErrAttachmentTooLarge, a.Name, len(a.Data))
			}
			p.note(fmt.Sprintf("dropped oversize attachment %s (%d bytes)", quote(a.Name), len(a.Data)))
			continue
		}

		name := attachmentName(a)
		if opt.DedupeAttachmentNames {
			name = dedupe(name, seen)
		}

		ct := clean(a.ContentType)
		if ct == "" {
			ct = "application/octet-stream"
		}

		out = append(out, Attachment{
			Name:          name,
			Content:       base64.StdEncoding.EncodeToString(a.Data),
			ContentType:   ct,
			ContentLength: len(a.Data),
			ContentID:     clean(a.ContentID),
		})
	}
	return out, nil
}

// attachmentName guarantees a non-empty, filesystem-safe name.
func attachmentName(a mailparse.Attachment) string {
	name := clean(a.Name)
	name = strings.TrimSpace(path.Base(strings.ReplaceAll(name, "\\", "/")))
	if name == "" || name == "." || name == ".." || name == "/" {
		name = "attachment"
		if exts := extensionFor(a.ContentType); exts != "" {
			name += exts
		}
	}
	if len(name) > 255 {
		name = name[:255]
	}
	return name
}

func dedupe(name string, seen map[string]int) string {
	key := strings.ToLower(name)
	seen[key]++
	if n := seen[key]; n > 1 {
		ext := path.Ext(name)
		return fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), n, ext)
	}
	return name
}

func (p *Inbound) note(s string) { p.Notes = append(p.Notes, s) }

// normaliseDate returns the Date header in RFC 2822 form, or the receive time
// when the header is missing or unreadable. The second result says which.
func normaliseDate(header string, received time.Time) (string, bool) {
	if t, err := mail.ParseDate(strings.TrimSpace(header)); err == nil {
		return t.Format(time.RFC1123Z), true
	}
	if received.IsZero() {
		received = time.Now()
	}
	return received.UTC().Format(time.RFC1123Z), false
}

// syntheticMessageID is the same for every delivery attempt of one message.
func syntheticMessageID(raw []byte, domain string) string {
	if domain == "" {
		domain = "mail-to-webhook.invalid"
	}
	sum := sha256.Sum256(raw)
	return "m2w." + hex.EncodeToString(sum[:16]) + "@" + domain
}

func trimAngles(v string) string {
	return strings.Trim(strings.TrimSpace(v), "<>")
}

func withoutHeader(headers []Header, name string) []Header {
	out := headers[:0]
	for _, h := range headers {
		if !strings.EqualFold(h.Name, name) {
			out = append(out, h)
		}
	}
	return out
}

func hasAddress(list []Address, email string) bool {
	for _, a := range list {
		if a.Email == email {
			return true
		}
	}
	return false
}

func quote(s string) string {
	if s == "" {
		return "(unnamed)"
	}
	return "\"" + s + "\""
}

// validAddresses keeps only entries the consumer's `email` rule will accept.
func validAddresses(list []Address) []Address {
	out := make([]Address, 0, len(list))
	for _, a := range list {
		if consumer.IsEmail(a.Email) {
			a.Name = clean(a.Name)
			out = append(out, a)
		}
	}
	return out
}

func firstValidAddress(candidates ...[]Address) Address {
	for _, list := range candidates {
		for _, a := range list {
			if consumer.IsEmail(a.Email) {
				a.Name = clean(a.Name)
				return a
			}
		}
	}
	return Address{}
}

func orAddr(name, fallback string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return fallback
}

// clean strips control characters and guarantees valid UTF-8 for single-line
// fields, so the JSON is safe for any consumer to store and display.
func clean(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F {
			return -1
		}
		return r
	}, s))
}

// cleanBody is the same but keeps the line structure of message bodies.
func cleanBody(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7F {
			return -1
		}
		return r
	}, s)
}

func extensionFor(mediaType string) string {
	switch mediaType {
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	case "text/html":
		return ".html"
	case "text/csv":
		return ".csv"
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "message/rfc822":
		return ".eml"
	case "application/zip":
		return ".zip"
	case "application/xml", "text/xml":
		return ".xml"
	case "text/calendar":
		return ".ics"
	case "image/webp":
		return ".webp"
	case "image/heic":
		return ".heic"
	case "image/tiff":
		return ".tif"
	case "message/delivery-status", "message/disposition-notification", "text/rfc822-headers":
		return ".txt"
	case "application/pkcs7-signature", "application/x-pkcs7-signature":
		return ".p7s"
	case "application/pkcs7-mime", "application/x-pkcs7-mime":
		return ".p7m"
	case "application/pgp-signature":
		return ".asc"
	}
	return ".bin"
}

// mailboxHash returns the part after "+" in the local part, which is how
// Postmark lets you route "inbound+order123@..." to one handler.
func mailboxHash(addr string) string {
	local, _, ok := strings.Cut(addr, "@")
	if !ok {
		return ""
	}
	if _, hash, ok := strings.Cut(local, "+"); ok {
		return hash
	}
	return ""
}

func parseList(v string) []Address {
	out := []Address{}
	if strings.TrimSpace(v) == "" {
		return out
	}
	list, err := mail.ParseAddressList(mailparse.DecodeWords(v))
	if err != nil {
		// Fall back to a naive split so a malformed header still yields data.
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, Address{Email: part, MailboxHash: mailboxHash(part)})
			}
		}
		return out
	}
	for _, a := range list {
		out = append(out, Address{
			Email:       a.Address,
			Name:        a.Name,
			MailboxHash: mailboxHash(a.Address),
		})
	}
	return out
}

func joinAddrs(list []Address) string {
	parts := make([]string, 0, len(list))
	for _, a := range list {
		if a.Name != "" {
			parts = append(parts, a.Name+" <"+a.Email+">")
			continue
		}
		parts = append(parts, a.Email)
	}
	return strings.Join(parts, ", ")
}

// replyMarkers are the usual "everything below this is quoted history" lines.
var replyMarkers = []*regexp.Regexp{
	regexp.MustCompile(`(?im)^\s*On .{5,80}\s+wrote:\s*$`),
	regexp.MustCompile(`(?im)^\s*-{2,}\s*Original Message\s*-{2,}\s*$`),
	regexp.MustCompile(`(?im)^\s*_{5,}\s*$`),
	regexp.MustCompile(`(?im)^\s*From:\s.+$`),
	regexp.MustCompile(`(?im)^\s*Op .{5,80}\s+schreef .{0,80}:\s*$`),
	regexp.MustCompile(`(?im)^\s*Am .{5,80}\s+schrieb .{0,80}:\s*$`),
	regexp.MustCompile(`(?im)^\s*-{2,}\s*$`), // signature delimiter
}

// StripReply is a best-effort trim of quoted history and signatures, matching
// what Postmark exposes as StrippedTextReply.
func StripReply(text string) string {
	if text == "" {
		return ""
	}
	cut := len(text)
	for _, re := range replyMarkers {
		if loc := re.FindStringIndex(text); loc != nil && loc[0] < cut {
			cut = loc[0]
		}
	}
	body := text[:cut]

	// Drop trailing quoted lines that were not preceded by a marker.
	lines := strings.Split(body, "\n")
	for len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "" || strings.HasPrefix(last, ">") {
			lines = lines[:len(lines)-1]
			continue
		}
		break
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
