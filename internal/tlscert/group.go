package tlscert

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Group keeps one certificate per host.
//
// One certificate with every name on it would be less to manage, and it would
// tie the hosts together: a single name that no longer resolves fails the
// order, and with it the renewal for all the others. With a certificate each,
// a host that cannot be validated loses only its own.
type Group struct {
	keepers []*ACME
	log     *slog.Logger
	now     func() time.Time

	ready chan struct{}
	once  sync.Once
}

// NewGroup prepares a certificate keeper for each host. The first host is the
// one a client gets that names no host, or one that is not in the list.
func NewGroup(hosts []string, email, directoryURL, dataDir string, log *slog.Logger) *Group {
	g := &Group{log: log, now: time.Now, ready: make(chan struct{})}
	for _, host := range hosts {
		g.keepers = append(g.keepers, NewACME(host, email, directoryURL, dataDir, log))
	}
	return g
}

// Run loads what is stored, then obtains and renews the certificates, one
// host after the other, until ctx ends. Orders are never made at the same
// time: the hosts share one account, and a CA limits what an account may do
// at once.
func (g *Group) Run(ctx context.Context) {
	for _, k := range g.keepers {
		k.Load()
	}
	g.markReady()

	due := make([]time.Time, len(g.keepers)) // the zero time is due at once
	for {
		for i, k := range g.keepers {
			if ctx.Err() != nil {
				return
			}
			if !g.now().Before(due[i]) {
				due[i] = g.now().Add(k.Step(ctx))
				g.markReady()
			}
		}
		next := due[0]
		for _, t := range due[1:] {
			if t.Before(next) {
				next = t
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(next.Sub(g.now()), time.Second)):
		}
	}
}

func (g *Group) markReady() {
	if g.Certificate() != nil {
		g.once.Do(func() { close(g.ready) })
	}
}

// Certificate returns the certificate of the first host that has one.
func (g *Group) Certificate() *tls.Certificate {
	for _, k := range g.keepers {
		if cert := k.Certificate(); cert != nil {
			return cert
		}
	}
	return nil
}

// CertificateFor returns the certificate of the named host, or nil.
func (g *Group) CertificateFor(name string) *tls.Certificate {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, k := range g.keepers {
		if k.Host == name {
			return k.Certificate()
		}
	}
	return nil
}

// Ready is closed once any host has a certificate. STARTTLS can be offered
// from then on; a host that is still waiting is served the first one.
func (g *Group) Ready() <-chan struct{} { return g.ready }

func (g *Group) Status() Status {
	st := Status{Mode: ModeACME}
	for i, k := range g.keepers {
		one := k.Status()
		if i == 0 {
			st.Host = one.Host
		}
		st.Hosts = append(st.Hosts, HostStatus{Host: one.Host, Ready: one.Ready, NotAfter: one.NotAfter, LastError: one.LastError})
		if one.Ready {
			if !st.Ready || one.NotAfter.Before(st.NotAfter) {
				st.NotAfter = one.NotAfter
			}
			st.Ready = true
		}
		if one.LastError != "" && st.LastError == "" {
			st.LastError = one.Host + ": " + one.LastError
		}
	}
	return st
}

// ServeHTTP answers the CA's http-01 requests for every host, and nothing else.
func (g *Group) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	serveChallenge(w, r, g.keepers...)
}
