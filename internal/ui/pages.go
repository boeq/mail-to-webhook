package ui

import (
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/boeq/mail-to-webhook/internal/delivery"
	"github.com/boeq/mail-to-webhook/internal/recipients"
	"github.com/boeq/mail-to-webhook/internal/store"
)

// pageError answers a page or fragment request that could not be served.
func (u *UI) pageError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		u.notFound(w, r)
	case errors.Is(err, errBadFilter):
		http.Error(w, strings.TrimPrefix(err.Error(), errBadFilter.Error()+": "), http.StatusBadRequest)
	default:
		u.Log.Error("could not read the history", "path", r.URL.Path, "err", err)
		http.Error(w, "The history could not be read.", http.StatusInternalServerError)
	}
}

func (u *UI) dashboardPage(w http.ResponseWriter, r *http.Request) {
	doc, err := u.dashboardDoc()
	if err != nil {
		u.pageError(w, r, err)
		return
	}
	u.renderPage(w, r, http.StatusOK, "dashboard", doc)
}

func (u *UI) dashboardFragment(w http.ResponseWriter, r *http.Request) {
	doc, err := u.dashboardDoc()
	if err != nil {
		u.pageError(w, r, err)
		return
	}
	u.renderFragment(w, "_dashboard", doc)
}

func (u *UI) messagesPage(w http.ResponseWriter, r *http.Request) {
	doc, err := u.messagesDoc(r.URL.Query())
	if err != nil {
		u.pageError(w, r, err)
		return
	}
	u.renderPage(w, r, http.StatusOK, "messages", doc)
}

func (u *UI) messagesFragment(w http.ResponseWriter, r *http.Request) {
	doc, err := u.messagesDoc(r.URL.Query())
	if err != nil {
		u.pageError(w, r, err)
		return
	}
	u.renderFragment(w, "_messages", doc)
}

func (u *UI) messagePage(w http.ResponseWriter, r *http.Request) {
	doc, err := u.messageDoc(r.PathValue("id"))
	if err != nil {
		u.pageError(w, r, err)
		return
	}
	u.renderPage(w, r, http.StatusOK, "message", doc)
}

func (u *UI) messageFragment(w http.ResponseWriter, r *http.Request) {
	doc, err := u.messageDoc(r.PathValue("id"))
	if err != nil {
		u.pageError(w, r, err)
		return
	}
	u.renderFragment(w, "_message", doc)
}

func (u *UI) addressesPage(w http.ResponseWriter, r *http.Request) {
	doc, err := u.addressesDoc(r.URL.Query())
	if err != nil {
		u.pageError(w, r, err)
		return
	}
	u.renderPage(w, r, http.StatusOK, "addresses", doc)
}

func (u *UI) addressesFragment(w http.ResponseWriter, r *http.Request) {
	doc, err := u.addressesDoc(r.URL.Query())
	if err != nil {
		u.pageError(w, r, err)
		return
	}
	u.renderFragment(w, "_addresses", doc)
}

func (u *UI) refusedPage(w http.ResponseWriter, r *http.Request) {
	doc, err := u.refusedDoc(r.URL.Query())
	if err != nil {
		u.pageError(w, r, err)
		return
	}
	u.renderPage(w, r, http.StatusOK, "refused", doc)
}

func (u *UI) refusedFragment(w http.ResponseWriter, r *http.Request) {
	doc, err := u.refusedDoc(r.URL.Query())
	if err != nil {
		u.pageError(w, r, err)
		return
	}
	u.renderFragment(w, "_refused", doc)
}

func (u *UI) settingsPage(w http.ResponseWriter, r *http.Request) {
	u.renderPage(w, r, http.StatusOK, "settings", u.settingsDoc())
}

// --- downloads --------------------------------------------------------------

func (u *UI) rawDownload(w http.ResponseWriter, r *http.Request) {
	msg, err := u.Store.GetMessage(r.PathValue("id"))
	if err != nil {
		u.downloadError(w, err)
		return
	}
	raw, err := u.Store.ReadRaw(msg)
	if err != nil {
		u.downloadError(w, err)
		return
	}
	sendFile(w, "message/rfc822", msg.ID+".eml", raw)
}

func (u *UI) attachmentDownload(w http.ResponseWriter, r *http.Request) {
	msg, err := u.Store.GetMessage(r.PathValue("id"))
	if err != nil {
		u.downloadError(w, err)
		return
	}
	raw, err := u.Store.ReadRaw(msg)
	if err != nil {
		u.downloadError(w, err)
		return
	}
	parsed, _ := u.Engine.Parse(raw)
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 || n >= len(parsed.Attachments) {
		u.fail(w, http.StatusNotFound, "not_found", "this message has no such attachment")
		return
	}
	// Always a download of unnamed bytes: whatever the mail called the file,
	// the browser is never asked to show it.
	sendFile(w, "application/octet-stream", downloadName(parsed.Attachments[n].Name, n), parsed.Attachments[n].Data)
}

func (u *UI) downloadError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		u.fail(w, http.StatusNotFound, "not_found", "no such message, or its raw content is no longer stored")
		return
	}
	u.Log.Error("could not read the stored message", "err", err)
	u.fail(w, http.StatusInternalServerError, "internal", "the stored message could not be read")
}

func sendFile(w http.ResponseWriter, contentType, name string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// downloadName keeps a file name to one path element without control
// characters; the header encoding is left to mime.FormatMediaType.
func downloadName(name string, n int) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7F:
			return -1
		case r == '/' || r == '\\':
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(strings.TrimSpace(name), ".")
	if name == "" {
		return "attachment-" + strconv.Itoa(n)
	}
	return name
}

// --- actions ----------------------------------------------------------------

func (u *UI) resend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	deliveries, err := u.Engine.Resend(id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		u.fail(w, http.StatusNotFound, "not_found", "no such message")
		return
	case errors.Is(err, delivery.ErrNoRaw):
		u.fail(w, http.StatusConflict, "conflict", "the raw message is no longer stored, so it cannot be sent again")
		return
	case err != nil:
		u.Log.Error("could not queue the re-send", "message", id, "err", err)
		u.fail(w, http.StatusInternalServerError, "internal", "the re-send could not be queued")
		return
	}
	queued := make([]map[string]string, 0, len(deliveries))
	for _, d := range deliveries {
		queued = append(queued, map[string]string{"id": d.ID, "recipient": d.Recipient})
	}
	w.Header().Set("HX-Trigger", "message-changed")
	u.writeJSON(w, http.StatusOK, map[string]any{
		"id":         id,
		"deliveries": queued,
		"status":     store.StatusPending,
		"message":    "Re-send queued.",
	})
}

func (u *UI) addressCheck(w http.ResponseWriter, r *http.Request) {
	address := strings.TrimSpace(r.FormValue("address"))
	if address == "" {
		u.fail(w, http.StatusUnprocessableEntity, "validation_error", "give the address to check")
		return
	}
	if !u.Checker.Enabled() {
		u.fail(w, http.StatusConflict, "conflict", "the recipient check is off; set RECIPIENT_CHECK_URL to use it")
		return
	}
	verdict := u.Checker.Ask(r.Context(), address)
	if verdict == recipients.Unavailable {
		u.fail(w, http.StatusServiceUnavailable, "unavailable", "the check endpoint could not be asked; the list is unchanged")
		return
	}
	w.Header().Set("HX-Trigger", "address-changed")
	u.writeJSON(w, http.StatusOK, map[string]any{
		"address": address,
		"status":  verdict.String(),
		"message": address + " is " + verdict.String() + ".",
	})
}

func (u *UI) addressForget(w http.ResponseWriter, r *http.Request) {
	address := strings.TrimSpace(r.FormValue("address"))
	if address == "" {
		u.fail(w, http.StatusUnprocessableEntity, "validation_error", "give the address to forget")
		return
	}
	if err := u.Store.ForgetRecipient(address); err != nil {
		u.Log.Error("could not forget the address", "err", err)
		u.fail(w, http.StatusInternalServerError, "internal", "the address could not be removed from the list")
		return
	}
	w.Header().Set("HX-Trigger", "address-changed")
	u.writeJSON(w, http.StatusOK, map[string]any{
		"address": address,
		"message": "Forgotten. " + address + " is checked again when mail arrives for it.",
	})
}
