package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/boeq/mail-to-webhook/internal/config"
	"github.com/boeq/mail-to-webhook/internal/delivery"
	"github.com/boeq/mail-to-webhook/internal/store"
)

const (
	pageSize    = 50
	maxPageSize = 200

	// displayLimit caps how much of one body, source or payload a page carries.
	displayLimit = 256 << 10
)

var messageStatuses = []string{store.StatusPending, store.StatusDelivered, store.StatusDeferred, store.StatusFailed}

var refusalReasons = []string{
	store.ReasonUnknownRecipient, store.ReasonDomainNotAllowed, store.ReasonTooManyBad,
	store.ReasonLookupFailed, store.ReasonConnectionLimit, store.ReasonBlocked,
}

var errBadFilter = errors.New("bad filter")

// --- messages ---------------------------------------------------------------

type messageFilters struct {
	Status  string
	From    string
	To      string
	Subject string
	Since   string
	Until   string
	Before  string
	limit   int
}

// values is the filters as a query string, the cursor left out.
func (f messageFilters) values() url.Values {
	v := url.Values{}
	set := func(key, value string) {
		if value != "" {
			v.Set(key, value)
		}
	}
	set("status", f.Status)
	set("from", f.From)
	set("to", f.To)
	set("subject", f.Subject)
	set("since", f.Since)
	set("until", f.Until)
	if f.limit != pageSize {
		v.Set("limit", strconv.Itoa(f.limit))
	}
	return v
}

func (f messageFilters) filtered() bool {
	return f.Status != "" || f.From != "" || f.To != "" || f.Subject != "" || f.Since != "" || f.Until != ""
}

func parseMessageFilters(q url.Values) (messageFilters, store.MessageFilter, error) {
	f := messageFilters{
		Status:  strings.TrimSpace(q.Get("status")),
		From:    strings.TrimSpace(q.Get("from")),
		To:      strings.TrimSpace(q.Get("to")),
		Subject: strings.TrimSpace(q.Get("subject")),
		Since:   strings.TrimSpace(q.Get("since")),
		Until:   strings.TrimSpace(q.Get("until")),
		Before:  strings.TrimSpace(q.Get("before")),
		limit:   pageSize,
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		f.limit = min(n, maxPageSize)
	}
	sf := store.MessageFilter{Status: f.Status, From: f.From, To: f.To, Subject: f.Subject, Before: f.Before, Limit: f.limit}
	if f.Status != "" && !contains(messageStatuses, f.Status) {
		return f, sf, fmt.Errorf("%w: unknown status", errBadFilter)
	}
	var err error
	if f.Since != "" {
		if sf.Since, err = time.Parse("2006-01-02", f.Since); err != nil {
			return f, sf, fmt.Errorf("%w: from date must look like 2026-01-31", errBadFilter)
		}
	}
	if f.Until != "" {
		until, err := time.Parse("2006-01-02", f.Until)
		if err != nil {
			return f, sf, fmt.Errorf("%w: to date must look like 2026-01-31", errBadFilter)
		}
		sf.Until = until.Add(24 * time.Hour) // the whole of that day
	}
	return f, sf, nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

type messageRow struct {
	ID          string
	URL         string
	Received    time.Time
	From        string
	To          string
	Subject     string
	Attachments int
	Size        int
	Status      string
	Attempts    int
	LastCode    int
}

func rowOf(m store.MessageRow) messageRow {
	return messageRow{
		ID:          m.ID,
		URL:         prefix + "/messages/" + m.ID,
		Received:    m.ReceivedAt,
		From:        firstNonEmpty(m.FromEmail, m.MailFrom),
		To:          recipientsLabel(m.RcptTo),
		Subject:     m.Subject,
		Attachments: len(m.Attachments),
		Size:        m.Size,
		Status:      m.Status,
		Attempts:    m.Attempts,
		LastCode:    m.LastStatusCode,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// recipientsLabel shows the first envelope recipient and how many more there are.
func recipientsLabel(rcpts []string) string {
	switch len(rcpts) {
	case 0:
		return ""
	case 1:
		return rcpts[0]
	}
	return rcpts[0] + " +" + strconv.Itoa(len(rcpts)-1)
}

type messagesDoc struct {
	Filters  messageFilters
	Statuses []string
	Rows     []messageRow
	TableURL string
	Trigger  string
	OlderURL string
}

func (u *UI) messagesDoc(q url.Values) (*messagesDoc, error) {
	f, sf, err := parseMessageFilters(q)
	if err != nil {
		return nil, err
	}
	rows, more, err := u.Store.ListMessages(sf)
	if err != nil {
		return nil, err
	}
	doc := &messagesDoc{Filters: f, Statuses: messageStatuses}
	for _, r := range rows {
		doc.Rows = append(doc.Rows, rowOf(r))
	}

	table := f.values()
	if f.Before != "" {
		table.Set("before", f.Before)
	}
	doc.TableURL = withQuery(prefix+"/api/messages/table", table)
	if more && len(rows) > 0 {
		older := f.values()
		older.Set("before", rows[len(rows)-1].ID)
		doc.OlderURL = withQuery(prefix+"/messages", older)
	}

	// A page of finished deliveries makes no requests at all, except the
	// unfiltered first page, where new mail shows up on its own.
	switch {
	case u.Store.HasPending():
		doc.Trigger = "every 2s"
	case !f.filtered() && f.Before == "":
		doc.Trigger = "every 10s"
	}
	return doc, nil
}

func withQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// --- dashboard --------------------------------------------------------------

type dashboardDoc struct {
	PanelURL   string
	Trigger    string
	Stats      store.Stats
	SpoolBytes int64
	Attention  []messageRow
	Recent     []messageRow
}

func (u *UI) dashboardDoc() (*dashboardDoc, error) {
	stats, err := u.Store.StatsSince(u.Now().Add(-24 * time.Hour))
	if err != nil {
		return nil, err
	}
	doc := &dashboardDoc{
		PanelURL:   prefix + "/api/dashboard/panel",
		Trigger:    "every 10s",
		Stats:      stats,
		SpoolBytes: u.Store.SpoolBytes(),
	}
	if u.Store.HasPending() {
		doc.Trigger = "every 2s"
	}
	for _, status := range []string{store.StatusFailed, store.StatusDeferred} {
		rows, _, err := u.Store.ListMessages(store.MessageFilter{Status: status, Limit: 10})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			doc.Attention = append(doc.Attention, rowOf(r))
		}
	}
	recent, _, err := u.Store.ListMessages(store.MessageFilter{Limit: 20})
	if err != nil {
		return nil, err
	}
	for _, r := range recent {
		doc.Recent = append(doc.Recent, rowOf(r))
	}
	return doc, nil
}

// --- one message ------------------------------------------------------------

type attachmentView struct {
	Name string
	Type string
	Size int
	URL  string
}

type headerView struct {
	Name  string
	Value string
}

type deliveryView struct {
	store.Delivery
	Response string
	Attempts []store.Attempt
}

type deliveryError struct {
	Status  string
	Message string
	Code    int
	Excerpt string
}

type messageDoc struct {
	Message   *store.Message
	From      string
	Recipient string
	PanelURL  string
	ResendURL string
	RawURL    string
	Trigger   string
	CanResend bool

	Error       *deliveryError
	Attachments []attachmentView
	Text        string
	HTML        string
	Headers     []headerView
	Deliveries  []deliveryView
	Receipts    []store.SinkReceipt
	Payload     string
	// PayloadNote says why there is no payload, or what is wrong with it.
	PayloadNote string
}

func (u *UI) messageDoc(id string) (*messageDoc, error) {
	msg, err := u.Store.GetMessage(id)
	if err != nil {
		return nil, err
	}
	deliveries, err := u.Store.DeliveriesFor(id)
	if err != nil {
		return nil, err
	}

	doc := &messageDoc{
		Message:   msg,
		From:      firstNonEmpty(msg.FromEmail, msg.MailFrom),
		PanelURL:  prefix + "/api/messages/" + id + "/panel",
		ResendURL: prefix + "/api/messages/" + id + "/webhook/resend",
		RawURL:    prefix + "/api/messages/" + id + "/raw.eml",
		Trigger:   "message-changed from:body",
		CanResend: msg.HasRaw() && len(deliveries) > 0,
	}
	if len(msg.RcptTo) > 0 {
		doc.Recipient = msg.RcptTo[0]
	}

	ids := make([]string, 0, len(deliveries))
	for _, d := range deliveries {
		attempts, err := u.Store.AttemptsFor(d.ID)
		if err != nil {
			return nil, err
		}
		doc.Deliveries = append(doc.Deliveries, deliveryView{
			Delivery: d,
			Response: firstNonEmpty(d.ResponseExcerpt, d.LastError),
			Attempts: attempts,
		})
		ids = append(ids, d.ID)
		if d.Status == store.StatusPending {
			doc.Trigger = "every 2s, message-changed from:body"
		}
	}
	if n := len(deliveries); n > 0 {
		if last := deliveries[n-1]; last.Status == store.StatusFailed || last.Status == store.StatusDeferred {
			doc.Error = &deliveryError{Status: last.Status, Message: last.LastError, Code: last.LastStatusCode, Excerpt: last.ResponseExcerpt}
		}
	}
	if doc.Receipts, err = u.Store.SinkReceiptsFor(ids); err != nil {
		return nil, err
	}

	u.addContent(doc)
	return doc, nil
}

// addContent fills in what only the raw message can tell: bodies, headers,
// attachments and the payload. All of it is rebuilt from the stored bytes, so
// the page shows what a resend would post.
func (u *UI) addContent(doc *messageDoc) {
	msg := doc.Message
	raw, err := u.Store.ReadRaw(msg)
	if err != nil {
		return
	}
	parsed, _ := u.Engine.Parse(raw)

	doc.Text = clip(parsed.Text)
	doc.HTML = clip(parsed.HTML)
	for _, h := range parsed.Headers {
		doc.Headers = append(doc.Headers, headerView{Name: h.Name, Value: h.Value})
	}
	for i, a := range parsed.Attachments {
		doc.Attachments = append(doc.Attachments, attachmentView{
			Name: a.Name,
			Type: a.ContentType,
			Size: len(a.Data),
			URL:  prefix + "/api/messages/" + msg.ID + "/attachments/" + strconv.Itoa(i),
		})
	}

	targets := u.Engine.Targets(msg.RcptTo)
	if len(targets) == 0 {
		doc.PayloadNote = "The message has no recipient, so there is no payload."
		return
	}
	_, body, err := u.Engine.Build(parsed, delivery.Envelope(msg), targets[0])
	switch {
	case body == nil && err != nil:
		doc.PayloadNote = "The payload could not be built: " + err.Error() + "."
		return
	case err != nil:
		doc.PayloadNote = "This payload would be refused: " + err.Error() + "."
	}
	doc.Payload = clip(prettyJSON(body))
}

// clip keeps a page from carrying megabytes of one mail.
func clip(s string) string {
	if len(s) <= displayLimit {
		return s
	}
	cut := displayLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… (cut off here; download the .eml for the rest)"
}

// base64Value finds the two fields that carry file contents. They are matched
// in the text, not in a decoded document, so the key order of the payload
// survives.
var base64Value = regexp.MustCompile(`"(Content|RawEmail)"\s*:\s*"([A-Za-z0-9+/=\\]+)"`)

// prettyJSON indents a payload and leaves out the file contents, which are
// megabytes of base64 nobody can read.
func prettyJSON(body []byte) string {
	elided := base64Value.ReplaceAllFunc(body, func(match []byte) []byte {
		parts := base64Value.FindSubmatch(match)
		encoded := bytes.ReplaceAll(parts[2], []byte(`\`), nil)
		size := len(encoded) * 3 / 4
		size -= len(encoded) - len(bytes.TrimRight(encoded, "="))
		return []byte(`"` + string(parts[1]) + `":"<` + strconv.Itoa(size) + ` bytes, base64 omitted>"`)
	})
	var out bytes.Buffer
	if err := json.Indent(&out, elided, "", "  "); err != nil {
		return strings.ToValidUTF8(string(elided), "�")
	}
	return out.String()
}

// --- addresses --------------------------------------------------------------

type addressRow struct {
	store.Recipient
	// Vals is the address as the JSON htmx posts with the row's buttons.
	Vals    string
	Confirm string
}

type addressesDoc struct {
	Enabled   bool
	Status    string
	Query     string
	Statuses  []string
	Rows      []addressRow
	TableURL  string
	CheckURL  string
	ForgetURL string
}

func (u *UI) addressesDoc(q url.Values) (*addressesDoc, error) {
	doc := &addressesDoc{
		Enabled:   u.Checker.Enabled(),
		Status:    strings.TrimSpace(q.Get("status")),
		Query:     strings.TrimSpace(q.Get("q")),
		Statuses:  []string{store.RecipientKnown, store.RecipientUnknown},
		CheckURL:  prefix + "/api/addresses/check",
		ForgetURL: prefix + "/api/addresses/forget",
	}
	if doc.Status != "" && !contains(doc.Statuses, doc.Status) {
		return nil, fmt.Errorf("%w: unknown status", errBadFilter)
	}
	list, err := u.Store.ListRecipients(store.RecipientFilter{Status: doc.Status, Query: doc.Query})
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		vals, _ := json.Marshal(map[string]string{"address": r.Address})
		doc.Rows = append(doc.Rows, addressRow{
			Recipient: r,
			Vals:      string(vals),
			Confirm:   "Forget " + r.Address + "? It is checked again when mail arrives for it.",
		})
	}
	v := url.Values{}
	if doc.Status != "" {
		v.Set("status", doc.Status)
	}
	if doc.Query != "" {
		v.Set("q", doc.Query)
	}
	doc.TableURL = withQuery(prefix+"/api/addresses/table", v)
	return doc, nil
}

// --- refused ----------------------------------------------------------------

type refusedDoc struct {
	Reason   string
	Query    string
	Reasons  []string
	Rows     []store.Refusal
	TableURL string
	Trigger  string
	OlderURL string
}

func (u *UI) refusedDoc(q url.Values) (*refusedDoc, error) {
	doc := &refusedDoc{
		Reason:  strings.TrimSpace(q.Get("reason")),
		Query:   strings.TrimSpace(q.Get("q")),
		Reasons: refusalReasons,
	}
	if doc.Reason != "" && !contains(refusalReasons, doc.Reason) {
		return nil, fmt.Errorf("%w: unknown reason", errBadFilter)
	}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	rows, more, err := u.Store.ListRefusals(store.RefusalFilter{Reason: doc.Reason, Query: doc.Query, Before: before, Limit: pageSize})
	if err != nil {
		return nil, err
	}
	doc.Rows = rows

	v := url.Values{}
	if doc.Reason != "" {
		v.Set("reason", doc.Reason)
	}
	if doc.Query != "" {
		v.Set("q", doc.Query)
	}
	table := url.Values{}
	for k := range v {
		table.Set(k, v.Get(k))
	}
	if before > 0 {
		table.Set("before", strconv.FormatInt(before, 10))
	}
	doc.TableURL = withQuery(prefix+"/api/refused/table", table)
	// Refusals arrive on their own, so the unfiltered first page keeps itself fresh.
	if doc.Reason == "" && doc.Query == "" && before == 0 {
		doc.Trigger = "every 30s"
	}
	if more && len(rows) > 0 {
		v.Set("before", strconv.FormatInt(rows[len(rows)-1].ID, 10))
		doc.OlderURL = withQuery(prefix+"/refused", v)
	}
	return doc, nil
}

// --- settings ---------------------------------------------------------------

type settingRow struct {
	Key    string
	Value  string
	Set    bool
	Secret bool
}

type settingsDoc struct {
	Version       string
	DataDir       string
	DatabaseBytes int64
	SpoolBytes    int64
	RetentionDays int
	Settings      []settingRow
}

func (u *UI) settingsDoc() *settingsDoc {
	doc := &settingsDoc{
		Version:       u.Version,
		DataDir:       u.Cfg.DataDir,
		DatabaseBytes: u.Store.DatabaseBytes(),
		SpoolBytes:    u.Store.SpoolBytes(),
		RetentionDays: u.Cfg.RetentionDays,
	}
	for _, s := range u.Cfg.Settings() {
		row := settingRow{Key: s.Key, Set: s.Set, Secret: config.Secret(s.Key)}
		if !row.Secret {
			row.Value = withoutCredentials(s.Value)
		}
		doc.Settings = append(doc.Settings, row)
	}
	return doc
}

// withoutCredentials drops "user:password@" from a URL setting. The webhook
// URL is already stored without it; this covers any other URL an operator
// wrote credentials into.
func withoutCredentials(value string) string {
	if !strings.Contains(value, "://") || !strings.Contains(value, "@") {
		return value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User == nil {
		return value
	}
	parsed.User = nil
	return parsed.String()
}
