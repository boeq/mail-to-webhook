// Package ui is the operator interface: what arrived, what happened to it,
// and the few actions an operator needs.
//
// Pages are rendered on the server and refreshed by htmx fragments that
// re-fetch themselves, so polling starts and stops with the data. Everything
// a page shows that came from a mail, a webhook response or a request goes
// through html/template's escaping; mail HTML is shown as source and never
// rendered.
package ui

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/boeq/mail-to-webhook/internal/config"
	"github.com/boeq/mail-to-webhook/internal/delivery"
	"github.com/boeq/mail-to-webhook/internal/recipients"
	"github.com/boeq/mail-to-webhook/internal/store"
	"github.com/boeq/mail-to-webhook/internal/tlscert"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const (
	prefix   = "/ui"
	sinkPath = prefix + "/api/webhook-sink"

	contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'"
)

type UI struct {
	Cfg      *config.Config
	Store    *store.Store
	Engine   *delivery.Engine
	Checker  *recipients.Checker
	Log      *slog.Logger
	Version  string
	SMTPAddr string
	// TLS reports the STARTTLS certificate. Nil means there is none configured.
	TLS func() tlscert.Status
	Now func() time.Time

	once      sync.Once
	fragments *template.Template
	pages     map[string]*template.Template
}

// Mount registers the UI under /ui.
func Mount(mux *http.ServeMux, u *UI) {
	h := u.Handler()
	mux.Handle(prefix, h)
	mux.Handle(prefix+"/", h)
}

// Handler serves everything under /ui.
func (u *UI) Handler() http.Handler {
	u.once.Do(u.init)

	static, _ := fs.Sub(staticFS, "static")
	files := http.StripPrefix(prefix+"/static/", http.FileServerFS(static))

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET "+prefix+"/{$}", u.dashboardPage)
	mux.HandleFunc("GET "+prefix+"/messages", u.messagesPage)
	mux.HandleFunc("GET "+prefix+"/messages/{id}", u.messagePage)
	mux.HandleFunc("GET "+prefix+"/addresses", u.addressesPage)
	mux.HandleFunc("GET "+prefix+"/refused", u.refusedPage)
	mux.HandleFunc("GET "+prefix+"/settings", u.settingsPage)
	mux.HandleFunc("GET "+prefix+"/try", u.tryPage)
	mux.Handle("GET "+prefix+"/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		files.ServeHTTP(w, r)
	}))

	api := prefix + "/api"
	mux.HandleFunc("GET "+api+"/shell/status", u.shellFragment)
	mux.HandleFunc("GET "+api+"/dashboard/panel", u.dashboardFragment)
	mux.HandleFunc("GET "+api+"/messages/table", u.messagesFragment)
	mux.HandleFunc("GET "+api+"/messages/{id}/panel", u.messageFragment)
	mux.HandleFunc("GET "+api+"/messages/{id}/raw.eml", u.rawDownload)
	mux.HandleFunc("GET "+api+"/messages/{id}/attachments/{n}", u.attachmentDownload)
	mux.HandleFunc("POST "+api+"/messages/{id}/webhook/resend", u.resend)
	mux.HandleFunc("GET "+api+"/addresses/table", u.addressesFragment)
	mux.HandleFunc("POST "+api+"/addresses/check", u.addressCheck)
	mux.HandleFunc("POST "+api+"/addresses/forget", u.addressForget)
	mux.HandleFunc("GET "+api+"/refused/table", u.refusedFragment)
	mux.HandleFunc("POST "+api+"/try", u.trySend)
	mux.HandleFunc("POST "+sinkPath, u.sink)
	mux.HandleFunc(prefix+"/", u.notFound)

	return u.guard(mux)
}

func (u *UI) init() {
	if u.Now == nil {
		u.Now = time.Now
	}
	if u.Log == nil {
		u.Log = slog.New(slog.DiscardHandler)
	}
	u.fragments = template.Must(template.New("").Funcs(funcs()).ParseFS(templateFS, "templates/_*.html", "templates/base.html"))
	u.pages = map[string]*template.Template{}
	for _, name := range []string{"dashboard", "messages", "message", "addresses", "refused", "settings", "try", "notfound"} {
		page := template.Must(template.Must(u.fragments.Clone()).ParseFS(templateFS, "templates/"+name+".html"))
		u.pages[name] = page
	}
}

// guard puts the response headers, the optional login and the cross-site
// check in front of every route. The sink is the exception to the last two:
// the service itself posts to it, with the webhook's credentials, not the
// operator's browser.
func (u *UI) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// Pages carry mail content; only the static files override this.
		h.Set("Cache-Control", "private, no-store")

		if r.URL.Path == sinkPath {
			next.ServeHTTP(w, r)
			return
		}
		if !u.authorised(r) {
			h.Set("WWW-Authenticate", `Basic realm="mail-to-webhook"`)
			u.fail(w, http.StatusUnauthorized, "unauthorized", "sign in to use the operator interface")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			u.fail(w, http.StatusForbidden, "forbidden", "this action must come from the operator interface itself")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (u *UI) authorised(r *http.Request) bool {
	if u.Cfg.UIAuthUser == "" {
		return true
	}
	user, pass, ok := r.BasicAuth()
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(u.Cfg.UIAuthUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(u.Cfg.UIAuthPass)) == 1
	return ok && userOK && passOK
}

// sameOrigin is the cross-site request check. A form on another site cannot
// set the htmx header, and a script there that tries is stopped by a CORS
// preflight this server never answers. A plain form post from one of our own
// pages has no such header, so the browser's own word on the origin counts too.
func sameOrigin(r *http.Request) bool {
	if r.Header.Get("HX-Request") == "true" || r.Header.Get("Sec-Fetch-Site") == "same-origin" {
		return true
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if parsed, err := url.Parse(origin); err == nil && parsed.Host != "" && parsed.Host == r.Host {
			return true
		}
	}
	return false
}

type navItem struct {
	Href    string
	Label   string
	Icon    string
	Current bool
}

// nav marks the current place. The dashboard is current on /ui/ only; every
// other item is current on its own path and anything under it.
func nav(path string) []navItem {
	items := []navItem{
		{Href: prefix + "/", Label: "Dashboard", Icon: "home"},
		{Href: prefix + "/messages", Label: "Messages", Icon: "list"},
		{Href: prefix + "/addresses", Label: "Addresses", Icon: "repo"},
		{Href: prefix + "/refused", Label: "Refused", Icon: "x-circle"},
		{Href: prefix + "/settings", Label: "Settings", Icon: "gear"},
	}
	for i := range items {
		href := items[i].Href
		if i == 0 {
			items[i].Current = path == href
			continue
		}
		items[i].Current = path == href || strings.HasPrefix(path, href+"/")
	}
	return items
}

type page struct {
	Nav   []navItem
	Shell shellStatus
	Doc   any
}

// renderPage writes a full page: the shell around one page template.
func (u *UI) renderPage(w http.ResponseWriter, r *http.Request, status int, name string, doc any) {
	u.execute(w, status, u.pages[name], "base", page{Nav: nav(r.URL.Path), Shell: u.shellStatus(), Doc: doc})
}

// renderFragment writes one fragment, the part of a page htmx swaps in.
func (u *UI) renderFragment(w http.ResponseWriter, name string, doc any) {
	u.execute(w, http.StatusOK, u.fragments, name, doc)
}

func (u *UI) execute(w http.ResponseWriter, status int, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		u.Log.Error("could not render the page", "template", name, "err", err)
		http.Error(w, "The page could not be rendered.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (u *UI) notFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, prefix+"/api/") {
		u.fail(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	u.renderPage(w, r, http.StatusNotFound, "notfound", nil)
}

// writeJSON answers an action. Only values built here go in: never a setting.
func (u *UI) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// fail answers with the error envelope every action shares.
func (u *UI) fail(w http.ResponseWriter, status int, code, message string) {
	u.failWith(w, status, code, message, map[string]any{})
}

func (u *UI) failWith(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	u.writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message, "details": details},
	})
}

type shellStatus struct {
	StatusURL string
	SMTP      string
	Webhook   string
	// Check is "ok", "failing" or "off".
	Check      string
	CheckError string
	// TLS is "on", "waiting" (no certificate yet) or "off".
	TLS      string
	TLSTitle string
}

func (u *UI) shellStatus() shellStatus {
	st := shellStatus{StatusURL: prefix + "/api/shell/status", SMTP: u.SMTPAddr, Check: "off"}
	if parsed, err := url.Parse(u.Cfg.WebhookURL); err == nil {
		st.Webhook = parsed.Host
	}
	if h := u.Checker.Health(); h.Enabled {
		st.Check = "ok"
		if !h.OK {
			st.Check = "failing"
			st.CheckError = h.LastError
		}
	}
	st.TLS, st.TLSTitle = "off", "No certificate is configured, so mail arrives unencrypted"
	if u.TLS != nil {
		switch t := u.TLS(); {
		case t.Ready:
			st.TLS = "on"
			st.TLSTitle = "Certificate for " + t.Host + ", valid until " + t.NotAfter.UTC().Format("2006-01-02")
		case t.Mode != tlscert.ModeOff:
			st.TLS = "waiting"
			st.TLSTitle = "No certificate yet; STARTTLS is offered once one has been obtained"
		}
	}
	return st
}

func (u *UI) shellFragment(w http.ResponseWriter, _ *http.Request) {
	u.renderFragment(w, "_shell_status", u.shellStatus())
}
