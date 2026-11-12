// Package consumer holds the rules every payload must satisfy before it is
// posted: what a receiver that validates Postmark-shaped JSON strictly will
// insist on.
//
// Posting something the endpoint rejects with 422 means a bounced email: the message is gone and the sender gets a
// delivery failure for what is really our bug. Validating locally turns that
// into a log line and a 451, which keeps the mail queued at the sender while
// the problem is fixed.
//
//	FromFull                  required, array
//	FromFull.Email            required, email
//	ToFull                    required, array, min:1
//	ToFull.*.Email            required, email
//	CcFull                    nullable, array
//	CcFull.*.Email            required, email
//	Subject                   nullable, string
//	TextBody                  nullable, string
//	HtmlBody                  nullable, string
//	Date                      nullable, string
//	Headers                   nullable, array
//	Headers.*.Name            required, string
//	Headers.*.Value           nullable, string
//	Attachments               nullable, array
//	Attachments.*.Name        required, string
//	Attachments.*.Content     required, string
//	Attachments.*.ContentType required, string
//	Attachments.*.ContentID   nullable, string
package consumer

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Violation is one failed rule, named the way the consumer reports it.
type Violation struct {
	Field string
	Rule  string
}

func (v Violation) String() string { return v.Field + " fails " + v.Rule }

// Violations renders a list for logging.
func Violations(vs []Violation) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, v.String())
	}
	return strings.Join(parts, "; ")
}

// Validate checks the exact JSON bytes that would be posted.
func Validate(body []byte) []Violation {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return []Violation{{Field: "(body)", Rule: "json"}}
	}
	var out []Violation

	// FromFull: required, array (a JSON object counts as an array here).
	from, ok := doc["FromFull"]
	switch {
	case !present(from):
		out = append(out, Violation{"FromFull", "required"})
	case !isObject(from):
		out = append(out, Violation{"FromFull", "array"})
	default:
		out = append(out, checkEmail("FromFull.Email", from.(map[string]any)["Email"])...)
	}

	// ToFull: required, array, min:1
	to, ok := doc["ToFull"]
	list, isList := to.([]any)
	switch {
	case !ok || to == nil:
		out = append(out, Violation{"ToFull", "required"})
	case !isList:
		out = append(out, Violation{"ToFull", "array"})
	case len(list) == 0:
		out = append(out, Violation{"ToFull", "min:1"})
	default:
		for i, item := range list {
			out = append(out, checkAddr(fmt.Sprintf("ToFull.%d", i), item)...)
		}
	}

	// CcFull: nullable, array
	if cc, ok := doc["CcFull"]; ok && cc != nil {
		list, isList := cc.([]any)
		if !isList {
			out = append(out, Violation{"CcFull", "array"})
		} else {
			for i, item := range list {
				out = append(out, checkAddr(fmt.Sprintf("CcFull.%d", i), item)...)
			}
		}
	}

	// Nullable strings.
	for _, f := range []string{"Subject", "TextBody", "HtmlBody", "Date"} {
		if v, ok := doc[f]; ok && v != nil {
			if _, isStr := v.(string); !isStr {
				out = append(out, Violation{f, "string"})
			}
		}
	}

	// Headers: nullable array of {Name required string, Value nullable string}
	if hs, ok := doc["Headers"]; ok && hs != nil {
		list, isList := hs.([]any)
		if !isList {
			out = append(out, Violation{"Headers", "array"})
		} else {
			for i, item := range list {
				field := fmt.Sprintf("Headers.%d", i)
				obj, isObj := item.(map[string]any)
				if !isObj {
					out = append(out, Violation{field, "array"})
					continue
				}
				out = append(out, checkRequiredString(field+".Name", obj["Name"])...)
				if v, ok := obj["Value"]; ok && v != nil {
					if _, isStr := v.(string); !isStr {
						out = append(out, Violation{field + ".Value", "string"})
					}
				}
			}
		}
	}

	// Attachments: nullable array of {Name, Content, ContentType} all required strings
	if as, ok := doc["Attachments"]; ok && as != nil {
		list, isList := as.([]any)
		if !isList {
			out = append(out, Violation{"Attachments", "array"})
		} else {
			for i, item := range list {
				field := fmt.Sprintf("Attachments.%d", i)
				obj, isObj := item.(map[string]any)
				if !isObj {
					out = append(out, Violation{field, "array"})
					continue
				}
				for _, key := range []string{"Name", "Content", "ContentType"} {
					out = append(out, checkRequiredString(field+"."+key, obj[key])...)
				}
				if v, ok := obj["ContentID"]; ok && v != nil {
					if _, isStr := v.(string); !isStr {
						out = append(out, Violation{field + ".ContentID", "string"})
					}
				}
			}
		}
	}
	return out
}

func checkAddr(field string, item any) []Violation {
	obj, ok := item.(map[string]any)
	if !ok {
		return []Violation{{field, "array"}}
	}
	return checkEmail(field+".Email", obj["Email"])
}

func checkEmail(field string, v any) []Violation {
	s, ok := v.(string)
	if !present(v) || (ok && s == "") {
		return []Violation{{field, "required"}}
	}
	if !ok {
		return []Violation{{field, "email"}}
	}
	if !IsEmail(s) {
		return []Violation{{field, "email"}}
	}
	return nil
}

// checkRequiredString: null, "" and [] all fail "required".
func checkRequiredString(field string, v any) []Violation {
	s, ok := v.(string)
	if !present(v) || (ok && s == "") {
		return []Violation{{field, "required"}}
	}
	if !ok {
		return []Violation{{field, "string"}}
	}
	return nil
}

func present(v any) bool  { return v != nil }
func isObject(v any) bool { _, ok := v.(map[string]any); return ok }

// IsEmail is the strictest common reading of "a valid email address": ASCII
// only, an unquoted local part, and a dotted domain.
//
// It errs on the strict side on purpose. A false negative only makes us
// substitute a known-good fallback address; a false positive produces a 422
// and a bounced message.
func IsEmail(s string) bool {
	if s == "" || len(s) > 254 {
		return false
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 {
		return false
	}
	return validLocal(s[:at]) && validDomain(s[at+1:])
}

func validLocal(local string) bool {
	if local == "" || len(local) > 64 {
		return false
	}
	if local[0] == '.' || local[len(local)-1] == '.' || strings.Contains(local, "..") {
		return false
	}
	for i := 0; i < len(local); i++ {
		if !isAtext(local[i]) && local[i] != '.' {
			return false
		}
	}
	return true
}

func validDomain(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}
	// Bracketed literals like [192.0.2.1] are valid SMTP, but strict
	// validators reject them.
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false // require a dotted domain
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	// TLD must not be all digits.
	tld := labels[len(labels)-1]
	allDigits := true
	for i := 0; i < len(tld); i++ {
		if tld[i] < '0' || tld[i] > '9' {
			allDigits = false
			break
		}
	}
	return !allDigits
}

func isAtext(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0
}
