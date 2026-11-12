package mailparse

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/gogs/chardet"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/ianaindex"
)

// decodeTransfer undoes a Content-Transfer-Encoding. It never fails: damaged
// input is decoded as far as it goes, since a slightly broken attachment is
// worth more to the recipient than none.
func decodeTransfer(data []byte, encodingName string) []byte {
	name := strings.ToLower(strings.TrimSpace(encodingName))
	if i := strings.IndexAny(name, " ;("); i >= 0 {
		name = name[:i]
	}
	switch name {
	case "base64":
		return decodeBase64(data)
	case "quoted-printable":
		return decodeQuotedPrintable(data)
	case "x-uuencode", "uuencode", "x-uue", "uue":
		if out, ok := decodeUU(data); ok {
			return out
		}
	}
	return data
}

func decodeBase64(data []byte) []byte {
	clean := make([]byte, 0, len(data))
	for _, c := range data {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			clean = append(clean, c)
		case c == '-':
			clean = append(clean, '+') // URL-safe alphabet, seen from a few senders
		case c == '_':
			clean = append(clean, '/')
		}
	}
	if len(clean)%4 == 1 {
		clean = clean[:len(clean)-1]
	}
	out := make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
	n, _ := base64.RawStdEncoding.Decode(out, clean)
	return out[:n]
}

func decodeQuotedPrintable(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		c := data[i]
		if c != '=' {
			out = append(out, c)
			continue
		}
		rest := data[i+1:]
		switch {
		case len(rest) >= 2 && isHex(rest[0]) && isHex(rest[1]):
			out = append(out, unhex(rest[0])<<4|unhex(rest[1]))
			i += 2
		case len(rest) >= 2 && rest[0] == '\r' && rest[1] == '\n':
			i += 2 // soft line break
		case len(rest) >= 1 && rest[0] == '\n':
			i++
		case len(bytes.TrimRight(lineRest(rest), " \t")) == 0 && len(rest) > 0:
			// "=" followed by trailing whitespace is still a soft break.
			i += len(lineRest(rest))
			if i+1 < len(data) && data[i+1] == '\r' {
				i++
			}
			if i+1 < len(data) && data[i+1] == '\n' {
				i++
			}
		default:
			out = append(out, c) // a literal "=" a careless encoder left behind
		}
	}
	return out
}

// lineRest returns b up to, not including, the end of its first line.
func lineRest(b []byte) []byte {
	if i := bytes.IndexAny(b, "\r\n"); i >= 0 {
		return b[:i]
	}
	return b
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' || c >= 'a' && c <= 'f'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// decodeUU reads a uuencoded block ("begin 644 name" ... "end").
func decodeUU(data []byte) ([]byte, bool) {
	var out []byte
	started := false
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !started {
			started = bytes.HasPrefix(line, []byte("begin "))
			continue
		}
		if bytes.Equal(line, []byte("end")) {
			return out, true
		}
		if len(line) == 0 {
			continue
		}
		n := int(line[0]-' ') & 0x3F
		body := line[1:]
		chunk := make([]byte, 0, n+3)
		for i := 0; i+3 < len(body)+3 && len(chunk) < n; i += 4 {
			var q [4]byte
			for j := range q {
				if i+j < len(body) {
					q[j] = (body[i+j] - ' ') & 0x3F
				}
			}
			chunk = append(chunk, q[0]<<2|q[1]>>4, q[1]<<4|q[2]>>2, q[2]<<6|q[3])
		}
		if len(chunk) > n {
			chunk = chunk[:n]
		}
		out = append(out, chunk...)
	}
	return out, started && len(out) > 0
}

// toUTF8 converts body text to UTF-8. The declared charset is tried first;
// when it is missing, unknown or contradicted by the bytes, the charset is
// detected, and Windows-1252 is the last resort because it accepts any byte.
func toUTF8(data []byte, label string) string {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	switch name := strings.ToLower(strings.TrimSpace(label)); name {
	case "", "us-ascii", "ascii", "utf-8", "utf8", "unknown-8bit":
		if utf8.Valid(data) {
			return string(data)
		}
	default:
		if enc := lookupCharset(name); enc != nil {
			if out, err := enc.NewDecoder().Bytes(data); err == nil {
				return strings.ToValidUTF8(string(out), "�")
			}
		} else if utf8.Valid(data) {
			return string(data)
		}
	}
	return detectAndDecode(data)
}

func detectAndDecode(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	if result, err := chardet.NewTextDetector().DetectBest(data); err == nil && result != nil {
		if enc := lookupCharset(result.Charset); enc != nil {
			if out, derr := enc.NewDecoder().Bytes(data); derr == nil {
				return strings.ToValidUTF8(string(out), "�")
			}
		}
	}
	out, _ := charmap.Windows1252.NewDecoder().Bytes(data)
	return strings.ToValidUTF8(string(out), "�")
}

func lookupCharset(label string) encoding.Encoding {
	if enc, _ := charset.Lookup(label); enc != nil {
		return enc
	}
	if enc, err := ianaindex.MIME.Encoding(label); err == nil && enc != nil {
		return enc
	}
	return nil
}

// headerText makes a header fragment valid UTF-8. Headers are meant to be
// ASCII, but raw 8-bit bytes are common in Subject and From.
func headerText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return detectAndDecode(b)
}

func charsetParam(contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return params["charset"]
}

var wordDecoder = &mime.WordDecoder{
	CharsetReader: func(label string, input io.Reader) (io.Reader, error) {
		data, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		return strings.NewReader(toUTF8(data, label)), nil
	},
}

// DecodeWords decodes RFC 2047 encoded words ("=?utf-8?q?Caf=C3=A9?=") and
// returns the input unchanged when they cannot be decoded.
func DecodeWords(v string) string {
	if !strings.Contains(v, "=?") {
		return v
	}
	out, err := wordDecoder.DecodeHeader(v)
	if err != nil {
		return v
	}
	return out
}
