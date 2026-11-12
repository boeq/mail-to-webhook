package smtpsrv

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	netsmtp "net/smtp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boeq/mail-to-webhook/internal/config"
	"github.com/boeq/mail-to-webhook/internal/delivery"
	"github.com/boeq/mail-to-webhook/internal/limits"
	"github.com/boeq/mail-to-webhook/internal/store"
	"github.com/boeq/mail-to-webhook/internal/tlscert"
	"github.com/boeq/mail-to-webhook/internal/webhook"
)

// source is a certificate source a test fills when it chooses to.
type source struct {
	cert  atomic.Pointer[tls.Certificate]
	ready chan struct{}
	once  sync.Once
}

func newSource() *source { return &source{ready: make(chan struct{})} }

func (s *source) set(c *tls.Certificate) {
	s.cert.Store(c)
	s.once.Do(func() { close(s.ready) })
}
func (s *source) Certificate() *tls.Certificate { return s.cert.Load() }
func (s *source) Ready() <-chan struct{}        { return s.ready }
func (s *source) Status() tlscert.Status        { return tlscert.Status{Mode: tlscert.ModeACME} }

func selfSigned(t *testing.T, serial int64) (*tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "mx.example.org"},
		DNSNames:     []string{"mx.example.org"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// fixture is a running server in front of a webhook receiver.
type fixture struct {
	addr     string
	server   *Server
	served   chan error
	payloads chan map[string]any
}

func start(t *testing.T, certs tlscert.Source) *fixture {
	t.Helper()
	f := &fixture{served: make(chan error, 1), payloads: make(chan map[string]any, 8)}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var doc map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &doc)
		f.payloads <- doc
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)

	t.Setenv("WEBHOOK_URL", receiver.URL)
	t.Setenv("SMTP_DOMAIN", "mx.example.org")
	t.Setenv("ALLOWED_DOMAINS", "example.org")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	wh := webhook.New(webhook.Options{URL: cfg.WebhookURL, Timeout: 5 * time.Second, PermanentStatus: cfg.PermanentStatus})
	backend := &Backend{Cfg: cfg, Engine: delivery.New(cfg, st, wh, log), Store: st,
		Limiter: limits.New(limits.Config{}), Log: log, Now: time.Now}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = l.Addr().String()
	f.server = New(backend, certs)
	go func() { f.served <- f.server.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.server.Shutdown(ctx)
	})
	return f
}

func dial(t *testing.T, addr string) *netsmtp.Client {
	t.Helper()
	c, err := netsmtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Hello("client.test"); err != nil {
		t.Fatal(err)
	}
	return c
}

func send(t *testing.T, c *netsmtp.Client, subject string) {
	t.Helper()
	if err := c.Mail("a@sender.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("box@example.org"); err != nil {
		t.Fatal(err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "From: a@sender.test\r\nTo: box@example.org\r\nSubject: "+subject+"\r\n\r\nbody\r\n")
	if err := w.Close(); err != nil {
		t.Fatalf("the mail was not accepted: %v", err)
	}
}

func (f *fixture) tlsOf(t *testing.T, subject string) bool {
	t.Helper()
	select {
	case doc := <-f.payloads:
		if doc["Subject"] != subject {
			t.Fatalf("payload for %q, want %q", doc["Subject"], subject)
		}
		return doc["Envelope"].(map[string]any)["TLS"].(bool)
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook arrived")
		return false
	}
}

// STARTTLS is offered from the moment a certificate exists: not before, when
// a handshake could only fail, and without a restart.
func TestSTARTTLSIsOfferedOnceThereIsACertificate(t *testing.T) {
	certs := newSource()
	f := start(t, certs)

	early := dial(t, f.addr)
	if ok, _ := early.Extension("STARTTLS"); ok {
		t.Fatal("STARTTLS offered before there is a certificate")
	}

	first, firstPool := selfSigned(t, 1)
	certs.set(first)

	secure := dial(t, f.addr)
	if ok, _ := secure.Extension("STARTTLS"); !ok {
		t.Fatal("STARTTLS not offered although there is a certificate")
	}
	if err := secure.StartTLS(&tls.Config{RootCAs: firstPool, ServerName: "mx.example.org"}); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	state, _ := secure.TLSConnectionState()
	if state.Version < tls.VersionTLS12 || state.PeerCertificates[0].SerialNumber.Int64() != 1 {
		t.Errorf("version %x, serial %d", state.Version, state.PeerCertificates[0].SerialNumber)
	}
	send(t, secure, "over tls")
	if !f.tlsOf(t, "over tls") {
		t.Error("Envelope.TLS is false for a mail sent over TLS")
	}

	// The connection opened before the certificate came keeps working.
	send(t, early, "in clear text")
	if f.tlsOf(t, "in clear text") {
		t.Error("Envelope.TLS is true for a mail sent in clear text")
	}

	// A renewed certificate is served to the next connection.
	second, secondPool := selfSigned(t, 2)
	certs.set(second)
	renewed := dial(t, f.addr)
	if err := renewed.StartTLS(&tls.Config{RootCAs: secondPool, ServerName: "mx.example.org"}); err != nil {
		t.Fatalf("handshake after renewal: %v", err)
	}
	if state, _ := renewed.TLSConnectionState(); state.PeerCertificates[0].SerialNumber.Int64() != 2 {
		t.Errorf("serial %d after renewal, want 2", state.PeerCertificates[0].SerialNumber)
	}

	// A client that sends no server name and one that asks for another host
	// get the same certificate: a mail server has one.
	for _, name := range []string{"", "other.example.net"} {
		c := dial(t, f.addr)
		err := c.StartTLS(&tls.Config{ServerName: name, InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("server name %q: %v", name, err)
			continue
		}
		if state, _ := c.TLSConnectionState(); state.PeerCertificates[0].SerialNumber.Int64() != 2 {
			t.Errorf("server name %q got serial %d", name, state.PeerCertificates[0].SerialNumber)
		}
	}

	// TLS 1.1 is refused.
	old := dial(t, f.addr)
	if err := old.StartTLS(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}); err == nil {
		t.Error("a TLS 1.1 handshake succeeded")
	}
}

func TestWithoutACertificateSourceThereIsNoSTARTTLS(t *testing.T) {
	f := start(t, nil)
	c := dial(t, f.addr)
	if ok, _ := c.Extension("STARTTLS"); ok {
		t.Error("STARTTLS offered without any certificate source")
	}
	send(t, c, "plain")
	if f.tlsOf(t, "plain") {
		t.Error("Envelope.TLS is true")
	}
}

func TestShutdownEndsServe(t *testing.T) {
	for name, certs := range map[string]tlscert.Source{"without tls": nil, "with a certificate source": newSource()} {
		t.Run(name, func(t *testing.T) {
			f := start(t, certs)
			dial(t, f.addr).Quit()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := f.server.Shutdown(ctx); err != nil {
				t.Fatalf("shutdown: %v", err)
			}
			select {
			case err := <-f.served:
				if err != nil {
					t.Errorf("Serve returned %v after a shutdown, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve did not return")
			}
			if _, err := net.DialTimeout("tcp", f.addr, time.Second); err == nil {
				t.Error("the port still accepts connections")
			}
		})
	}
}
