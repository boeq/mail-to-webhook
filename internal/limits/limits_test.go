package limits

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

type clock struct{ now time.Time }

func limiter(cfg Config) (*Limiter, *clock) {
	c := &clock{now: time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC)}
	l := New(cfg)
	l.now = func() time.Time { return c.now }
	return l, c
}

func TestConnectionsAtOnce(t *testing.T) {
	l, _ := limiter(Config{MaxConnections: 3, MaxConnectionsPerIP: 2})

	a1, reason := l.Acquire("192.0.2.1")
	if reason != "" {
		t.Fatal(reason)
	}
	_, _ = l.Acquire("192.0.2.1")
	if _, reason := l.Acquire("192.0.2.1"); reason != ReasonConnectionLimit {
		t.Errorf("third connection from one address: %q", reason)
	}
	if _, reason := l.Acquire("192.0.2.2"); reason != "" {
		t.Errorf("another address is unaffected: %q", reason)
	}
	if _, reason := l.Acquire("192.0.2.3"); reason != ReasonConnectionLimit {
		t.Errorf("fourth connection overall: %q", reason)
	}

	a1()
	a1() // releasing twice must not free two slots
	if _, reason := l.Acquire("192.0.2.3"); reason != "" {
		t.Errorf("after a release: %q", reason)
	}
	if _, reason := l.Acquire("192.0.2.4"); reason != ReasonConnectionLimit {
		t.Errorf("a double release freed a second slot: %q", reason)
	}
}

func TestConnectionsPerMinute(t *testing.T) {
	l, c := limiter(Config{MaxConnectionsPerIPMin: 2})
	for i := 0; i < 2; i++ {
		release, reason := l.Acquire("192.0.2.1")
		if reason != "" {
			t.Fatal(reason)
		}
		release()
	}
	if _, reason := l.Acquire("192.0.2.1"); reason != ReasonConnectionLimit {
		t.Errorf("third connection within the minute: %q", reason)
	}
	c.now = c.now.Add(61 * time.Second)
	if _, reason := l.Acquire("192.0.2.1"); reason != "" {
		t.Errorf("a minute later: %q", reason)
	}
}

func TestAnAddressThatKeepsGuessingIsBlocked(t *testing.T) {
	l, c := limiter(Config{BlockAfterBadRcpts: 3, BlockDuration: 15 * time.Minute})

	if l.BadRecipient("192.0.2.1") || l.BadRecipient("192.0.2.1") {
		t.Fatal("blocked too early")
	}
	if l.BadRecipient("192.0.2.9") {
		t.Fatal("another address was counted")
	}
	if !l.BadRecipient("192.0.2.1") {
		t.Fatal("not blocked at the limit")
	}
	if _, reason := l.Acquire("192.0.2.1"); reason != ReasonBlocked {
		t.Errorf("while blocked: %q", reason)
	}
	if _, reason := l.Acquire("192.0.2.9"); reason != "" {
		t.Errorf("another address: %q", reason)
	}
	c.now = c.now.Add(16 * time.Minute)
	if _, reason := l.Acquire("192.0.2.1"); reason != "" {
		t.Errorf("after the block: %q", reason)
	}

	// Unknown recipients spread over more than the window do not add up.
	l, c = limiter(Config{BlockAfterBadRcpts: 3, BlockDuration: time.Minute})
	for i := 0; i < 5; i++ {
		if l.BadRecipient("192.0.2.1") {
			t.Fatal("blocked although the attempts were far apart")
		}
		c.now = c.now.Add(6 * time.Minute)
	}
}

func TestZeroSwitchesALimitOff(t *testing.T) {
	l, _ := limiter(Config{})
	for i := 0; i < 500; i++ {
		if _, reason := l.Acquire("192.0.2.1"); reason != "" {
			t.Fatalf("connection %d refused: %q", i, reason)
		}
		if l.BadRecipient("192.0.2.1") {
			t.Fatalf("blocked after %d unknown recipients", i)
		}
	}
}

func TestListenerRefusesBeforeAnySMTP(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()

	refused := make(chan string, 1)
	l := &Listener{Listener: inner, Limiter: New(Config{MaxConnectionsPerIP: 1}),
		OnRefuse: func(_, reason string) { refused <- reason }}
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()

	first, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	server := <-accepted

	second, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, _ := bufio.NewReader(second).ReadString('\n')
	if !strings.HasPrefix(line, "421 ") {
		t.Errorf("second connection got %q, want a 421", line)
	}
	if reason := <-refused; reason != ReasonConnectionLimit {
		t.Errorf("reason = %q", reason)
	}

	// Closing the first connection frees its slot.
	server.Close()
	third, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(5 * time.Second):
		t.Error("the slot was not released when the connection closed")
	}
}
