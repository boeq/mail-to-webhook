package ui

import (
	"fmt"
	"html"
	"html/template"
	"strconv"
	"time"

	"github.com/boeq/mail-to-webhook/internal/store"
)

// tones maps kind → value → tone. One state has one colour wherever its badge
// appears, and no template picks a colour itself: a new state gets an entry
// here. Any value not listed is neutral.
var tones = map[string]map[string]string{
	"webhook":   {store.StatusPending: "neutral", store.StatusDelivered: "done", store.StatusDeferred: "wait", store.StatusFailed: "danger"},
	"message":   {store.StatusPending: "neutral", store.StatusDelivered: "done", store.StatusDeferred: "wait", store.StatusFailed: "danger"},
	"recipient": {store.RecipientKnown: "done", store.RecipientUnknown: "neutral"},
	"signature": {"valid": "done", "unsigned": "neutral", "missing": "danger", "invalid": "danger", "stale": "danger"},
	"setting":   {"set": "done", "not set": "neutral"},
	"reason":    {store.ReasonBlocked: "wait", store.ReasonTooManyBad: "wait"},
	"check":     {"ok": "done", "failing": "danger", "off": "neutral"},
	"tls":       {"on": "done", "waiting": "wait", "off": "neutral"},
}

func toneOf(kind, value string) string {
	if tone, ok := tones[kind][value]; ok {
		return tone
	}
	return "neutral"
}

// pulseOf is true only for a delivery that is being worked on right now.
func pulseOf(kind, value string) bool {
	return (kind == "webhook" || kind == "message") && value == store.StatusPending
}

// badge is the one place a status becomes markup. The class comes from the
// tone table and the label is escaped, so the result is safe to mark as HTML
// whatever the value is.
func badge(kind, value string) template.HTML {
	class := "badge badge--" + toneOf(kind, value)
	if pulseOf(kind, value) {
		class += " badge--pulse"
	}
	return template.HTML(`<span class="` + class + `">` + html.EscapeString(value) + `</span>`)
}

// icon renders one Octicon inline. An unknown name is an error at render
// time, never a blank space.
func icon(name string, class ...string) (template.HTML, error) {
	path, ok := iconPaths[name]
	if !ok {
		return "", fmt.Errorf("unknown icon %q", name)
	}
	classes := "icon"
	for _, c := range class {
		classes += " " + html.EscapeString(c)
	}
	return template.HTML(`<svg class="` + classes + `" viewBox="0 0 16 16" width="16" height="16" fill="currentColor" aria-hidden="true" focusable="false"><path d="` + path + `" /></svg>`), nil
}

// markSVG is the wordmark's diamond.
const markSVG = `<svg class="mark" viewBox="0 0 32 32" width="22" height="22" aria-hidden="true">` +
	`<rect width="32" height="32" rx="7" fill="var(--color-surface-sunken)" />` +
	`<rect x="0.5" y="0.5" width="31" height="31" rx="6.5" fill="none" stroke="var(--color-line-default)" />` +
	`<path d="M16 7.5 24.5 16 16 24.5 7.5 16Z" fill="var(--color-accent-fg)" />` +
	`<path d="M16 12.5 19.5 16 16 19.5 12.5 16Z" fill="var(--color-surface-sunken)" />` +
	`</svg>`

func mark() template.HTML { return template.HTML(markSVG) }

// humanBytes: 1536 → "1.5 KiB".
func humanBytes(value any) string {
	var n float64
	switch v := value.(type) {
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	default:
		return "—"
	}
	for _, unit := range []string{"B", "KiB", "MiB", "GiB"} {
		if n < 1024 {
			if unit == "B" {
				return strconv.Itoa(int(n)) + " B"
			}
			return strconv.FormatFloat(n, 'f', 1, 64) + " " + unit
		}
		n /= 1024
	}
	return strconv.FormatFloat(n, 'f', 1, 64) + " TiB"
}

// timestamp prints a stored time as it is: UTC, ISO 8601, no relative wording.
func timestamp(value any) string {
	var t time.Time
	switch v := value.(type) {
	case time.Time:
		t = v
	case *time.Time:
		if v == nil {
			return "—"
		}
		t = *v
	default:
		return "—"
	}
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

func dash(v string) string {
	if v == "" {
		return "—"
	}
	return v
}

func funcs() template.FuncMap {
	return template.FuncMap{
		"icon":    icon,
		"mark":    mark,
		"badge":   badge,
		"tone":    toneOf,
		"shortID": store.ShortID,
		"bytes":   humanBytes,
		"ts":      timestamp,
		"dash":    dash,
	}
}
