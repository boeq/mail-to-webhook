// mail-to-webhook receives mail over SMTP and posts each message to a webhook
// as Postmark-shaped JSON.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/boeq/mail-to-webhook/internal/config"
	"github.com/boeq/mail-to-webhook/internal/delivery"
	"github.com/boeq/mail-to-webhook/internal/limits"
	"github.com/boeq/mail-to-webhook/internal/recipients"
	"github.com/boeq/mail-to-webhook/internal/smtpsrv"
	"github.com/boeq/mail-to-webhook/internal/store"
	"github.com/boeq/mail-to-webhook/internal/tlscert"
	"github.com/boeq/mail-to-webhook/internal/ui"
	"github.com/boeq/mail-to-webhook/internal/webhook"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mail-to-webhook:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log := newLogger(cfg.LogLevel)

	st, err := store.Open(cfg.DataDir, cfg.StoreRaw)
	if err != nil {
		return fmt.Errorf("history in %s: %w", cfg.DataDir, err)
	}
	defer st.Close()
	if n, err := st.RecoverInterrupted(time.Now()); err != nil {
		log.Error("could not settle interrupted deliveries", "err", err)
	} else if n > 0 {
		log.Info("settled deliveries interrupted by the last shutdown", "count", n)
	}

	wh := webhook.New(webhook.Options{
		URL:             cfg.WebhookURL,
		Timeout:         cfg.WebhookTimeout,
		Secret:          cfg.WebhookSecret,
		AuthUser:        cfg.WebhookAuthUser,
		AuthPass:        cfg.WebhookAuthPass,
		Headers:         cfg.WebhookHeaders,
		PermanentStatus: cfg.PermanentStatus,
	})
	if !wh.Signed() {
		log.Warn("WEBHOOK_SECRET is not set: webhooks are delivered without a signature header")
	}
	if len(cfg.AllowedDomains) == 0 {
		log.Warn("ALLOWED_DOMAINS is empty: mail for every domain is accepted")
	}

	engine := delivery.New(cfg, st, wh, log)
	checker := &recipients.Checker{
		URL:        cfg.RecipientCheckURL,
		Timeout:    cfg.RecipientCheckTimeout,
		Recheck:    cfg.RecipientRecheck,
		UnknownTTL: cfg.RecipientUnknownTTL,
		Store:      st,
		Client:     wh,
		Log:        log,
		Now:        time.Now,
	}
	backend := &smtpsrv.Backend{
		Cfg:     cfg,
		Engine:  engine,
		Store:   st,
		Checker: checker,
		Limiter: limits.New(limits.Config{
			MaxConnections:         cfg.MaxConnections,
			MaxConnectionsPerIP:    cfg.MaxConnectionsPerIP,
			MaxConnectionsPerIPMin: cfg.MaxConnectionsPerIPMin,
			BlockAfterBadRcpts:     cfg.BlockAfterBadRcpts,
			BlockDuration:          cfg.BlockDuration,
		}),
		Log: log,
		Now: time.Now,
	}

	// The certificate for STARTTLS: from files, through ACME, or none.
	var certs tlscert.Source
	var keeper *tlscert.Group
	var acmeServer *http.Server
	var acmeListener net.Listener
	tlsStatus := tlscert.Off
	switch {
	case cfg.TLSCert != "":
		static, err := tlscert.LoadFiles(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return fmt.Errorf("TLS_CERT/TLS_KEY: %w", err)
		}
		certs, tlsStatus = static, static.Status
	case cfg.ACMEEnabled:
		keeper = tlscert.NewGroup(cfg.SMTPDomains, cfg.ACMEEmail, cfg.ACMEDirectoryURL, cfg.DataDir, log)
		// The CA fetches its challenge from port 80 of the host; this listener
		// answers those requests and nothing else.
		acmeListener, err = net.Listen("tcp", cfg.ACMEHTTPAddr)
		if err != nil {
			return fmt.Errorf("ACME_HTTP_ADDR %s: %w", cfg.ACMEHTTPAddr, err)
		}
		acmeServer = &http.Server{Handler: keeper, ReadHeaderTimeout: 10 * time.Second}
		certs, tlsStatus = keeper, keeper.Status
	}

	smtpServer := smtpsrv.New(backend, certs)
	smtpListener, err := smtpsrv.Listen(backend)
	if err != nil {
		return fmt.Errorf("SMTP_ADDR %s: %w", cfg.SMTPAddr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health(st, checker, tlsStatus))
	mux.HandleFunc("GET /readyz", health(st, checker, tlsStatus))
	if cfg.UIEnabled {
		ui.Mount(mux, &ui.UI{
			Cfg:      cfg,
			Store:    st,
			Engine:   engine,
			Checker:  checker,
			Log:      log,
			Version:  version,
			SMTPAddr: smtpListener.Addr().String(),
			TLS:      tlsStatus,
			Now:      time.Now,
		})
	}
	httpServer := &http.Server{Addr: cfg.HTTPAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	httpListener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("HTTP_ADDR %s: %w", cfg.HTTPAddr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	failed := make(chan error, 3)
	go engine.Run(ctx)
	if keeper != nil {
		go keeper.Run(ctx)
		go func() {
			if err := acmeServer.Serve(acmeListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- fmt.Errorf("acme http: %w", err)
			}
		}()
	}
	go func() {
		if err := smtpServer.Serve(smtpListener); err != nil && !errors.Is(err, smtp.ErrServerClosed) {
			failed <- fmt.Errorf("smtp: %w", err)
		}
	}()
	go func() {
		if err := httpServer.Serve(httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- fmt.Errorf("http: %w", err)
		}
	}()

	log.Info("mail-to-webhook started",
		"version", version,
		"smtp", cfg.SMTPAddr,
		"hostnames", cfg.SMTPDomains,
		"http", cfg.HTTPAddr,
		"webhook", wh.URL(),
		"recipient_check", checker.Enabled(),
		"ui", cfg.UIEnabled,
		"starttls", tlsStatus().Mode,
	)

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-failed:
		log.Error("server stopped", "err", err)
		stop()
	}

	// A transaction in flight is waiting for the webhook; give it the time a
	// delivery can take before the connection is cut.
	grace, cancel := context.WithTimeout(context.Background(),
		cfg.WebhookTimeout*time.Duration(cfg.WebhookRetries+1)+10*time.Second)
	defer cancel()
	_ = smtpServer.Shutdown(grace)
	_ = httpServer.Shutdown(grace)
	if acmeServer != nil {
		_ = acmeServer.Shutdown(grace)
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

// health answers 200 while the history is readable. The recipient check is
// reported but does not fail the probe: when the consumer is down, mail is
// deferred, which is the service working as intended.
func health(st *store.Store, checker *recipients.Checker, tlsStatus func() tlscert.Status) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{"status": "ok"}
		code := http.StatusOK
		if _, err := st.StatsSince(time.Now()); err != nil {
			body["status"] = "degraded"
			body["history"] = "unavailable"
			code = http.StatusServiceUnavailable
		}
		if h := checker.Health(); h.Enabled {
			body["recipient_check"] = map[string]any{"ok": h.OK}
		}
		if t := tlsStatus(); t.Mode != tlscert.ModeOff {
			starttls := map[string]any{"mode": t.Mode, "ready": t.Ready}
			if t.Ready {
				starttls["expires"] = t.NotAfter.UTC().Format(time.RFC3339)
			}
			var hosts []map[string]any
			for _, h := range t.Hosts {
				host := map[string]any{"host": h.Host, "ready": h.Ready}
				if h.Ready {
					host["expires"] = h.NotAfter.UTC().Format(time.RFC3339)
				}
				hosts = append(hosts, host)
			}
			if hosts != nil {
				starttls["hosts"] = hosts
			}
			body["starttls"] = starttls
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
}

// healthcheck is what the container's HEALTHCHECK runs; the image has no shell.
func healthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthz answered", resp.StatusCode)
		return 1
	}
	return 0
}
