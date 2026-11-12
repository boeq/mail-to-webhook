package tlscert

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// selfSigned makes a certificate for host, valid from notBefore for lifetime.
func selfSigned(t *testing.T, host string, notBefore time.Time, lifetime time.Duration) *tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    notBefore,
		NotAfter:     notBefore.Add(lifetime),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	if err := setLeaf(cert); err != nil {
		t.Fatal(err)
	}
	return cert
}

type clock struct{ now time.Time }

// keeper returns an ACME source whose orders are answered by issue.
func keeper(t *testing.T, dir string, issue func() (*tls.Certificate, error)) (*ACME, *clock, *int) {
	t.Helper()
	clk := &clock{now: t0}
	calls := 0
	a := NewACME("mx.example.org", "ops@example.org", "https://ca.test/directory", dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.Now = func() time.Time { return clk.now }
	a.issue = func(context.Context) (*tls.Certificate, error) {
		calls++
		return issue()
	}
	return a, clk, &calls
}

func isReady(src Source) bool {
	select {
	case <-src.Ready():
		return true
	default:
		return false
	}
}

func TestACMEObtainsStoresAndRenews(t *testing.T) {
	dir := t.TempDir()
	next := selfSigned(t, "mx.example.org", t0, 90*24*time.Hour)
	a, clk, calls := keeper(t, dir, func() (*tls.Certificate, error) { return next, nil })

	if a.Certificate() != nil || isReady(a) || a.Status().Ready {
		t.Fatal("ready before any certificate exists")
	}

	if wait := a.Step(context.Background()); wait != checkEvery {
		t.Errorf("after the first certificate: wait %s, want %s", wait, checkEvery)
	}
	if a.Certificate() != next || !isReady(a) {
		t.Fatal("the certificate is not in use")
	}
	if st := a.Status(); st.Mode != ModeACME || !st.Ready || st.Host != "mx.example.org" || !st.NotAfter.Equal(next.Leaf.NotAfter) || st.LastError != "" {
		t.Errorf("status = %+v", st)
	}

	// The private key is on disk for the next start, readable by the owner only.
	certFile, keyFile := a.certFiles()
	for _, f := range []string{certFile, keyFile} {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %o, want 600", filepath.Base(f), info.Mode().Perm())
		}
	}

	// With two thirds of the lifetime left nothing is ordered.
	clk.now = t0.Add(59 * 24 * time.Hour)
	a.Step(context.Background())
	if *calls != 1 {
		t.Fatalf("%d orders with 31 days left, want 1", *calls)
	}

	// With less than a third left it is renewed, and the new one is served.
	renewed := selfSigned(t, "mx.example.org", clk.now, 90*24*time.Hour)
	next = renewed
	clk.now = t0.Add(61 * 24 * time.Hour)
	a.Step(context.Background())
	if *calls != 2 || a.Certificate() != renewed {
		t.Errorf("%d orders with 29 days left; renewed certificate in use: %v", *calls, a.Certificate() == renewed)
	}
}

// A restart reads the certificate back instead of ordering another: the CA
// limits how many it issues for one host.
func TestACMEAStoredCertificateSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	first, _, _ := keeper(t, dir, func() (*tls.Certificate, error) {
		return selfSigned(t, "mx.example.org", t0, 90*24*time.Hour), nil
	})
	first.Step(context.Background())

	second, clk, calls := keeper(t, dir, func() (*tls.Certificate, error) { return nil, errors.New("must not be called") })
	cert, err := second.load()
	if err != nil || cert == nil {
		t.Fatalf("load: %v, %v", cert, err)
	}
	second.use(cert)
	second.Step(context.Background())
	if *calls != 0 || !isReady(second) {
		t.Errorf("%d orders after a restart, ready=%v", *calls, isReady(second))
	}
	if second.Certificate().Leaf.SerialNumber.Cmp(first.Certificate().Leaf.SerialNumber) != 0 {
		t.Error("another certificate than the stored one is in use")
	}

	// What is stored is not used when it has expired, or is for another host.
	clk.now = t0.Add(91 * 24 * time.Hour)
	if cert, err := second.load(); cert != nil || err == nil {
		t.Errorf("an expired certificate was loaded: %v", err)
	}
	clk.now = t0
	second.Host = "mail.example.org"
	if cert, err := second.load(); cert != nil || err == nil {
		t.Errorf("a certificate for another host was loaded: %v", err)
	}

	// Another CA, staging against production for one, has its own folder.
	other := NewACME("mx.example.org", "", "https://other-ca.test/directory", dir, second.Log)
	if other.Dir == first.Dir {
		t.Error("two directories share one folder")
	}
	if cert, err := other.load(); cert != nil || err != nil {
		t.Errorf("a certificate of another CA was loaded: %v, %v", cert, err)
	}
}

// A failure is retried later and later, so a host that cannot be validated
// does not use up the CA's limit on failed attempts.
func TestACMEBacksOffWhenTheCARefuses(t *testing.T) {
	fail := true
	good := selfSigned(t, "mx.example.org", t0, 90*24*time.Hour)
	a, _, calls := keeper(t, t.TempDir(), func() (*tls.Certificate, error) {
		if fail {
			return nil, errors.New("the CA could not reach port 80")
		}
		return good, nil
	})

	var waits []time.Duration
	for i := 0; i < 7; i++ {
		waits = append(waits, a.Step(context.Background()))
	}
	want := []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 2 * time.Hour, 2 * time.Hour}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
	}
	if isReady(a) || a.Certificate() != nil {
		t.Error("ready without a certificate")
	}
	if st := a.Status(); st.Ready || !strings.Contains(st.LastError, "port 80") {
		t.Errorf("status = %+v", st)
	}

	// In the first hour that is four attempts; Let's Encrypt allows five failures.
	var elapsed time.Duration
	attempts := 1
	for _, w := range waits {
		if elapsed += w; elapsed < time.Hour {
			attempts++
		}
	}
	if attempts > 5 {
		t.Errorf("%d attempts in the first hour", attempts)
	}

	fail = false
	if wait := a.Step(context.Background()); wait != checkEvery || !isReady(a) {
		t.Errorf("after recovery: wait %s, ready %v", wait, isReady(a))
	}
	if st := a.Status(); st.LastError != "" {
		t.Errorf("the old error is still reported: %q", st.LastError)
	}
	// The count starts over: the next failure waits two minutes again.
	a.cert.Store(selfSigned(t, "mx.example.org", t0.Add(-80*24*time.Hour), 90*24*time.Hour))
	fail = true
	if wait := a.Step(context.Background()); wait != 2*time.Minute {
		t.Errorf("first failure after a success: wait %s, want 2m", wait)
	}
	if *calls != 9 {
		t.Errorf("%d orders in total, want 9", *calls)
	}
}

func TestACMEAnswersOnlyItsOwnChallenge(t *testing.T) {
	a, _, _ := keeper(t, t.TempDir(), func() (*tls.Certificate, error) { return nil, errors.New("unused") })
	a.challenges["/.well-known/acme-challenge/token-1"] = "token-1.thumbprint"
	srv := httptest.NewServer(a)
	defer srv.Close()

	get := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if code, body := get("GET", "/.well-known/acme-challenge/token-1"); code != 200 || body != "token-1.thumbprint" {
		t.Errorf("the challenge: %d %q", code, body)
	}
	for _, path := range []string{"/", "/.well-known/acme-challenge/", "/.well-known/acme-challenge/other", "/ui/", "/healthz"} {
		if code, _ := get("GET", path); code != 404 {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
	if code, _ := get("POST", "/.well-known/acme-challenge/token-1"); code != 404 {
		t.Errorf("POST to the challenge = %d, want 404", code)
	}
}

func TestCertificateFromFiles(t *testing.T) {
	dir := t.TempDir()
	cert := selfSigned(t, "mx.example.org", t0, 24*time.Hour)
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	keyDER, _ := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)

	src, err := LoadFiles(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !isReady(src) || src.Certificate() == nil {
		t.Error("a certificate from files is ready at once")
	}
	if st := src.Status(); st.Mode != ModeFile || st.Host != "mx.example.org" || !st.Ready {
		t.Errorf("status = %+v", st)
	}
	if _, err := LoadFiles(certFile, filepath.Join(dir, "missing.pem")); err == nil {
		t.Error("a missing key file was accepted")
	}
	if _, err := LoadFiles(keyFile, certFile); err == nil {
		t.Error("swapped files were accepted")
	}
}

// A mail server often names no host in the handshake. There is one
// certificate, and every client gets it.
func TestTLSConfigServesTheCurrentCertificateWhateverIsAsked(t *testing.T) {
	a, _, _ := keeper(t, t.TempDir(), func() (*tls.Certificate, error) {
		return selfSigned(t, "mx.example.org", t0, 90*24*time.Hour), nil
	})
	cfg := TLSConfig(a)
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("minimum version = %x", cfg.MinVersion)
	}
	if _, err := cfg.GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Error("a certificate was served before there was one")
	}

	a.Step(context.Background())
	for _, name := range []string{"", "mx.example.org", "something.else.test"} {
		cert, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: name})
		if err != nil || cert != a.Certificate() {
			t.Errorf("server name %q: %v", name, err)
		}
	}
}
