package consumer

import (
	"strings"
	"testing"
)

// TestValidateCatchesEachRule feeds a deliberately broken payload per rule and
// asserts the violation is reported. Without this the pre-flight check could
// silently pass everything.
func TestValidateCatchesEachRule(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing FromFull", `{"ToFull":[{"Email":"a@b.com"}]}`, "FromFull fails required"},
		{"FromFull not object", `{"FromFull":"a@b.com","ToFull":[{"Email":"a@b.com"}]}`, "FromFull fails array"},
		{"FromFull.Email empty", `{"FromFull":{"Email":""},"ToFull":[{"Email":"a@b.com"}]}`, "FromFull.Email fails required"},
		{"FromFull.Email invalid", `{"FromFull":{"Email":"Mailer Daemon"},"ToFull":[{"Email":"a@b.com"}]}`, "FromFull.Email fails email"},
		{"ToFull missing", `{"FromFull":{"Email":"a@b.com"}}`, "ToFull fails required"},
		{"ToFull empty", `{"FromFull":{"Email":"a@b.com"},"ToFull":[]}`, "ToFull fails min:1"},
		{"ToFull entry invalid", `{"FromFull":{"Email":"a@b.com"},"ToFull":[{"Email":"nope"}]}`, "ToFull.0.Email fails email"},
		{"CcFull entry invalid", `{"FromFull":{"Email":"a@b.com"},"ToFull":[{"Email":"a@b.com"}],"CcFull":[{"Email":"x"}]}`, "CcFull.0.Email fails email"},
		{"Subject not string", `{"FromFull":{"Email":"a@b.com"},"ToFull":[{"Email":"a@b.com"}],"Subject":42}`, "Subject fails string"},
		{"header name empty", `{"FromFull":{"Email":"a@b.com"},"ToFull":[{"Email":"a@b.com"}],"Headers":[{"Name":"","Value":"x"}]}`, "Headers.0.Name fails required"},
		{"attachment content empty", `{"FromFull":{"Email":"a@b.com"},"ToFull":[{"Email":"a@b.com"}],"Attachments":[{"Name":"a.pdf","Content":"","ContentType":"application/pdf"}]}`, "Attachments.0.Content fails required"},
		{"attachment name missing", `{"FromFull":{"Email":"a@b.com"},"ToFull":[{"Email":"a@b.com"}],"Attachments":[{"Content":"eA==","ContentType":"application/pdf"}]}`, "Attachments.0.Name fails required"},
		{"attachment type missing", `{"FromFull":{"Email":"a@b.com"},"ToFull":[{"Email":"a@b.com"}],"Attachments":[{"Name":"a","Content":"eA=="}]}`, "Attachments.0.ContentType fails required"},
		{"HtmlBody not string", `{"FromFull":{"Email":"a@b.com"},"ToFull":[{"Email":"a@b.com"}],"HtmlBody":["<p>"]}`, "HtmlBody fails string"},
		{"attachment content id not string", `{"FromFull":{"Email":"a@b.com"},"ToFull":[{"Email":"a@b.com"}],"Attachments":[{"Name":"a","Content":"eA==","ContentType":"image/png","ContentID":7}]}`, "Attachments.0.ContentID fails string"},
		{"not json", `{oops`, "(body) fails json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Violations(Validate([]byte(tc.body)))
			if !strings.Contains(got, tc.want) {
				t.Errorf("Validate() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestValidateAcceptsAGoodPayload(t *testing.T) {
	body := `{
	  "FromFull": {"Email":"alice@sender.test","Name":"Alice"},
	  "ToFull": [{"Email":"inbound+h@example.org"}],
	  "CcFull": [],
	  "Subject": "hi", "TextBody": "body", "Date": "Sun, 16 Aug 2026 09:20:00 +0000",
	  "Headers": [{"Name":"Subject","Value":"hi"},{"Name":"X-Empty","Value":""}],
	  "Attachments": [{"Name":"a.pdf","Content":"eA==","ContentType":"application/pdf"}]
	}`
	if v := Validate([]byte(body)); len(v) > 0 {
		t.Errorf("valid payload rejected: %s", Violations(v))
	}
}

// Nullable fields may be absent or null.
func TestValidateNullables(t *testing.T) {
	body := `{"FromFull":{"Email":"a@b.com"},"ToFull":[{"Email":"a@b.com"}],
	          "CcFull":null,"Subject":null,"TextBody":null,"Date":null,
	          "Headers":null,"Attachments":null}`
	if v := Validate([]byte(body)); len(v) > 0 {
		t.Errorf("nullables rejected: %s", Violations(v))
	}
}

func TestIsEmail(t *testing.T) {
	valid := []string{
		"a@b.com", "first.last@sub.example.co.uk", "user+tag@example.org",
		"x'y@example.com", "a_b-c@example-host.com",
	}
	for _, s := range valid {
		if !IsEmail(s) {
			t.Errorf("IsEmail(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"", "plainstring", "Mailer Daemon", "a@", "@b.com", "a b@example.com",
		"a@localhost", "a@b", ".a@example.com", "a.@example.com", "a..b@example.com",
		"a@-example.com", "a@example-.com", "a@example..com", "a@192.0.2.1",
		"пользователь@почта.рф", "a@example.com\n", "a@exam ple.com",
		strings.Repeat("a", 65) + "@example.com",
	}
	for _, s := range invalid {
		if IsEmail(s) {
			t.Errorf("IsEmail(%q) = true, want false", s)
		}
	}
}
