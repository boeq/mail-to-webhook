package tlscert

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// group returns a Group whose orders are answered by issue, per host.
func group(t *testing.T, hosts []string, issue func(host string) (*tls.Certificate, error)) *Group {
	t.Helper()
	g := NewGroup(hosts, "ops@example.org", "https://ca.test/directory", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.now = func() time.Time { return t0 }
	for _, k := range g.keepers {
		k.Now = func() time.Time { return t0 }
		k.issue = func(context.Context) (*tls.Certificate, error) { return issue(k.Host) }
	}
	return g
}

// step does what one pass of Run does, without the waiting.
func step(g *Group) {
	for _, k := range g.keepers {
		k.Step(context.Background())
	}
	g.markReady()
}

func served(t *testing.T, g *Group, serverName string) string {
	t.Helper()
	cert, err := TLSConfig(g).GetCertificate(&tls.ClientHelloInfo{ServerName: serverName})
	if err != nil {
		t.Fatalf("server name %q: %v", serverName, err)
	}
	return cert.Leaf.DNSNames[0]
}

func TestEachHostGetsItsOwnCertificate(t *testing.T) {
	hosts := []string{"mx.example.org", "mx.example.net"}
	g := group(t, hosts, func(host string) (*tls.Certificate, error) {
		return selfSigned(t, host, t0, 90*24*time.Hour), nil
	})

	if g.Certificate() != nil || isReady(g) {
		t.Fatal("ready before any certificate exists")
	}
	if _, err := TLSConfig(g).GetCertificate(&tls.ClientHelloInfo{ServerName: "mx.example.org"}); err == nil {
		t.Error("a certificate was served before there was one")
	}

	step(g)
	if !isReady(g) {
		t.Fatal("not ready although both hosts have a certificate")
	}

	// A client gets the certificate for the name it asks for. One that names
	// no host, or a host that is not ours, gets the first.
	for asked, want := range map[string]string{
		"mx.example.org":   "mx.example.org",
		"mx.example.net":   "mx.example.net",
		"MX.Example.NET.":  "mx.example.net",
		"":                 "mx.example.org",
		"other.example.io": "mx.example.org",
	} {
		if got := served(t, g, asked); got != want {
			t.Errorf("server name %q got the certificate for %s, want %s", asked, got, want)
		}
	}

	st := g.Status()
	if st.Mode != ModeACME || st.Host != "mx.example.org" || !st.Ready || len(st.Hosts) != 2 || !st.Hosts[1].Ready || st.LastError != "" {
		t.Errorf("status = %+v", st)
	}

	// The hosts share the account and keep their certificates apart.
	a, b := g.keepers[0], g.keepers[1]
	if a.AccountKey != b.AccountKey || a.Dir == b.Dir {
		t.Errorf("account keys %q %q, folders %q %q", a.AccountKey, b.AccountKey, a.Dir, b.Dir)
	}
	if !strings.HasSuffix(a.Dir, "mx.example.org") || !strings.HasSuffix(b.Dir, "mx.example.net") {
		t.Errorf("folders %q %q", a.Dir, b.Dir)
	}
}

// A host the CA cannot validate loses its own certificate, and nobody else's.
func TestOneHostThatFailsDoesNotStopTheOthers(t *testing.T) {
	g := group(t, []string{"gone.example.org", "mx.example.net"}, func(host string) (*tls.Certificate, error) {
		if host == "gone.example.org" {
			return nil, errors.New("the name does not resolve")
		}
		return selfSigned(t, host, t0, 90*24*time.Hour), nil
	})
	step(g)

	if !isReady(g) {
		t.Fatal("STARTTLS waits for a host that fails")
	}
	// The first host has no certificate, so its clients get the one there is.
	for _, asked := range []string{"", "gone.example.org", "mx.example.net"} {
		if got := served(t, g, asked); got != "mx.example.net" {
			t.Errorf("server name %q got %s", asked, got)
		}
	}

	st := g.Status()
	if !st.Ready || st.Hosts[0].Ready || !st.Hosts[1].Ready {
		t.Errorf("status = %+v", st)
	}
	if !strings.HasPrefix(st.LastError, "gone.example.org: ") || !strings.Contains(st.LastError, "does not resolve") {
		t.Errorf("last error = %q", st.LastError)
	}
	if !st.NotAfter.Equal(g.keepers[1].Certificate().Leaf.NotAfter) {
		t.Errorf("expiry = %s, want that of the one certificate there is", st.NotAfter)
	}
}

func TestGroupAnswersTheChallengeOfEveryHost(t *testing.T) {
	g := group(t, []string{"mx.example.org", "mx.example.net"}, func(string) (*tls.Certificate, error) {
		return nil, errors.New("unused")
	})
	g.keepers[1].challenges["/.well-known/acme-challenge/token-b"] = "token-b.thumbprint"
	srv := httptest.NewServer(g)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/.well-known/acme-challenge/token-b")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "token-b.thumbprint" {
		t.Errorf("challenge of the second host: %d %q", resp.StatusCode, body)
	}
	if resp, _ := http.Get(srv.URL + "/.well-known/acme-challenge/unknown"); resp.StatusCode != 404 {
		t.Errorf("unknown token: %d", resp.StatusCode)
	}
}

// The hosts share one account at the CA, so their orders never overlap.
func TestRunOrdersOneHostAtATime(t *testing.T) {
	var running, overlap, orders atomic.Int32
	hosts := []string{"a.example.org", "b.example.org", "c.example.org"}
	g := group(t, hosts, func(host string) (*tls.Certificate, error) {
		if running.Add(1) > 1 {
			overlap.Add(1)
		}
		time.Sleep(20 * time.Millisecond)
		running.Add(-1)
		orders.Add(1)
		return selfSigned(t, host, t0, 90*24*time.Hour), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()

	select {
	case <-g.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("never became ready")
	}
	deadline := time.Now().Add(5 * time.Second)
	for orders.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}

	if orders.Load() != 3 || overlap.Load() != 0 {
		t.Errorf("%d orders, %d of them overlapping", orders.Load(), overlap.Load())
	}
	for _, host := range hosts {
		if g.CertificateFor(host) == nil {
			t.Errorf("%s has no certificate", host)
		}
	}
}
