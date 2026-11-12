package tlscert

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"
)

// LetsEncryptURL is the directory used when none is configured.
const LetsEncryptURL = acme.LetsEncryptURL

// checkEvery is how often a healthy certificate is looked at again.
const checkEvery = 12 * time.Hour

// retryAfter are the waits after consecutive failures. Let's Encrypt allows
// five failed validations per host and hour; this stays under it.
var retryAfter = []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour}

// ACME obtains a certificate for one host and keeps it renewed.
//
// It proves control of the host with the http-01 challenge: the CA fetches a
// token over plain HTTP on port 80 of the host. ACME is an http.Handler that
// answers exactly those requests; serve it on port 80.
//
// Go's autocert package does the same job for web servers, and is not used
// here for two reasons. It picks the certificate by the name in the TLS
// handshake, which a mail server often does not send. And it tries a
// challenge on port 443 before the one on port 80, a validation that fails on
// every issuance for a host that only speaks SMTP.
type ACME struct {
	Host         string
	Email        string // optional contact for expiry notices
	DirectoryURL string
	Dir          string // where the certificate of this host is kept
	AccountKey   string // path of the account key, shared by all hosts of one CA
	Log          *slog.Logger
	Now          func() time.Time

	// issue obtains a fresh certificate. Tests replace it.
	issue func(context.Context) (*tls.Certificate, error)

	cert      atomic.Pointer[tls.Certificate]
	ready     chan struct{}
	readyOnce sync.Once

	mu         sync.Mutex
	challenges map[string]string // request path -> response body
	lastError  string
	failures   int
}

// NewACME prepares a source. Run does the work.
func NewACME(host, email, directoryURL, dataDir string, log *slog.Logger) *ACME {
	if directoryURL == "" {
		directoryURL = LetsEncryptURL
	}
	// A certificate from one CA must not be served after a switch to another,
	// staging to production in particular, so each directory has its own folder.
	sum := sha256.Sum256([]byte(directoryURL))
	base := filepath.Join(dataDir, "acme", hex.EncodeToString(sum[:4]))
	a := &ACME{
		Host:         host,
		Email:        email,
		DirectoryURL: directoryURL,
		Dir:          filepath.Join(base, host),
		AccountKey:   filepath.Join(base, "account.key"),
		Log:          log,
		Now:          time.Now,
		ready:        make(chan struct{}),
		challenges:   map[string]string{},
	}
	a.issue = a.order
	return a
}

func (a *ACME) Certificate() *tls.Certificate { return a.cert.Load() }
func (a *ACME) Ready() <-chan struct{}        { return a.ready }

func (a *ACME) Status() Status {
	st := Status{Mode: ModeACME, Host: a.Host}
	if cert := a.cert.Load(); cert != nil {
		st.Ready = true
		st.NotAfter = cert.Leaf.NotAfter
	}
	a.mu.Lock()
	st.LastError = a.lastError
	a.mu.Unlock()
	return st
}

// Load puts a stored certificate into use, so that a restart does not start
// with an order.
func (a *ACME) Load() {
	if cert, err := a.load(); err != nil {
		a.Log.Warn("stored certificate not usable, requesting a new one", "host", a.Host, "err", err)
	} else if cert != nil {
		a.use(cert)
		a.Log.Info("certificate loaded", "host", a.Host, "expires", cert.Leaf.NotAfter.UTC().Format(time.RFC3339))
	}
}

// Step looks at the certificate once, obtains or renews it when that is due,
// and returns how long to wait before the next look.
func (a *ACME) Step(ctx context.Context) time.Duration {
	current := a.cert.Load()
	if current != nil && !a.due(current) {
		return checkEvery
	}

	cert, err := a.issue(ctx)
	if err == nil {
		err = a.save(cert)
	}
	if err != nil {
		a.mu.Lock()
		a.lastError = err.Error()
		wait := retryAfter[min(a.failures, len(retryAfter)-1)]
		a.failures++
		a.mu.Unlock()
		a.Log.Error("could not obtain a certificate", "host", a.Host, "err", err, "retry_in", wait.String())
		return wait
	}

	a.mu.Lock()
	a.lastError, a.failures = "", 0
	a.mu.Unlock()
	a.use(cert)
	a.Log.Info("certificate obtained", "host", a.Host, "expires", cert.Leaf.NotAfter.UTC().Format(time.RFC3339))
	return checkEvery
}

func (a *ACME) use(cert *tls.Certificate) {
	a.cert.Store(cert)
	a.readyOnce.Do(func() { close(a.ready) })
}

// due reports whether less than a third of the lifetime is left, which for a
// ninety-day certificate is thirty days.
func (a *ACME) due(cert *tls.Certificate) bool {
	lifetime := cert.Leaf.NotAfter.Sub(cert.Leaf.NotBefore)
	return cert.Leaf.NotAfter.Sub(a.Now()) < lifetime/3
}

// ServeHTTP answers the CA's http-01 requests and nothing else.
func (a *ACME) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	serveChallenge(w, r, a)
}

// challenge returns the response for a challenge path this host is waiting on.
func (a *ACME) challenge(path string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	response, ok := a.challenges[path]
	return response, ok
}

func serveChallenge(w http.ResponseWriter, r *http.Request, keepers ...*ACME) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		for _, a := range keepers {
			if response, ok := a.challenge(r.URL.Path); ok {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = io.WriteString(w, response)
				return
			}
		}
	}
	http.NotFound(w, r)
}

// order runs one ACME order from account to certificate.
func (a *ACME) order(ctx context.Context) (*tls.Certificate, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	accountKey, err := a.accountKey()
	if err != nil {
		return nil, fmt.Errorf("account key: %w", err)
	}
	client := &acme.Client{Key: accountKey, DirectoryURL: a.DirectoryURL, UserAgent: "mail-to-webhook"}

	account := &acme.Account{}
	if a.Email != "" {
		account.Contact = []string{"mailto:" + a.Email}
	}
	if _, err := client.Register(ctx, account, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("register the account: %w", err)
	}

	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(a.Host))
	if err != nil {
		return nil, fmt.Errorf("open the order: %w", err)
	}
	for _, url := range order.AuthzURLs {
		if err := a.authorize(ctx, client, url); err != nil {
			return nil, err
		}
	}
	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, fmt.Errorf("wait for the order: %w", err)
	}

	// RSA: every mail server still in use can verify it, which is not true of ECDSA.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: a.Host},
		DNSNames: []string{a.Host},
	}, key)
	if err != nil {
		return nil, err
	}
	// CreateCertFromOrder rather than CreateOrderCert: the latter finds the
	// order again through a Location header that not every CA sends.
	chain, _, err := client.CreateCertFromOrder(ctx, order, csr, true)
	if err != nil {
		return nil, fmt.Errorf("finalize the order: %w", err)
	}
	cert := &tls.Certificate{Certificate: chain, PrivateKey: key}
	if err := setLeaf(cert); err != nil {
		return nil, err
	}
	return cert, nil
}

// authorize proves control of the host for one authorization.
func (a *ACME) authorize(ctx context.Context, client *acme.Client, url string) error {
	authz, err := client.GetAuthorization(ctx, url)
	if err != nil {
		return fmt.Errorf("read the authorization: %w", err)
	}
	if authz.Status == acme.StatusValid {
		return nil
	}
	var challenge *acme.Challenge
	for _, c := range authz.Challenges {
		if c.Type == "http-01" {
			challenge = c
			break
		}
	}
	if challenge == nil {
		return errors.New("the CA offers no http-01 challenge for this host")
	}
	response, err := client.HTTP01ChallengeResponse(challenge.Token)
	if err != nil {
		return err
	}
	path := client.HTTP01ChallengePath(challenge.Token)

	a.mu.Lock()
	a.challenges[path] = response
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.challenges, path)
		a.mu.Unlock()
	}()

	if _, err := client.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("accept the challenge: %w", err)
	}
	if _, err := client.WaitAuthorization(ctx, authz.URI); err != nil {
		return fmt.Errorf("the CA could not validate http://%s%s (is port 80 of the host reachable?): %w", a.Host, path, err)
	}
	return nil
}

func (a *ACME) accountKey() (crypto.Signer, error) {
	path := a.AccountKey
	if raw, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("%s is not PEM", path)
		}
		return x509.ParseECPrivateKey(block.Bytes)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return key, a.write(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func (a *ACME) certFiles() (string, string) {
	return filepath.Join(a.Dir, "cert.pem"), filepath.Join(a.Dir, "key.pem")
}

// save writes the certificate so a restart does not order a new one. The CA
// limits how many it issues per host and week.
func (a *ACME) save(cert *tls.Certificate) error {
	var chain []byte
	for _, der := range cert.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return err
	}
	certFile, keyFile := a.certFiles()
	if err := a.write(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})); err != nil {
		return err
	}
	return a.write(certFile, chain)
}

// load returns the stored certificate when it is for this host and has not
// expired, and nil when there is none.
func (a *ACME) load() (*tls.Certificate, error) {
	certFile, keyFile := a.certFiles()
	if _, err := os.Stat(certFile); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	if err := setLeaf(&cert); err != nil {
		return nil, err
	}
	if err := cert.Leaf.VerifyHostname(a.Host); err != nil {
		return nil, fmt.Errorf("it is for another host: %w", err)
	}
	if !a.Now().Before(cert.Leaf.NotAfter) {
		return nil, fmt.Errorf("it expired on %s", cert.Leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	return &cert, nil
}

func (a *ACME) write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
