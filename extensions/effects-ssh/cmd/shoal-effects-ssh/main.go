// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Command shoal-effects-ssh runs operator-registered commands over SSH for
// the effects gateway (#450). The gateway reaches it through an ordinary
// "key" route; docs/effects-ssh.md is the deployment guide.
//
//	shoal-effects-ssh journal init -journal-dir /var/lib/effects-ssh   # once; prints the ID
//	shoal-effects-ssh serve -config commands.json -listen 127.0.0.1:8443 \
//	    -allow-plaintext-loopback -caller-token-file /run/token \
//	    -journal-dir /var/lib/effects-ssh -journal-id <ID> -journal-retention 48h
//	shoal-effects-ssh journal list -journal-dir /var/lib/effects-ssh
//	shoal-effects-ssh journal show -journal-dir /var/lib/effects-ssh -key <key or digest>
package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/phrocker/shoal-oss/extensions/effects-ssh/service"
)

// Exit codes.
const (
	exitOK            = 0
	exitUsage         = 2
	exitRuntime       = 1
	exitJournalFailed = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, nil))
}

// run is main with its outputs and an optional stop channel, for tests.
func run(args []string, stdout, stderr io.Writer, stop <-chan struct{}) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: shoal-effects-ssh serve|journal ...")
		return exitUsage
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], stderr, stop)
	case "journal":
		return journal(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown subcommand %q; use serve or journal\n", args[0])
	return exitUsage
}

type serveConfig struct {
	server   *service.Server
	journal  *service.Journal
	listener net.Listener
	grace    time.Duration
	// tls is nil for plaintext on loopback.
	tls *tls.Config
}

func parseServe(args []string, stderr io.Writer) (*serveConfig, error) {
	flags := flag.NewFlagSet("shoal-effects-ssh serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFile := flags.String("config", "", "Command registry, strict JSON (required)")
	listen := flags.String("listen", "", "host:port to serve on (required)")
	certFile := flags.String("tls-cert-file", "", "TLS certificate chain (PEM)")
	keyFile := flags.String("tls-key-file", "", "TLS private key (PEM)")
	plaintext := flags.Bool("allow-plaintext-loopback", false,
		"Serve plaintext HTTP; refused unless -listen is a loopback address")
	tokenFile := flags.String("caller-token-file", "",
		"File holding the token the gateway presents as \"Authorization: Bearer <token>\". "+
			"Required unless -client-ca-file is set; with both, a request needs both")
	clientCA := flags.String("client-ca-file", "",
		"PEM bundle the gateway's client certificate must chain to, for client "+
			"authentication (#662). Requires TLS and at least one -client-identity. Read on every request")
	var clientIdentities stringList
	flags.Var(&clientIdentities, "client-identity",
		"A pinned client identity, repeatable: uri:<URI SAN>, dns:<DNS SAN> or "+
			"spki-sha256:<digest>. The certificate must match at least one")
	idempotencyHeader := flags.String("idempotency-header", service.DefaultIdempotencyHeader,
		"Header carrying the action's executor key: the gateway's -idempotency-header")
	journalDir := flags.String("journal-dir", "", "Directory holding an initialized journal (journal init); a persistent volume (required)")
	journalID := flags.String("journal-id", "", "The journal's identity, as journal init printed it (required); a directory holding another journal, or none, is refused")
	retention := flags.Duration("journal-retention", 0,
		"How long a key is remembered after its last record (required). The gateway's "+
			"-idempotency-retention must not exceed it")
	maxEntries := flags.Int("journal-max-entries", 10000, "Keys held at most; a new key beyond it is refused")
	maxRunning := flags.Int("max-concurrent", 4, "Commands run at once")
	attachWait := flags.Duration("attach-wait", 30*time.Second,
		"How long a repeat of a running key waits for it; below the gateway's -operation-timeout")
	grace := flags.Duration("shutdown-grace", 60*time.Second,
		"On SIGTERM, how long running commands may finish before they are abandoned as unknown")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected argument %q; every setting is a flag", flags.Arg(0))
	}
	switch {
	case *configFile == "":
		return nil, errors.New("-config is required")
	case *listen == "":
		return nil, errors.New("-listen is required")
	case *tokenFile == "" && *clientCA == "":
		return nil, errors.New("-caller-token-file or -client-ca-file is required; there is no " +
			"unauthenticated mode")
	case *clientCA == "" && len(clientIdentities) > 0:
		return nil, errors.New("-client-identity requires -client-ca-file")
	case *journalDir == "":
		return nil, errors.New("-journal-dir is required")
	case *journalID == "":
		return nil, errors.New("-journal-id is required: the identity journal init printed, so " +
			"an empty or different volume is refused rather than served as a fresh journal")
	case *retention == 0:
		return nil, errors.New("-journal-retention is required")
	case *grace <= 0:
		return nil, errors.New("-shutdown-grace must be positive")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return nil, errors.New("-listen must be host:port")
	}
	tlsEnabled := *certFile != "" || *keyFile != ""
	switch {
	case tlsEnabled && (*certFile == "" || *keyFile == ""):
		return nil, errors.New("-tls-cert-file and -tls-key-file are required together")
	case tlsEnabled && *plaintext:
		return nil, errors.New("-allow-plaintext-loopback and TLS are mutually exclusive")
	case !tlsEnabled && !*plaintext:
		return nil, errors.New("TLS is required: set -tls-cert-file and -tls-key-file, or " +
			"-allow-plaintext-loopback with a loopback -listen address")
	case !tlsEnabled && *clientCA != "":
		return nil, errors.New("-client-ca-file requires TLS: a plaintext listener has no " +
			"client certificate")
	case !tlsEnabled && !loopback(host):
		return nil, errors.New("-allow-plaintext-loopback requires -listen to be a loopback " +
			"address; the caller token and every answer would cross the network in the clear")
	}
	var clientAuth *service.ClientAuth
	if *clientCA != "" {
		if clientAuth, err = service.NewClientAuth(*clientCA, clientIdentities, nil); err != nil {
			return nil, fmt.Errorf("-client-ca-file: %w", err)
		}
	}
	raw, err := os.ReadFile(*configFile)
	if err != nil {
		return nil, errors.New("-config cannot be read")
	}
	config, err := service.ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	if err := service.CheckJournalBounds(*maxEntries, config.MaxOutputBytes()); err != nil {
		return nil, err
	}
	journal, err := service.OpenJournal(*journalDir, service.JournalOptions{
		Retention: *retention, MaxEntries: *maxEntries, ID: *journalID})
	if err != nil {
		return nil, err
	}
	server, err := service.NewServer(service.Options{
		Config: config, Journal: journal, IdempotencyHeader: *idempotencyHeader,
		CallerTokenFile: *tokenFile, ClientAuth: clientAuth,
		MaxRunning: *maxRunning, AttachWait: *attachWait,
		Log: stderr,
	})
	if err != nil {
		_ = journal.Close()
		return nil, err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		_ = journal.Close()
		return nil, fmt.Errorf("-listen: %w", err)
	}
	if !tlsEnabled {
		// "localhost" is resolved by the system, which need not map it to
		// loopback; what was actually bound is what counts.
		bound, ok := listener.Addr().(*net.TCPAddr)
		if !ok || !bound.IP.IsLoopback() {
			_ = listener.Close()
			_ = journal.Close()
			return nil, errors.New("-allow-plaintext-loopback: -listen did not bind a loopback address")
		}
	}
	var tlsConfig *tls.Config
	if tlsEnabled {
		certificate, err := newServingCertificate(*certFile, *keyFile)
		if err != nil {
			_ = listener.Close()
			_ = journal.Close()
			return nil, err
		}
		certificate.report = server.LogEvent
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certificate.get}
		if clientAuth != nil {
			// Request, and do not verify, at the handshake: the handshake
			// still proves the client holds the certificate's key, and the
			// chain and the pin are checked in the request
			// (service.ClientAuth), so a refusal is an answer the journal
			// can turn into outcome_unknown for a key it holds.
			tlsConfig.ClientAuth = tls.RequestClientCert
		}
	}
	logStart(stderr, config, *retention, *maxEntries, listener.Addr().String(), tlsEnabled,
		authFactors(*tokenFile != "", clientAuth != nil))
	return &serveConfig{server: server, journal: journal, listener: listener,
		grace: *grace, tls: tlsConfig}, nil
}

// authFactors names the caller checks in force, for the start line.
func authFactors(token, certificate bool) []string {
	var factors []string
	if token {
		factors = append(factors, "token")
	}
	if certificate {
		factors = append(factors, "client_certificate")
	}
	return factors
}

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// servingCertificate is the listener's certificate, read again at each
// handshake so a rotated Secret takes effect without a restart. A pair that
// cannot be loaded at a handshake keeps the last one that could; at startup
// it refuses the start.
type servingCertificate struct {
	certFile, keyFile string
	mu                sync.Mutex
	last              *tls.Certificate
	// report receives a failed reload (the server's serialized log), at most
	// once a minute, so a broken rotation is seen before the last good
	// certificate expires. It names nothing from the files.
	report   func(event, reason string)
	reported time.Time
}

func newServingCertificate(certFile, keyFile string) (*servingCertificate, error) {
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, errors.New("TLS certificate and key cannot be loaded")
	}
	return &servingCertificate{certFile: certFile, keyFile: keyFile, last: &certificate}, nil
}

func (s *servingCertificate) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	certificate, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	s.mu.Lock()
	if err == nil {
		s.last = &certificate
		s.mu.Unlock()
		return &certificate, nil
	}
	last := s.last
	report := s.report != nil && (s.reported.IsZero() || time.Since(s.reported) >= time.Minute)
	if report {
		s.reported = time.Now()
	}
	s.mu.Unlock()
	// Outside the lock: a blocked log writer must not stall every handshake.
	if report {
		s.report("tls_certificate_reload_failed", "serving_last_loaded_pair")
	}
	return last, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

// logStart records the declared bounds, so the journal's size is something an
// operator can read off rather than discover.
func logStart(out io.Writer, config *service.Config, retention time.Duration, maxEntries int, address string, tlsEnabled bool, auth []string) {
	perEntry := service.MaxRecordBytes(config.MaxOutputBytes())
	line, _ := json.Marshal(map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano), "event": "start",
		"listen": address, "tls": tlsEnabled, "caller_auth": auth, "commands": config.Names(),
		"journal_retention": retention.String(), "journal_max_entries": maxEntries,
		"journal_max_entry_bytes": perEntry,
		"journal_bound_bytes":     service.JournalBound(maxEntries, config.MaxOutputBytes()),
	})
	_, _ = out.Write(append(line, '\n'))
}

func serve(args []string, stderr io.Writer, stop <-chan struct{}) int {
	cfg, err := parseServe(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, err)
		}
		return exitUsage
	}
	httpServer := &http.Server{
		Handler:           cfg.server,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// No WriteTimeout: the answer to a run is written when the run ends,
		// which the run timeout bounds. ReadTimeout covers reading the
		// request; the handler clears it once the body is read
		// (Server.ServeHTTP).
		TLSConfig: cfg.tls,
	}
	served := make(chan error, 1)
	go func() {
		if cfg.tls != nil {
			// ServeTLS, not a tls.Listener: it is what enables HTTP/2, which
			// the gateway's client attempts.
			served <- httpServer.ServeTLS(cfg.listener, "", "")
			return
		}
		served <- httpServer.Serve(cfg.listener)
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	code := exitOK
	select {
	case <-signals:
	case <-stop:
	case <-cfg.journal.Failed():
		fmt.Fprintln(stderr, "journal write failed; no new command is started. Draining and exiting")
		code = exitJournalFailed
	case err := <-served:
		fmt.Fprintln(stderr, "serve:", err)
		code = exitRuntime
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.grace)
	defer cancel()
	shutdown := make(chan struct{})
	go func() {
		_ = httpServer.Shutdown(ctx)
		close(shutdown)
	}()
	cfg.server.Shutdown(ctx)
	<-shutdown
	_ = httpServer.Close()
	if err := cfg.journal.Close(); err != nil && code == exitOK {
		code = exitRuntime
	}
	if cfg.journal.Err() != nil {
		code = exitJournalFailed
	}
	return code
}

func journal(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "list" && args[0] != "show" && args[0] != "init") {
		fmt.Fprintln(stderr, "usage: shoal-effects-ssh journal init|list|show -journal-dir DIR [-key KEY]")
		return exitUsage
	}
	if args[0] == "init" {
		return journalInit(args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("shoal-effects-ssh journal "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("journal-dir", "", "Journal directory (required)")
	key := flags.String("key", "", "Executor key, or its 12-hex-digit digest (show only)")
	if err := flags.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if *dir == "" || (args[0] == "show") != (*key != "") {
		fmt.Fprintln(stderr, "-journal-dir is required, and -key exactly with show")
		return exitUsage
	}
	records, torn, err := service.ReadJournal(*dir)
	var corrupt *service.CorruptError
	if err != nil && !errors.As(err, &corrupt) {
		fmt.Fprintln(stderr, err)
		return exitRuntime
	}
	encoder := json.NewEncoder(stdout)
	wanted := canonicalKey(*key)
	for _, record := range records {
		digest := service.KeyDigest(record.Key)
		switch args[0] {
		case "list":
			// Metadata only: output stays in the file unless asked for by key.
			if record.Finished != nil {
				finished := *record.Finished
				finished.Stdout, finished.Stderr = nil, nil
				record.Finished = &finished
			}
		case "show":
			if record.Key == "" || (record.Key != wanted && digest != *key) {
				continue
			}
		}
		if record.Key == "" {
			digest = ""
		}
		_ = encoder.Encode(struct {
			service.JournalRecord
			Digest string `json:"digest"`
		}{record, digest})
	}
	if corrupt != nil {
		// Everything before the bad line is above; this is where and why the
		// server refuses to open the journal.
		fmt.Fprintf(stderr, "journal is corrupt at line %d (byte offset %d): %s; the records "+
			"before it are listed above, and nothing after it was read\n",
			corrupt.Line, corrupt.Offset, corrupt.Reason)
		return exitRuntime
	}
	if torn {
		fmt.Fprintln(stderr, "the journal ends in an incomplete record; the server drops it on its next start")
	}
	return exitOK
}

// journalInit creates the journal once and prints its identity, which serve
// must then be given as -journal-id.
func journalInit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("shoal-effects-ssh journal init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("journal-dir", "", "Existing, empty directory on the journal's volume (required)")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if *dir == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "-journal-dir is required")
		return exitUsage
	}
	id, err := service.InitJournal(*dir, nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRuntime
	}
	fmt.Fprintln(stdout, id)
	return exitOK
}

// canonicalKey accepts a key in either platform spelling.
func canonicalKey(key string) string {
	if decoded, err := base64.StdEncoding.Strict().DecodeString(key); err == nil && strings.HasSuffix(key, "=") {
		return base64.RawURLEncoding.EncodeToString(decoded)
	}
	return key
}
