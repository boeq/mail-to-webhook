package smtpsrv

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"

	"github.com/emersion/go-smtp"

	"github.com/boeq/mail-to-webhook/internal/tlscert"
)

// Server runs the SMTP conversation on one listener and offers STARTTLS from
// the moment a certificate exists.
//
// A go-smtp server either advertises STARTTLS or it does not, decided when it
// is configured. A certificate obtained through ACME arrives later, minutes
// after the first start. So there are two servers behind the one listener,
// one without TLS and one with, and each new connection goes to the one that
// fits: nothing is ever advertised that cannot be honoured, and nothing needs
// a restart.
type Server struct {
	plain  *smtp.Server
	secure *smtp.Server // nil without a certificate source
	certs  tlscert.Source

	plainConns  *connQueue
	secureConns *connQueue

	mu       sync.Mutex
	listener net.Listener
	closing  atomic.Bool
}

// New returns a server for the backend. certs may be nil: no STARTTLS then.
func New(b *Backend, certs tlscert.Source) *Server {
	s := &Server{plain: newSMTPServer(b), certs: certs}
	if certs != nil {
		s.secure = newSMTPServer(b)
		s.secure.TLSConfig = tlscert.TLSConfig(certs)
	}
	return s
}

func newSMTPServer(b *Backend) *smtp.Server {
	srv := smtp.NewServer(b)
	srv.Addr = b.Cfg.SMTPAddr
	srv.Domain = b.Cfg.SMTPDomain
	srv.MaxMessageBytes = b.Cfg.MaxMessageBytes
	srv.MaxRecipients = b.Cfg.MaxRecipients
	srv.MaxLineLength = b.Cfg.MaxLineBytes
	srv.ReadTimeout = b.Cfg.ReadTimeout
	srv.WriteTimeout = b.Cfg.WriteTimeout
	// Without SMTPUTF8 a sender with an internationalised address cannot
	// deliver at all. BINARYMIME stays off: senders encode instead, and
	// nothing is gained by taking unencoded binary.
	srv.EnableSMTPUTF8 = true
	srv.ErrorLog = logAdapter{b.Log}
	return srv
}

// Serve accepts connections until the listener fails or Shutdown is called.
// After Shutdown it returns nil.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	s.listener = l
	s.mu.Unlock()

	if s.secure == nil {
		if err := s.plain.Serve(l); err != nil && !errors.Is(err, smtp.ErrServerClosed) {
			return err
		}
		return nil
	}

	s.plainConns = newConnQueue(l.Addr())
	s.secureConns = newConnQueue(l.Addr())
	go func() { _ = s.plain.Serve(s.plainConns) }()
	go func() { _ = s.secure.Serve(s.secureConns) }()

	for {
		conn, err := l.Accept()
		if err != nil {
			s.plainConns.Close()
			s.secureConns.Close()
			if s.closing.Load() {
				return nil
			}
			return err
		}
		target := s.plainConns
		select {
		case <-s.certs.Ready():
			target = s.secureConns
		default:
		}
		target.put(conn)
	}
}

// Shutdown stops accepting and waits for the conversations in flight.
func (s *Server) Shutdown(ctx context.Context) error {
	s.closing.Store(true)
	s.mu.Lock()
	l := s.listener
	s.mu.Unlock()
	if l != nil && s.secure != nil {
		_ = l.Close()
	}
	err := s.plain.Shutdown(ctx)
	if s.secure != nil {
		if serr := s.secure.Shutdown(ctx); err == nil {
			err = serr
		}
	}
	return err
}

// connQueue hands accepted connections to one of the two servers, as the
// net.Listener that server reads from.
type connQueue struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	addr  net.Addr
}

func newConnQueue(addr net.Addr) *connQueue {
	return &connQueue{conns: make(chan net.Conn), done: make(chan struct{}), addr: addr}
}

func (q *connQueue) put(c net.Conn) {
	select {
	case q.conns <- c:
	case <-q.done:
		_ = c.Close()
	}
}

func (q *connQueue) Accept() (net.Conn, error) {
	select {
	case c := <-q.conns:
		return c, nil
	case <-q.done:
		return nil, net.ErrClosed
	}
}

func (q *connQueue) Close() error {
	q.once.Do(func() { close(q.done) })
	return nil
}

func (q *connQueue) Addr() net.Addr { return q.addr }
