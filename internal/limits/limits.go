// Package limits bounds what one sending address, and all of them together,
// can make the server do. A limit of zero is switched off.
package limits

import (
	"net"
	"sync"
	"time"
)

// badWindow is the period over which unknown recipients are counted.
const badWindow = 10 * time.Minute

// Refusal reasons, also used as the reason of a recorded refusal.
const (
	ReasonConnectionLimit = "connection_limit"
	ReasonBlocked         = "blocked"
)

type Config struct {
	MaxConnections         int
	MaxConnectionsPerIP    int
	MaxConnectionsPerIPMin int
	BlockAfterBadRcpts     int
	BlockDuration          time.Duration
}

type Limiter struct {
	cfg Config
	now func() time.Time

	mu    sync.Mutex
	total int
	ips   map[string]*ipState
	calls int
}

type ipState struct {
	open         int
	connections  []time.Time // within the last minute
	bad          []time.Time // unknown recipients within badWindow
	blockedUntil time.Time
}

func New(cfg Config) *Limiter {
	return &Limiter{cfg: cfg, now: time.Now, ips: map[string]*ipState{}}
}

// Acquire admits a connection. It returns a release function, or the reason
// the connection is refused.
func (l *Limiter) Acquire(ip string) (release func(), reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)

	st := l.state(ip)
	st.connections = since(st.connections, now.Add(-time.Minute))

	switch {
	case now.Before(st.blockedUntil):
		return nil, ReasonBlocked
	case l.cfg.MaxConnections > 0 && l.total >= l.cfg.MaxConnections:
		return nil, ReasonConnectionLimit
	case l.cfg.MaxConnectionsPerIP > 0 && st.open >= l.cfg.MaxConnectionsPerIP:
		return nil, ReasonConnectionLimit
	case l.cfg.MaxConnectionsPerIPMin > 0 && len(st.connections) >= l.cfg.MaxConnectionsPerIPMin:
		return nil, ReasonConnectionLimit
	}

	st.open++
	st.connections = append(st.connections, now)
	l.total++

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			if st := l.ips[ip]; st != nil {
				st.open--
			}
		})
	}, ""
}

// BadRecipient counts a recipient that turned out not to exist, and reports
// whether the address is now blocked. An address that keeps guessing gets no
// further connections for a while, which also caps the lookups it can cause.
func (l *Limiter) BadRecipient(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.state(ip)
	st.bad = append(since(st.bad, now.Add(-badWindow)), now)
	if l.cfg.BlockAfterBadRcpts > 0 && len(st.bad) >= l.cfg.BlockAfterBadRcpts {
		st.blockedUntil = now.Add(l.cfg.BlockDuration)
		st.bad = nil
		return true
	}
	return false
}

func (l *Limiter) state(ip string) *ipState {
	st := l.ips[ip]
	if st == nil {
		st = &ipState{}
		l.ips[ip] = st
	}
	return st
}

// sweep forgets addresses with nothing left to remember, every so often.
func (l *Limiter) sweep(now time.Time) {
	l.calls++
	if l.calls%256 != 0 {
		return
	}
	for ip, st := range l.ips {
		st.connections = since(st.connections, now.Add(-time.Minute))
		st.bad = since(st.bad, now.Add(-badWindow))
		if st.open == 0 && len(st.connections) == 0 && len(st.bad) == 0 && !now.Before(st.blockedUntil) {
			delete(l.ips, ip)
		}
	}
}

func since(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	return times[i:]
}

// Listener refuses connections over the limits before any SMTP is spoken.
type Listener struct {
	net.Listener
	Limiter *Limiter
	// OnRefuse is told about each refused connection.
	OnRefuse func(ip, reason string)
}

func (l *Listener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := HostOf(c.RemoteAddr())
		release, reason := l.Limiter.Acquire(ip)
		if reason == "" {
			return &conn{Conn: c, release: release}, nil
		}
		if l.OnRefuse != nil {
			l.OnRefuse(ip, reason)
		}
		go refuse(c)
	}
}

func refuse(c net.Conn) {
	defer c.Close()
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write([]byte("421 4.7.0 Too many connections, try again later\r\n"))
}

type conn struct {
	net.Conn
	release func()
}

func (c *conn) Close() error {
	c.release()
	return c.Conn.Close()
}

// HostOf is the IP of a network address, without the port.
func HostOf(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}
