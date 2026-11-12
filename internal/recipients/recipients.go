// Package recipients decides whether an address may receive mail, by keeping
// a list of addresses the consumer has confirmed and asking it about new ones.
//
// The contract is one HTTP call, documented in docs/recipient-check.md:
//
//	POST <RECIPIENT_CHECK_URL>   {"addresses": ["a@example.org"]}
//	200                          {"known": ["a@example.org"]}
package recipients

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/boeq/mail-to-webhook/internal/store"
	"github.com/boeq/mail-to-webhook/internal/webhook"
)

// EventCheck names the call in the event header.
const EventCheck = "recipient.check"

type Verdict int

const (
	// Known: the consumer has this address. Accept the mail.
	Known Verdict = iota
	// Unknown: the consumer has no such address. Refuse the mail for good.
	Unknown
	// Unavailable: the consumer could not be asked. Defer; the sender retries.
	Unavailable
)

func (v Verdict) String() string {
	switch v {
	case Known:
		return "known"
	case Unknown:
		return "unknown"
	}
	return "unavailable"
}

type Checker struct {
	URL        string
	Timeout    time.Duration
	Recheck    time.Duration // how long a known address is trusted
	UnknownTTL time.Duration // how long an unknown answer is remembered
	Store      *store.Store  // nil keeps the list in memory only
	Client     *webhook.Client
	Log        *slog.Logger
	Now        func() time.Time

	mu       sync.Mutex
	inflight map[string]*call
	memory   map[string]entry
	health   Health
}

type call struct {
	done    chan struct{}
	verdict Verdict
}

type entry struct {
	status    string
	checkedAt time.Time
}

// Health is the state of the lookup, for the status row in the UI.
type Health struct {
	Enabled   bool
	OK        bool
	LastError string
	At        time.Time
}

// Enabled is false when no check URL is configured; every address then passes.
func (c *Checker) Enabled() bool { return c != nil && c.URL != "" }

func (c *Checker) Health() Health {
	if !c.Enabled() {
		return Health{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.health
	h.Enabled = true
	if h.At.IsZero() {
		h.OK = true // nothing asked yet is not a failure
	}
	return h
}

// Check answers for one address, from the list when it can.
//
//   - A known address is trusted for Recheck. After that it is asked again, so
//     an address the consumer removed stops being accepted; if the consumer
//     cannot be reached, the old answer stands.
//   - An unknown answer is remembered for UnknownTTL only: long enough that a
//     repeated address costs one call, short enough that a newly created
//     address works within minutes.
func (c *Checker) Check(ctx context.Context, address string) Verdict {
	if !c.Enabled() {
		return Known
	}
	now := c.Now()
	if e, ok := c.lookupList(address); ok {
		age := now.Sub(e.checkedAt)
		switch {
		case e.status == store.RecipientKnown && age < c.Recheck:
			return Known
		case e.status == store.RecipientUnknown && age < c.UnknownTTL:
			return Unknown
		case e.status == store.RecipientKnown:
			if v := c.ask(ctx, address); v != Unavailable {
				return v
			}
			return Known
		}
	}
	return c.ask(ctx, address)
}

// Ask checks the address with the consumer now, whatever the list says.
func (c *Checker) Ask(ctx context.Context, address string) Verdict {
	if !c.Enabled() {
		return Known
	}
	return c.ask(ctx, address)
}

func (c *Checker) lookupList(address string) (entry, bool) {
	if c.Store != nil {
		r, err := c.Store.GetRecipient(address)
		if err == nil {
			if r == nil {
				return entry{}, false
			}
			return entry{status: r.Status, checkedAt: r.CheckedAt}, true
		}
		c.Log.Error("could not read the address list", "err", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.memory[address]
	return e, ok
}

func (c *Checker) remember(address, status string, at time.Time) {
	if c.Store != nil {
		if err := c.Store.PutRecipient(address, status, at); err == nil {
			return
		} else {
			c.Log.Error("could not save to the address list", "err", err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.memory == nil {
		c.memory = map[string]entry{}
	}
	c.memory[address] = entry{status: status, checkedAt: at}
}

// ask makes the call. Simultaneous questions about one address share it.
func (c *Checker) ask(ctx context.Context, address string) Verdict {
	c.mu.Lock()
	if running, ok := c.inflight[address]; ok {
		c.mu.Unlock()
		select {
		case <-running.done:
			return running.verdict
		case <-ctx.Done():
			return Unavailable
		}
	}
	if c.inflight == nil {
		c.inflight = map[string]*call{}
	}
	mine := &call{done: make(chan struct{})}
	c.inflight[address] = mine
	c.mu.Unlock()

	verdict, err := c.request(ctx, address)

	c.mu.Lock()
	delete(c.inflight, address)
	c.health = Health{OK: err == nil, At: c.Now()}
	if err != nil {
		c.health.LastError = err.Error()
	}
	c.mu.Unlock()

	if err != nil {
		c.Log.Warn("recipient check failed", "err", err)
	} else {
		status := store.RecipientKnown
		if verdict == Unknown {
			status = store.RecipientUnknown
		}
		c.remember(address, status, c.Now())
	}
	mine.verdict = verdict
	close(mine.done)
	return verdict
}

func (c *Checker) request(ctx context.Context, address string) (Verdict, error) {
	body, err := json.Marshal(map[string][]string{"addresses": {address}})
	if err != nil {
		return Unavailable, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	status, response, err := c.Client.PostTo(ctx, c.URL, EventCheck, body)
	if err != nil {
		return Unavailable, fmt.Errorf("the check endpoint could not be reached: %w", err)
	}
	if status != 200 {
		return Unavailable, fmt.Errorf("the check endpoint answered %d", status)
	}
	var answer struct {
		Known []string `json:"known"`
	}
	if err := json.Unmarshal(response, &answer); err != nil {
		return Unavailable, fmt.Errorf("the check endpoint sent an unreadable answer: %w", err)
	}
	for _, known := range answer.Known {
		if known == address {
			return Known, nil
		}
	}
	return Unknown, nil
}
