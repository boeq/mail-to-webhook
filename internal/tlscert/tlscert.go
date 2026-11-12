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
	Host      string // the first, or only, host
	Ready     bool   // at least one certificate is in use
	NotAfter  time.Time
	LastError string
	// Hosts has one entry per host when certificates are kept per host.
	Hosts []HostStatus
}

type HostStatus struct {
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
// A mail server often names no host in the handshake, and then it gets the
// certificate of the first host. A source with a certificate per host serves
// the one for the name that was asked for, when it has it.
func TLSConfig(src Source) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if byName, ok := src.(interface {
				CertificateFor(string) *tls.Certificate
			}); ok {
				if cert := byName.CertificateFor(hello.ServerName); cert != nil {
					return cert, nil
				}
			}
			if cert := src.Certificate(); cert != nil {
				return cert, nil
			}
			return nil, fmt.Errorf("no certificate yet")
		},
	}
}
