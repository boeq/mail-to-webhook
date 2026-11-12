// Package mailparse turns a raw RFC 5322 message into bodies, attachments and
// an ordered header list.
//
// The MIME tree comes from enmime, which copes with the malformed structure
// real mail has. Part contents are decoded here rather than by enmime, because
// enmime converts every text/* part to UTF-8: right for a body, wrong for an
// attachment, where an XML invoice declaring ISO-8859-1 must keep its bytes.
package mailparse

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"

	"github.com/inbucket/html2text"
	"github.com/jhillyerd/enmime/v2"
	"github.com/teamwork/tnef"
)

// maxDepth bounds how deep attached messages are opened.
const maxDepth = 4

type Header struct {
	Name  string
	Value string
}

type Attachment struct {
	Name        string
	ContentType string
	ContentID   string
	Data        []byte
}

type Message struct {
	// Headers keeps the original order, duplicates and encoded values.
	Headers     []Header
	Text        string
	HTML        string
	Attachments []Attachment
	Raw         []byte
}

type Options struct {
	// TextFromHTML derives Text from HTML when the message has no text part.
	TextFromHTML bool
	// ExpandAttachedMessages also surfaces the attachments of a message that
	// was forwarded as an attachment (message/rfc822).
	ExpandAttachedMessages bool
	// ExpandTNEF replaces an Outlook winmail.dat by the files inside it.
	ExpandTNEF bool
}

func DefaultOptions() Options {
	return Options{TextFromHTML: true, ExpandAttachedMessages: true, ExpandTNEF: true}
}

// Parse reads a message with the default options.
func Parse(r io.Reader) (*Message, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return ParseBytes(raw, DefaultOptions())
}

// ParseBytes never returns a nil message. When the MIME structure cannot be
// read it returns what the headers gave plus the whole message as an .eml
// attachment, together with the error, so nothing is lost to a parse failure.
func ParseBytes(raw []byte, opt Options) (*Message, error) {
	return parse(raw, opt, 0)
}

func parse(raw []byte, opt Options, depth int) (msg *Message, err error) {
	headers, bodyOffset := parseHeaders(raw)
	msg = &Message{Headers: headers, Raw: raw}

	defer func() {
		if r := recover(); r != nil {
			msg = fallback(raw, headers, bodyOffset)
			err = fmt.Errorf("mime parser panicked: %v", r)
		}
	}()

	parser := enmime.NewParser(
		enmime.RawContent(true),
		enmime.SkipMalformedParts(true),
		enmime.MultipartWOBoundaryAsSinglePart(true),
		enmime.DisableTextConversion(true),
	)
	root, perr := parser.ReadParts(bytes.NewReader(raw))
	if perr != nil || root == nil {
		if perr == nil {
			perr = errors.New("no parts")
		}
		return fallback(raw, headers, bodyOffset), fmt.Errorf("mime structure unreadable: %w", perr)
	}

	w := &walker{msg: msg, opt: opt, depth: depth}
	w.walk(root)

	msg.Text = strings.Join(w.text, "\n")
	msg.HTML = strings.Join(w.html, "\n")
	if msg.Text == "" && msg.HTML != "" && opt.TextFromHTML {
		if text, herr := html2text.FromString(msg.HTML); herr == nil {
			msg.Text = text
		}
	}
	return msg, nil
}

// fallback is the answer for a message whose structure cannot be read.
func fallback(raw []byte, headers []Header, bodyOffset int) *Message {
	msg := &Message{Headers: headers, Raw: raw}
	ctype := strings.ToLower(msg.Get("Content-Type"))
	if ctype == "" || strings.HasPrefix(ctype, "text/plain") {
		body := decodeTransfer(raw[bodyOffset:], msg.Get("Content-Transfer-Encoding"))
		msg.Text = toUTF8(body, charsetParam(msg.Get("Content-Type")))
	}
	msg.Attachments = []Attachment{{Name: "message.eml", ContentType: "message/rfc822", Data: raw}}
	return msg
}

type walker struct {
	msg   *Message
	opt   Options
	depth int
	text  []string
	html  []string
}

func (w *walker) walk(p *enmime.Part) {
	if p == nil {
		return
	}
	if p.FirstChild != nil {
		for c := p.FirstChild; c != nil; c = c.NextSibling {
			w.walk(c)
		}
		return
	}
	w.leaf(p)
}

func (w *walker) leaf(p *enmime.Part) {
	ctype := strings.ToLower(strings.TrimSpace(p.ContentType))
	if ctype == "" || strings.HasPrefix(ctype, "multipart/") {
		// No type, or a multipart whose boundary never appeared: readable text at best.
		ctype = "text/plain"
	}
	data := decodeTransfer(p.Content, p.Header.Get("Content-Transfer-Encoding"))
	name := strings.TrimSpace(p.FileName)
	disposition := strings.ToLower(p.Disposition)

	switch {
	case disposition != "attachment" && name == "" && ctype == "text/plain":
		w.text = append(w.text, toUTF8(data, p.Charset))
		return
	case disposition != "attachment" && name == "" && ctype == "text/html":
		w.html = append(w.html, toUTF8(data, p.Charset))
		return
	case ctype == "text/x-amp-html" || ctype == "text/watch-html":
		// Alternative renderings of the same body, of no use to a consumer.
		return
	}

	if w.opt.ExpandTNEF && isTNEF(ctype, name) {
		if inner := decodeTNEF(data); len(inner) > 0 {
			w.msg.Attachments = append(w.msg.Attachments, inner...)
			return
		}
	}

	if ctype == "message/rfc822" || ctype == "message/global" {
		w.attachedMessage(name, data)
		return
	}

	if name == "" && ctype == "text/calendar" {
		name = "invite.ics"
	}
	w.msg.Attachments = append(w.msg.Attachments, Attachment{
		Name:        name,
		ContentType: ctype,
		ContentID:   strings.Trim(p.ContentID, "<> \t"),
		Data:        data,
	})
}

// attachedMessage forwards the attached message as an .eml and, when asked,
// surfaces the attachments inside it: a mail client's "forward as attachment"
// otherwise hides the document the sender meant to pass on.
func (w *walker) attachedMessage(name string, data []byte) {
	var inner *Message
	if len(data) > 0 && w.depth < maxDepth {
		inner, _ = parse(data, w.opt, w.depth+1)
	}
	if name == "" {
		name = "message.eml"
		if inner != nil {
			if subject := fileNameFrom(inner.GetDecoded("Subject")); subject != "" {
				name = subject + ".eml"
			}
		}
	}
	w.msg.Attachments = append(w.msg.Attachments, Attachment{
		Name:        name,
		ContentType: "message/rfc822",
		Data:        data,
	})
	if inner != nil && w.opt.ExpandAttachedMessages {
		w.msg.Attachments = append(w.msg.Attachments, inner.Attachments...)
	}
}

func isTNEF(ctype, name string) bool {
	return ctype == "application/ms-tnef" || ctype == "application/vnd.ms-tnef" ||
		strings.EqualFold(name, "winmail.dat")
}

// decodeTNEF returns the files inside a winmail.dat, or nil when it holds
// none or cannot be read; the caller then forwards the wrapper itself.
func decodeTNEF(data []byte) (out []Attachment) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	decoded, err := tnef.Decode(data)
	if err != nil || decoded == nil {
		return nil
	}
	for _, a := range decoded.Attachments {
		if a == nil || len(a.Data) == 0 {
			continue
		}
		name := strings.TrimRight(a.Title, "\x00")
		ctype := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
		if ctype, _, _ = strings.Cut(ctype, ";"); ctype == "" {
			ctype = "application/octet-stream"
		}
		out = append(out, Attachment{Name: name, ContentType: ctype, Data: a.Data})
	}
	return out
}

// fileNameFrom makes a subject usable as a file name.
func fileNameFrom(subject string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7F:
			return -1
		case strings.ContainsRune(`/\:*?"<>|`, r):
			return ' '
		}
		return r
	}, subject)
	name = strings.Join(strings.Fields(name), " ")
	if runes := []rune(name); len(runes) > 80 {
		name = strings.TrimSpace(string(runes[:80]))
	}
	return name
}

// Get returns the raw value of the first header with that name.
func (m *Message) Get(name string) string {
	for _, h := range m.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// GetDecoded is Get with RFC 2047 encoded words decoded.
func (m *Message) GetDecoded(name string) string {
	return DecodeWords(m.Get(name))
}

// parseHeaders reads the header block by hand: the order, the duplicates and
// the encoded values must survive, and a map-based reader keeps none of them.
// It returns the offset of the body.
func parseHeaders(raw []byte) ([]Header, int) {
	var headers []Header
	offset := 0
	for offset < len(raw) {
		end := bytes.IndexByte(raw[offset:], '\n')
		var line []byte
		next := len(raw)
		if end >= 0 {
			line = raw[offset : offset+end]
			next = offset + end + 1
		} else {
			line = raw[offset:]
		}
		line = bytes.TrimRight(line, "\r")
		offset = next

		if len(line) == 0 {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if n := len(headers); n > 0 {
				headers[n-1].Value = strings.TrimSpace(headers[n-1].Value + " " + headerText(bytes.TrimSpace(line)))
			}
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			// An mbox "From " line or stray text; not a header, and not the body yet.
			continue
		}
		headers = append(headers, Header{
			Name:  strings.TrimSpace(headerText(line[:colon])),
			Value: strings.TrimSpace(headerText(line[colon+1:])),
		})
	}
	return headers, offset
}
