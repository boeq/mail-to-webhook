package smtpsrv

import (
	"testing"

	"github.com/boeq/mail-to-webhook/internal/config"
)

// The domain is case-insensitive and stored lower-case by consumers; the
// local part may be case-sensitive and is never touched.
func TestNormaliseRecipient(t *testing.T) {
	cases := map[string]string{
		"AbC123.purchase@Inbound.Example.ORG": "AbC123.purchase@inbound.example.org",
		"plain@example.org":                   "plain@example.org",
		"\"odd@local\"@EXAMPLE.org":           "\"odd@local\"@example.org",
		"no-at-sign":                          "no-at-sign",
	}
	for in, want := range cases {
		if got := NormaliseRecipient(in); got != want {
			t.Errorf("NormaliseRecipient(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRelayPolicy(t *testing.T) {
	open := &Backend{Cfg: &config.Config{}}
	domains := &Backend{Cfg: &config.Config{AllowedDomains: []string{"example.org"}}}
	boxes := &Backend{Cfg: &config.Config{AllowedDomains: []string{"example.org"}, AllowedMailboxes: []string{"inbound"}}}

	cases := []struct {
		backend *Backend
		addr    string
		want    bool
	}{
		{open, "anyone@anywhere.test", true},
		{open, "not-an-address", false},
		{open, "@example.org", false},
		{domains, "x@example.org", true},
		{domains, "x@EXAMPLE.org", true},
		{domains, "x@sub.example.org", false},
		{domains, "x@notexample.org", false},
		{boxes, "inbound@example.org", true},
		{boxes, "Inbound+order42@example.org", true},
		{boxes, "other@example.org", false},
	}
	for _, tc := range cases {
		if got := tc.backend.accepts(tc.addr); got != tc.want {
			t.Errorf("accepts(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
