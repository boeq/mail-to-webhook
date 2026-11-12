package ui

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/boeq/mail-to-webhook/internal/store"
	"github.com/boeq/mail-to-webhook/internal/webhook"
)

const (
	// sinkReadLimit is the largest webhook the sink takes: a 25 MiB mail
	// posts roughly 34 MiB of JSON.
	sinkReadLimit = 64 << 20
	// sinkStoreLimit is how much of a webhook is kept to look at.
	sinkStoreLimit = 64 << 10
	// sinkMaxSkew is how far a signature's timestamp may be off.
	sinkMaxSkew = 5 * time.Minute
)

// sink is a webhook receiver of our own. Point WEBHOOK_URL at it to try the
// whole path, signature included, before the real consumer is connected.
//
// It answers anyone, so it keeps nothing it cannot tie to a delivery it knows:
// a receipt is stored only when the delivery header names a row in the history.
func (u *UI) sink(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sinkReadLimit))
	if err != nil {
		u.fail(w, http.StatusRequestEntityTooLarge, "too_large", "the webhook body is larger than 64 MiB")
		return
	}
	deliveryID := r.Header.Get(webhook.DeliveryHeader)
	if deliveryID == "" {
		u.fail(w, http.StatusNotFound, "not_found", "the request names no delivery")
		return
	}
	if _, err := u.Store.GetDelivery(deliveryID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			u.fail(w, http.StatusNotFound, "not_found", "unknown delivery")
			return
		}
		u.Log.Error("webhook sink could not read the history", "err", err)
		u.fail(w, http.StatusInternalServerError, "internal", "the history could not be read")
		return
	}

	status := webhook.Verify(u.Cfg.WebhookSecret, r.Header.Get(webhook.TimestampHeader),
		r.Header.Get(webhook.SignatureHeader), body, u.Now(), sinkMaxSkew)
	receipt := store.SinkReceipt{
		ReceivedAt:      u.Now(),
		DeliveryID:      deliveryID,
		Event:           cut(strings.ToValidUTF8(r.Header.Get(webhook.EventHeader), "�"), 100),
		SignatureStatus: status,
		Body:            cut(prettyJSON(body), sinkStoreLimit),
	}
	if err := u.Store.AddSinkReceipt(receipt); err != nil {
		u.Log.Error("webhook sink could not store the receipt", "delivery", deliveryID, "err", err)
		u.fail(w, http.StatusInternalServerError, "internal", "the receipt could not be stored")
		return
	}
	// One line, never the body: it is somebody's mail.
	u.Log.Info("webhook sink received a delivery", "delivery", deliveryID, "signature", status, "bytes", len(body))
	w.WriteHeader(http.StatusNoContent)
}

// cut shortens s to at most limit bytes without splitting a character.
func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}
