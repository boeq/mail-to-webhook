// Package tlscert supplies the certificate for STARTTLS: read from files, or
// obtained and renewed through ACME, which is how Let's Encrypt issues them.
package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"
)

// Source is where the SMTP server gets its certificate.
type Source interface {
	// Certificate returns the current certificate, or nil while there is none.
	Certificate() *tls.Certificate
	// Ready is closed once a certificate is available.
	Ready() <-chan struct{}
	// Status describes the source for the health endpoint and the UI.
	Status() Status
}

// Modes of a Status.
const (
	ModeOff  = "off"
	ModeFile = "file"
	ModeACME = "acme"
)

type Status struct {
	Mode      string
	Host      string
	Ready     bool
	NotAfter  time.Time
	LastError string
}

// Off is the status when no certificate is configured.
func Off() Status { return Status{Mode: ModeOff} }

// Static is a certificate read from files once, at startup.
type Static struct {
	cert  *tls.Certificate
	ready chan struct{}
}

// LoadFiles reads a PEM certificate chain and its private key.
func LoadFiles(certFile, keyFile string) (*Static, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	if err := setLeaf(&cert); err != nil {
		return nil, err
	}
	ready := make(chan struct{})
	close(ready)
	return &Static{cert: &cert, ready: ready}, nil
}

func (s *Static) Certificate() *tls.Certificate { return s.cert }
func (s *Static) Ready() <-chan struct{}        { return s.ready }

func (s *Static) Status() Status {
	return Status{Mode: ModeFile, Host: hostOf(s.cert), Ready: true, NotAfter: s.cert.Leaf.NotAfter}
}

func setLeaf(cert *tls.Certificate) error {
	if cert.Leaf != nil {
		return nil
	}
	if len(cert.Certificate) == 0 {
		return fmt.Errorf("the certificate file holds no certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	cert.Leaf = leaf
	return nil
}

func hostOf(cert *tls.Certificate) string {
	if cert == nil || cert.Leaf == nil {
		return ""
	}
	if len(cert.Leaf.DNSNames) > 0 {
		return cert.Leaf.DNSNames[0]
	}
	return cert.Leaf.Subject.CommonName
}

// TLSConfig is the server configuration that serves whatever the source holds
// at the moment of the handshake, so a renewed certificate is picked up
// without a restart.
//
// The name a client asks for is ignored. A mail server often sends none, and
// there is one certificate here: the one for the host the MX record names.
func TLSConfig(src Source) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			if cert := src.Certificate(); cert != nil {
				return cert, nil
			}
			return nil, fmt.Errorf("no certificate yet")
		},
	}
}
