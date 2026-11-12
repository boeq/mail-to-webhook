package recipients

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boeq/mail-to-webhook/internal/store"
	"github.com/boeq/mail-to-webhook/internal/webhook"
)

// consumer is a stand-in for the endpoint that knows the addresses.
type consumer struct {
	mu     sync.Mutex
	known  map[string]bool
	down   bool
	calls  int32
	last   *http.Request
	server *httptest.Server
	gate   chan struct{} // when set, an answer waits for it
}

func newConsumer(t *testing.T, known ...string) *consumer {
	c := &consumer{known: map[string]bool{}}
	for _, k := range known {
		c.known[k] = true
	}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&c.calls, 1)
		if c.gate != nil {
			<-c.gate
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.last = r
		if c.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var in struct {
			Addresses []string `json:"addresses"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &in)
		out := []string{}
		for _, a := range in.Addresses {
			if c.known[a] {
				out = append(out, a)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string][]string{"known": out})
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *consumer) set(address string, known, down bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.known[address] = known
	c.down = down
}

type clock struct{ now time.Time }

func checker(t *testing.T, c *consumer, st *store.Store) (*Checker, *clock) {
	t.Helper()
	clk := &clock{now: time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC)}
	return &Checker{
		URL:        c.server.URL,
		Timeout:    2 * time.Second,
		Recheck:    24 * time.Hour,
		UnknownTTL: 5 * time.Minute,
		Store:      st,
		Client:     webhook.New(webhook.Options{URL: "http://unused.test", Timeout: 2 * time.Second, AuthUser: "hook", AuthPass: "token", Secret: "s3cret"}),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        func() time.Time { return clk.now },
	}, clk
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

const box = "AbC123.purchase@inbound.example.org"

func TestAKnownAddressIsAskedOnce(t *testing.T) {
	for name, st := range map[string]*store.Store{"with the list on disk": openStore(t), "with the list in memory": nil} {
		t.Run(name, func(t *testing.T) {
			c := newConsumer(t, box)
			chk, clk := checker(t, c, st)
			ctx := context.Background()

			if v := chk.Check(ctx, box); v != Known {
				t.Fatalf("first check = %s", v)
			}
			clk.now = clk.now.Add(23 * time.Hour)
			if v := chk.Check(ctx, box); v != Known {
				t.Fatalf("second check = %s", v)
			}
			if c.calls != 1 {
				t.Errorf("%d calls, want 1: a listed address needs no call", c.calls)
			}

			user, pass, _ := c.last.BasicAuth()
			if user != "hook" || pass != "token" || c.last.Header.Get(webhook.EventHeader) != EventCheck ||
				c.last.Header.Get(webhook.SignatureHeader) == "" {
				t.Errorf("the call must carry the webhook's credentials and signature: %v", c.last.Header)
			}

			// After a day it is asked again, so a removed address stops working.
			clk.now = clk.now.Add(2 * time.Hour)
			c.set(box, false, false)
			if v := chk.Check(ctx, box); v != Unknown {
				t.Errorf("after removal = %s, want unknown", v)
			}
		})
	}
}

func TestAnUnknownAnswerIsRememberedBriefly(t *testing.T) {
	c := newConsumer(t)
	chk, clk := checker(t, c, openStore(t))
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if v := chk.Check(ctx, box); v != Unknown {
			t.Fatalf("check %d = %s", i, v)
		}
	}
	if c.calls != 1 {
		t.Errorf("%d calls, want 1: a repeated guess must not reach the consumer again", c.calls)
	}

	// The address is created a moment later; it works once the answer has expired.
	c.set(box, true, false)
	clk.now = clk.now.Add(4 * time.Minute)
	if v := chk.Check(ctx, box); v != Unknown {
		t.Errorf("inside the TTL = %s", v)
	}
	clk.now = clk.now.Add(2 * time.Minute)
	if v := chk.Check(ctx, box); v != Known {
		t.Errorf("after the TTL = %s", v)
	}

	// "Check again" in the UI does not wait for the TTL.
	c.set("new@inbound.example.org", false, false)
	chk.Check(ctx, "new@inbound.example.org")
	c.set("new@inbound.example.org", true, false)
	if v := chk.Ask(ctx, "new@inbound.example.org"); v != Known {
		t.Errorf("Ask = %s", v)
	}
}

func TestWhenTheConsumerCannotBeAsked(t *testing.T) {
	c := newConsumer(t, box)
	chk, clk := checker(t, c, openStore(t))
	ctx := context.Background()

	chk.Check(ctx, box)
	if h := chk.Health(); !h.Enabled || !h.OK {
		t.Errorf("health after a good call = %+v", h)
	}
	c.set(box, true, true)

	// A new address cannot be judged: defer, do not refuse.
	if v := chk.Check(ctx, "new@inbound.example.org"); v != Unavailable {
		t.Errorf("new address = %s, want unavailable", v)
	}
	if h := chk.Health(); h.OK || h.LastError == "" {
		t.Errorf("health after a failed call = %+v", h)
	}
	if r, _ := chk.Store.GetRecipient("new@inbound.example.org"); r != nil {
		t.Error("a failed call must not be remembered as an answer")
	}

	// A listed address keeps working, even when its recheck is due.
	clk.now = clk.now.Add(48 * time.Hour)
	if v := chk.Check(ctx, box); v != Known {
		t.Errorf("known address during an outage = %s, want known", v)
	}
}

func TestAnAnswerThatIsNotTheContract(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"not json":       func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "<html>login</html>") },
		"a redirect":     func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/login", http.StatusFound) },
		"unauthorized":   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		"too many calls": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
	} {
		srv := httptest.NewServer(handler)
		c := &consumer{server: srv}
		chk, _ := checker(t, c, nil)
		if v := chk.Check(context.Background(), box); v != Unavailable {
			t.Errorf("%s: verdict = %s, want unavailable", name, v)
		}
		srv.Close()
	}
}

func TestExactAddressOnly(t *testing.T) {
	c := newConsumer(t, box)
	chk, _ := checker(t, c, openStore(t))
	if v := chk.Check(context.Background(), "abc123.purchase@inbound.example.org"); v != Unknown {
		t.Errorf("another case of the local part = %s, want unknown", v)
	}
}

func TestSwitchedOff(t *testing.T) {
	var none *Checker
	if none.Enabled() || none.Check(context.Background(), box) != Known || none.Health().Enabled {
		t.Error("a nil checker must accept everything")
	}
	c := newConsumer(t)
	chk, _ := checker(t, c, nil)
	chk.URL = ""
	if chk.Check(context.Background(), box) != Known || chk.Ask(context.Background(), box) != Known || c.calls != 0 {
		t.Errorf("without a URL every address passes and nothing is called (%d calls)", c.calls)
	}
}

func TestSimultaneousQuestionsShareOneCall(t *testing.T) {
	c := newConsumer(t, box)
	c.gate = make(chan struct{})
	chk, _ := checker(t, c, openStore(t))

	var wg sync.WaitGroup
	verdicts := make([]Verdict, 8)
	for i := range verdicts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			verdicts[i] = chk.Check(context.Background(), box)
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(c.gate)
	wg.Wait()

	for i, v := range verdicts {
		if v != Known {
			t.Errorf("caller %d got %s", i, v)
		}
	}
	if c.calls != 1 {
		t.Errorf("%d calls for one address asked at once, want 1", c.calls)
	}
}
