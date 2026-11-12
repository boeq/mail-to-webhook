package mailparse

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func mustParse(t *testing.T, raw string) *Message {
	t.Helper()
	m, err := ParseBytes([]byte(raw), DefaultOptions())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func encode(t *testing.T, enc encoding.Encoding, s string) string {
	t.Helper()
	out, err := enc.NewEncoder().String(s)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

func names(m *Message) []string {
	out := make([]string, 0, len(m.Attachments))
	for _, a := range m.Attachments {
		out = append(out, a.Name)
	}
	return out
}

func wantNames(t *testing.T, m *Message, want ...string) {
	t.Helper()
	got := names(m)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("attachments = %q, want %q", got, want)
	}
}

func TestTextBodiesInEveryEncoding(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain 7bit, no MIME headers", "Subject: x\r\n\r\nhello\r\n", "hello"},
		{"quoted-printable latin-1",
			"Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nCaf=E9 =\r\nau lait\r\n",
			"Café au lait"},
		{"base64 utf-8",
			"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
				base64.StdEncoding.EncodeToString([]byte("Grüße ☕")) + "\r\n",
			"Grüße ☕"},
		{"8bit windows-1252",
			"Content-Type: text/plain; charset=windows-1252\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" +
				encode(t, charmap.Windows1252, "€ 12,50 – betaald") + "\r\n",
			"€ 12,50 – betaald"},
		{"8bit koi8-r",
			"Content-Type: text/plain; charset=koi8-r\r\n\r\n" + encode(t, charmap.KOI8R, "Привет, мир") + "\r\n",
			"Привет, мир"},
		{"shift_jis",
			"Content-Type: text/plain; charset=Shift_JIS\r\n\r\n" + encode(t, japanese.ShiftJIS, "請求書を送ります") + "\r\n",
			"請求書を送ります"},
		{"iso-2022-jp",
			"Content-Type: text/plain; charset=ISO-2022-JP\r\n\r\n" + encode(t, japanese.ISO2022JP, "請求書") + "\r\n",
			"請求書"},
		{"gbk declared as gb2312",
			"Content-Type: text/plain; charset=gb2312\r\n\r\n" + encode(t, simplifiedchinese.GBK, "发票已附上") + "\r\n",
			"发票已附上"},
		{"latin-1 bytes with no charset at all",
			"Content-Type: text/plain\r\n\r\n" + encode(t, charmap.ISO8859_1, "Très bien, déjà payé à l'hôtel") + "\r\n",
			"Très bien, déjà payé à l'hôtel"},
		{"latin-1 bytes mislabelled as utf-8",
			"Content-Type: text/plain; charset=utf-8\r\n\r\n" + encode(t, charmap.ISO8859_1, "Très bien, déjà payé à l'hôtel") + "\r\n",
			"Très bien, déjà payé à l'hôtel"},
		{"an unknown charset name",
			"Content-Type: text/plain; charset=x-made-up\r\n\r\nplain ascii\r\n",
			"plain ascii"},
		{"utf-8 with a byte order mark",
			"Content-Type: text/plain; charset=utf-8\r\n\r\n\xEF\xBB\xBFbom\r\n",
			"bom"},
		{"bare line feeds",
			"Subject: x\nContent-Type: text/plain\n\nline one\nline two\n",
			"line one\nline two"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mustParse(t, tc.raw)
			if got := strings.TrimSpace(m.Text); got != tc.want {
				t.Errorf("Text = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHTMLOnlyMessageGetsATextBody(t *testing.T) {
	raw := "Content-Type: text/html; charset=utf-8\r\n\r\n<html><body><p>Uw <b>factuur</b> staat klaar.</p></body></html>\r\n"

	m := mustParse(t, raw)
	if !strings.Contains(m.HTML, "<b>factuur</b>") {
		t.Errorf("HTML = %q", m.HTML)
	}
	if !strings.Contains(m.Text, "factuur") || strings.Contains(m.Text, "<p>") {
		t.Errorf("Text = %q, want the readable words without markup", m.Text)
	}

	plain, _ := ParseBytes([]byte(raw), Options{})
	if plain.Text != "" {
		t.Errorf("Text = %q, want none when TextFromHTML is off", plain.Text)
	}
}

const nested = "From: a@sender.test\r\n" +
	"Content-Type: multipart/mixed; boundary=M\r\n\r\n" +
	"--M\r\nContent-Type: multipart/alternative; boundary=A\r\n\r\n" +
	"--A\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nplain body\r\n" +
	"--A\r\nContent-Type: multipart/related; boundary=R\r\n\r\n" +
	"--R\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>html body <img src=\"cid:logo@x\"></p>\r\n" +
	"--R\r\nContent-Type: image/png\r\nContent-ID: <logo@x>\r\nContent-Transfer-Encoding: base64\r\n\r\naW1n\r\n" +
	"--R--\r\n" +
	"--A--\r\n" +
	"--M\r\nContent-Type: application/pdf; name=\"invoice.pdf\"\r\nContent-Disposition: attachment; filename=\"invoice.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQ=\r\n" +
	"--M--\r\n"

func TestNestedMultipart(t *testing.T) {
	m := mustParse(t, nested)
	if m.Text != "plain body" {
		t.Errorf("Text = %q", m.Text)
	}
	if !strings.Contains(m.HTML, "html body") {
		t.Errorf("HTML = %q", m.HTML)
	}
	wantNames(t, m, "", "invoice.pdf")
	if m.Attachments[0].ContentID != "logo@x" || m.Attachments[0].ContentType != "image/png" {
		t.Errorf("inline image = %+v", m.Attachments[0])
	}
	if string(m.Attachments[1].Data) != "%PDF-1.4" {
		t.Errorf("pdf bytes = %q", m.Attachments[1].Data)
	}
}

func TestTextPartsAroundAnAttachmentAreAllKept(t *testing.T) {
	// Apple Mail puts text before and after an inline attachment.
	raw := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nbefore\r\n" +
		"--B\r\nContent-Type: image/jpeg; name=\"photo.jpg\"\r\nContent-Disposition: inline; filename=\"photo.jpg\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nanBn\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nafter\r\n" +
		"--B--\r\n"
	m := mustParse(t, raw)
	if m.Text != "before\nafter" {
		t.Errorf("Text = %q", m.Text)
	}
	wantNames(t, m, "photo.jpg")
}

func TestAttachmentFilenames(t *testing.T) {
	part := func(headers string) string {
		return "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
			"--B\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
			"--B\r\n" + headers + "Content-Transfer-Encoding: base64\r\n\r\nZGF0YQ==\r\n" +
			"--B--\r\n"
	}
	cases := []struct {
		name    string
		headers string
		want    string
	}{
		{"plain", "Content-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\n", "a.pdf"},
		{"name parameter only", "Content-Type: application/pdf; name=\"b.pdf\"\r\n", "b.pdf"},
		{"rfc 2047 encoded word", "Content-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"=?utf-8?B?ZmFjdHV1ciDigqwucGRm?=\"\r\n", "factuur €.pdf"},
		{"rfc 2231 with charset", "Content-Type: application/pdf\r\nContent-Disposition: attachment; filename*=utf-8''caf%C3%A9.pdf\r\n", "café.pdf"},
		{"rfc 2231 continuations", "Content-Type: application/pdf\r\nContent-Disposition: attachment;\r\n filename*0*=utf-8''caf%C3%A9;\r\n filename*1*=%20menu.pdf\r\n", "café menu.pdf"},
		{"no name at all", "Content-Type: application/octet-stream\r\nContent-Disposition: attachment\r\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mustParse(t, part(tc.headers))
			wantNames(t, m, tc.want)
			if string(m.Attachments[0].Data) != "data" {
				t.Errorf("data = %q", m.Attachments[0].Data)
			}
		})
	}
}

// A text attachment keeps its bytes. An XML invoice that declares ISO-8859-1
// and is converted to UTF-8 no longer matches its own declaration.
func TestTextAttachmentBytesAreNotConverted(t *testing.T) {
	xml := encode(t, charmap.ISO8859_1, `<?xml version="1.0" encoding="ISO-8859-1"?><Invoice><Note>Café</Note></Invoice>`)
	raw := "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
		"--B\r\nContent-Type: text/xml; charset=iso-8859-1; name=\"invoice.xml\"\r\n" +
		"Content-Disposition: attachment; filename=\"invoice.xml\"\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + xml + "\r\n" +
		"--B--\r\n"
	m := mustParse(t, raw)
	wantNames(t, m, "invoice.xml")
	if got := string(m.Attachments[0].Data); got != xml {
		t.Errorf("attachment bytes changed:\n got %q\nwant %q", got, xml)
	}
}

func TestDamagedTransferEncodingStillYieldsData(t *testing.T) {
	cases := []struct {
		name, encoding, body, want string
	}{
		{"base64 with stray characters and no padding", "base64", "aGVs bG8g\r\nd29y!bGQ", "hello world"},
		{"base64 in the url-safe alphabet", "base64", base64.URLEncoding.EncodeToString([]byte{0xfb, 0xff, 0xfe}), "\xfb\xff\xfe"},
		{"quoted-printable with a bare equals sign", "quoted-printable", "a=b =3D c=\r\nd", "a=b = cd"},
		{"quoted-printable in lower case hex", "quoted-printable", "caf=c3=a9", "café"},
		{"uuencode", "x-uuencode", "begin 644 a.txt\n#0V%T\n`\nend\n", "Cat"},
		{"an unknown encoding is passed through", "x-whatever", "as is", "as is"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(decodeTransfer([]byte(tc.body), tc.encoding)); got != tc.want {
				t.Errorf("decodeTransfer = %q, want %q", got, tc.want)
			}
		})
	}
}

const forwarded = "From: a@sender.test\r\nSubject: Fwd: see attached mail\r\n" +
	"Content-Type: multipart/mixed; boundary=OUT\r\n\r\n" +
	"--OUT\r\nContent-Type: text/plain\r\n\r\nforwarding this\r\n" +
	"--OUT\r\nContent-Type: message/rfc822\r\nContent-Disposition: attachment\r\n\r\n" +
	"From: supplier@shop.test\r\nSubject: Factuur 2026/0042\r\nContent-Type: multipart/mixed; boundary=IN\r\n\r\n" +
	"--IN\r\nContent-Type: text/plain\r\n\r\ninner body\r\n" +
	"--IN\r\nContent-Type: application/pdf; name=\"factuur.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQ=\r\n" +
	"--IN--\r\n" +
	"\r\n--OUT--\r\n"

func TestForwardedAsAttachment(t *testing.T) {
	m := mustParse(t, forwarded)
	if m.Text != "forwarding this" {
		t.Errorf("Text = %q, the inner body must not leak into the outer one", m.Text)
	}
	wantNames(t, m, "Factuur 2026 0042.eml", "factuur.pdf")
	if m.Attachments[0].ContentType != "message/rfc822" || !bytes.Contains(m.Attachments[0].Data, []byte("inner body")) {
		t.Errorf("eml attachment = %q %q", m.Attachments[0].ContentType, m.Attachments[0].Data)
	}
	if string(m.Attachments[1].Data) != "%PDF-1.4" {
		t.Errorf("inner pdf = %q", m.Attachments[1].Data)
	}

	closed, _ := ParseBytes([]byte(forwarded), Options{})
	wantNames(t, closed, "Factuur 2026 0042.eml")
}

// tnefFile builds a minimal winmail.dat holding one file.
func tnefFile(title string, data []byte) []byte {
	var b bytes.Buffer
	object := func(level byte, name uint16, payload []byte) {
		b.WriteByte(level)
		binary.Write(&b, binary.LittleEndian, name)
		binary.Write(&b, binary.LittleEndian, uint16(0x0006))
		binary.Write(&b, binary.LittleEndian, uint32(len(payload)))
		b.Write(payload)
		binary.Write(&b, binary.LittleEndian, uint16(0)) // checksum, not verified
	}
	binary.Write(&b, binary.LittleEndian, uint32(0x223e9f78))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	object(2, 0x9002, make([]byte, 14))            // attAttachRendData starts an attachment
	object(2, 0x8010, append([]byte(title), 0x00)) // attAttachTitle
	object(2, 0x800f, data)                        // attAttachData
	return b.Bytes()
}

func TestOutlookWinmailDat(t *testing.T) {
	wrap := func(payload []byte) string {
		return "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
			"--B\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
			"--B\r\nContent-Type: application/ms-tnef; name=\"winmail.dat\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
			base64.StdEncoding.EncodeToString(payload) + "\r\n" +
			"--B--\r\n"
	}

	m := mustParse(t, wrap(tnefFile("invoice.pdf", []byte("%PDF-1.4"))))
	wantNames(t, m, "invoice.pdf")
	if m.Attachments[0].ContentType != "application/pdf" || string(m.Attachments[0].Data) != "%PDF-1.4" {
		t.Errorf("extracted = %+v", m.Attachments[0])
	}

	// One that cannot be read is forwarded as it is, so nothing is lost.
	broken := mustParse(t, wrap([]byte("not tnef")))
	wantNames(t, broken, "winmail.dat")

	closed, _ := ParseBytes([]byte(wrap(tnefFile("invoice.pdf", []byte("%PDF-1.4")))), Options{})
	wantNames(t, closed, "winmail.dat")
}

func TestSignedEncryptedReportsAndInvites(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		text      string
		wantNames []string
		wantTypes []string
	}{
		{
			name: "s/mime signed",
			raw: "Content-Type: multipart/signed; protocol=\"application/pkcs7-signature\"; micalg=sha-256; boundary=S\r\n\r\n" +
				"--S\r\nContent-Type: text/plain\r\n\r\nsigned text\r\n" +
				"--S\r\nContent-Type: application/pkcs7-signature; name=\"smime.p7s\"\r\nContent-Transfer-Encoding: base64\r\n" +
				"Content-Disposition: attachment; filename=\"smime.p7s\"\r\n\r\nc2ln\r\n--S--\r\n",
			text:      "signed text",
			wantNames: []string{"smime.p7s"},
			wantTypes: []string{"application/pkcs7-signature"},
		},
		{
			name: "pgp signed",
			raw: "Content-Type: multipart/signed; protocol=\"application/pgp-signature\"; boundary=S\r\n\r\n" +
				"--S\r\nContent-Type: text/plain\r\n\r\nsigned text\r\n" +
				"--S\r\nContent-Type: application/pgp-signature\r\n\r\n-----BEGIN PGP SIGNATURE-----\r\n--S--\r\n",
			text:      "signed text",
			wantNames: []string{""},
			wantTypes: []string{"application/pgp-signature"},
		},
		{
			name: "s/mime encrypted",
			raw: "Content-Type: application/pkcs7-mime; smime-type=enveloped-data; name=\"smime.p7m\"\r\n" +
				"Content-Disposition: attachment; filename=\"smime.p7m\"\r\nContent-Transfer-Encoding: base64\r\n\r\nZW5j\r\n",
			text:      "",
			wantNames: []string{"smime.p7m"},
			wantTypes: []string{"application/pkcs7-mime"},
		},
		{
			name: "delivery status notification",
			raw: "From: MAILER-DAEMON@mx.test\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=D\r\n\r\n" +
				"--D\r\nContent-Type: text/plain\r\n\r\nYour message could not be delivered.\r\n" +
				"--D\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; mx.test\r\n\r\nFinal-Recipient: rfc822; x@y.test\r\nAction: failed\r\n" +
				"--D\r\nContent-Type: text/rfc822-headers\r\n\r\nSubject: original\r\n" +
				"--D--\r\n",
			text:      "Your message could not be delivered.",
			wantNames: []string{"", ""},
			wantTypes: []string{"message/delivery-status", "text/rfc822-headers"},
		},
		{
			name: "calendar invite",
			raw: "Content-Type: multipart/alternative; boundary=C\r\n\r\n" +
				"--C\r\nContent-Type: text/plain\r\n\r\nYou are invited.\r\n" +
				"--C\r\nContent-Type: text/calendar; method=REQUEST; charset=utf-8\r\n\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n" +
				"--C--\r\n",
			text:      "You are invited.",
			wantNames: []string{"invite.ics"},
			wantTypes: []string{"text/calendar"},
		},
		{
			name: "amp alternative is dropped",
			raw: "Content-Type: multipart/alternative; boundary=C\r\n\r\n" +
				"--C\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
				"--C\r\nContent-Type: text/x-amp-html\r\n\r\n<html amp4email></html>\r\n" +
				"--C\r\nContent-Type: text/html\r\n\r\n<p>plain</p>\r\n" +
				"--C--\r\n",
			text:      "plain",
			wantNames: []string{},
			wantTypes: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mustParse(t, tc.raw)
			if m.Text != tc.text {
				t.Errorf("Text = %q, want %q", m.Text, tc.text)
			}
			wantNames(t, m, tc.wantNames...)
			for i, a := range m.Attachments {
				if a.ContentType != tc.wantTypes[i] {
					t.Errorf("attachment %d type = %q, want %q", i, a.ContentType, tc.wantTypes[i])
				}
				if len(a.Data) == 0 {
					t.Errorf("attachment %d is empty", i)
				}
			}
		})
	}
}

func TestHeadersKeepOrderDuplicatesAndEncoding(t *testing.T) {
	raw := "Received: from b.test by c.test\r\n" +
		"Received: from a.test\r\n\tby b.test\r\n" +
		": orphan\r\n" +
		"this line is not a header\r\n" +
		"Subject: =?utf-8?q?Caf=C3=A9?= invoice\r\n" +
		"X-Raw: caf\xe9\r\n" +
		"From: =?iso-8859-1?Q?Andr=E9?= <andre@sender.test>\r\n" +
		"\r\nbody\r\n"
	m := mustParse(t, raw)

	want := []Header{
		{"Received", "from b.test by c.test"},
		{"Received", "from a.test by b.test"},
		{"", "orphan"},
		{"Subject", "=?utf-8?q?Caf=C3=A9?= invoice"},
		{"X-Raw", "café"},
		{"From", "=?iso-8859-1?Q?Andr=E9?= <andre@sender.test>"},
	}
	if len(m.Headers) != len(want) {
		t.Fatalf("headers = %+v", m.Headers)
	}
	for i, h := range want {
		if m.Headers[i] != h {
			t.Errorf("header %d = %+v, want %+v", i, m.Headers[i], h)
		}
	}
	if got := m.GetDecoded("subject"); got != "Café invoice" {
		t.Errorf("decoded subject = %q", got)
	}
	if got := m.GetDecoded("From"); got != "André <andre@sender.test>" {
		t.Errorf("decoded from = %q", got)
	}
	if strings.TrimSpace(m.Text) != "body" {
		t.Errorf("Text = %q", m.Text)
	}
}

func TestDecodeWordsNeverFails(t *testing.T) {
	cases := map[string]string{
		"plain":                                  "plain",
		"=?utf-8?b?4oKs?= 5":                     "€ 5",
		"=?x-made-up?q?abc?=":                    "abc",
		"=?utf-8?q?broken":                       "=?utf-8?q?broken",
		"=?windows-1252?q?=80?= =?utf-8?q?_ok?=": "€ ok",
		"=?gb2312?b?" + "t6LGsQ==" + "?=":        "发票",
	}
	for in, want := range cases {
		if got := DecodeWords(in); got != want {
			t.Errorf("DecodeWords(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOddMessagesStillParse(t *testing.T) {
	cases := map[string]string{
		"empty":                    "",
		"headers only":             "Subject: nothing else\r\n",
		"headers and blank line":   "Subject: x\r\n\r\n",
		"no headers at all":        "\r\njust a body\r\n",
		"multipart without bounds": "Content-Type: multipart/mixed\r\n\r\ntext without any boundary\r\n",
		"boundary never closed":    "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nunterminated\r\n",
		"attachment only":          "Content-Type: application/pdf; name=\"a.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQ=\r\n",
		"binary noise":             "\x00\x01\x02\xff\xfe\r\n\r\n\x00\xff",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			m, _ := ParseBytes([]byte(raw), DefaultOptions())
			if m == nil {
				t.Fatal("nil message")
			}
			if !bytes.Equal(m.Raw, []byte(raw)) {
				t.Error("Raw was not kept")
			}
		})
	}

	m := mustParse(t, cases["boundary never closed"])
	if strings.TrimSpace(m.Text) != "unterminated" {
		t.Errorf("unterminated part: Text = %q", m.Text)
	}
	m = mustParse(t, cases["attachment only"])
	wantNames(t, m, "a.pdf")
	if m.Text != "" {
		t.Errorf("attachment-only Text = %q", m.Text)
	}
}

// What comes back when the structure cannot be read at all: the headers, and
// the whole message as an attachment.
func TestFallbackKeepsTheWholeMessage(t *testing.T) {
	raw := []byte("Subject: broken\r\nContent-Type: text/plain; charset=iso-8859-1\r\n\r\ncaf\xe9\r\n")
	headers, offset := parseHeaders(raw)
	m := fallback(raw, headers, offset)

	if m.Get("Subject") != "broken" || strings.TrimSpace(m.Text) != "café" {
		t.Errorf("subject %q text %q", m.Get("Subject"), m.Text)
	}
	wantNames(t, m, "message.eml")
	if !bytes.Equal(m.Attachments[0].Data, raw) {
		t.Error("the attached message is not the original")
	}
}
