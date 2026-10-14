// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/extensions/effects-ssh/internal/sshtest"
	"github.com/phrocker/shoal-oss/extensions/effects-ssh/service"
)

const token = "caller-token-0123456789abcdef0123456789"

type fixture struct {
	dir, config, token, journal, id string
	host                            *sshtest.Server
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	host := sshtest.New(t)
	dir := t.TempDir()
	f := fixture{dir: dir, config: filepath.Join(dir, "config.json"),
		token: filepath.Join(dir, "token"), journal: filepath.Join(dir, "journal"), host: host}
	raw, _ := json.Marshal(map[string]any{"commands": map[string]any{"restart-web": map[string]any{
		"address": host.Addr(), "host_keys": []string{host.AuthorizedHostKey()}, "user": host.User,
		"private_key_file": host.ClientKeyFile, "command": "systemctl restart web", "run_timeout": "5s",
	}}})
	_ = os.WriteFile(f.config, raw, 0o600)
	_ = os.WriteFile(f.token, []byte(token), 0o600)
	_ = os.Mkdir(f.journal, 0o700)
	var stdout bytes.Buffer
	if code := run([]string{"journal", "init", "-journal-dir", f.journal}, &stdout, io.Discard, nil); code != exitOK {
		t.Fatalf("journal init exit %d", code)
	}
	f.id = strings.TrimSpace(stdout.String())
	return f
}

func (f fixture) args(extra ...string) []string {
	return append([]string{"-config", f.config, "-caller-token-file", f.token,
		"-journal-dir", f.journal, "-journal-id", f.id, "-journal-retention", "48h"}, extra...)
}

// TestServeRefusesAJournalItWasNotGiven: serve never creates a journal and
// never adopts one other than the pinned one.
func TestServeRefusesAJournalItWasNotGiven(t *testing.T) {
	f := newFixture(t)
	empty := filepath.Join(f.dir, "empty")
	_ = os.Mkdir(empty, 0o700)
	other := filepath.Join(f.dir, "other")
	_ = os.Mkdir(other, 0o700)
	if code := run([]string{"journal", "init", "-journal-dir", other}, io.Discard, io.Discard, nil); code != exitOK {
		t.Fatal("init other")
	}
	listen := []string{"-listen", "127.0.0.1:0", "-allow-plaintext-loopback"}
	base := []string{"-config", f.config, "-caller-token-file", f.token, "-journal-retention", "48h"}
	for name, args := range map[string][]string{
		"no journal id":    append(append(append([]string{}, base...), "-journal-dir", f.journal), listen...),
		"empty directory":  append(append(append([]string{}, base...), "-journal-dir", empty, "-journal-id", f.id), listen...),
		"another journal":  append(append(append([]string{}, base...), "-journal-dir", other, "-journal-id", f.id), listen...),
		"malformed id":     append(append(append([]string{}, base...), "-journal-dir", f.journal, "-journal-id", "x"), listen...),
		"compaction bound": append(f.args(listen...), "-journal-max-entries", "1000000"),
	} {
		var stderr bytes.Buffer
		if code := run(append([]string{"serve"}, args...), io.Discard, &stderr, nil); code != exitUsage {
			t.Errorf("%s: exit %d: %s", name, code, stderr.String())
		}
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 1 || entries[0].Name() != service.LockFileName {
		t.Fatalf("serve wrote into an empty directory: %v", entries)
	}
	// init runs once.
	var stderr bytes.Buffer
	if code := run([]string{"journal", "init", "-journal-dir", f.journal}, io.Discard, &stderr, nil); code != exitRuntime {
		t.Fatalf("second init exit %d", code)
	}
}

// TestJournalListShowsRecordsBeforeCorruption: the operator sees every record
// up to the bad line, and where and why it stopped.
func TestJournalListShowsRecordsBeforeCorruption(t *testing.T) {
	f := newFixture(t)
	file, err := os.OpenFile(filepath.Join(f.journal, service.JournalFileName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("deadbeef {\"op\":\"started\"}\n")
	_ = file.Close()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"journal", "list", "-journal-dir", f.journal}, &stdout, &stderr, nil); code != exitRuntime {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout.String(), `"journal_id":"`+f.id+`"`) ||
		!strings.Contains(stderr.String(), "line 2") || !strings.Contains(stderr.String(), "checksum mismatch") {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

func TestServeRefusesUnsafeListeners(t *testing.T) {
	f := newFixture(t)
	certFile, keyFile := writeCertificate(t)
	for name, args := range map[string][]string{
		"no tls, no flag":      f.args("-listen", "127.0.0.1:0"),
		"plaintext, remote":    f.args("-listen", "0.0.0.0:0", "-allow-plaintext-loopback"),
		"plaintext, any":       f.args("-listen", ":0", "-allow-plaintext-loopback"),
		"plaintext, host name": f.args("-listen", "example.com:0", "-allow-plaintext-loopback"),
		"half tls":             f.args("-listen", "127.0.0.1:0", "-tls-cert-file", certFile),
		"tls and plaintext":    f.args("-listen", "127.0.0.1:0", "-tls-cert-file", certFile, "-tls-key-file", keyFile, "-allow-plaintext-loopback"),
		"no token":             {"-config", f.config, "-journal-dir", f.journal, "-journal-retention", "48h", "-listen", "127.0.0.1:0", "-allow-plaintext-loopback"},
		"no retention":         {"-config", f.config, "-caller-token-file", f.token, "-journal-dir", f.journal, "-listen", "127.0.0.1:0", "-allow-plaintext-loopback"},
		"no journal":           {"-config", f.config, "-caller-token-file", f.token, "-journal-retention", "48h", "-listen", "127.0.0.1:0", "-allow-plaintext-loopback"},
		"missing journal dir":  {"-config", f.config, "-caller-token-file", f.token, "-journal-dir", filepath.Join(f.dir, "nope"), "-journal-retention", "48h", "-listen", "127.0.0.1:0", "-allow-plaintext-loopback"},
		"stray argument":       append(f.args("-listen", "127.0.0.1:0", "-allow-plaintext-loopback"), "extra"),
	} {
		var stderr bytes.Buffer
		if code := run(append([]string{"serve"}, args...), io.Discard, &stderr, nil); code != exitUsage {
			t.Errorf("%s: exit %d", name, code)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, "nope")); err == nil {
		t.Fatal("a missing journal directory was created")
	}
}

// TestServeOverTLSAndStop: the binary's serve path, over TLS, answers a run
// and shuts down cleanly; the journal is then listable.
func TestServeOverTLSAndStop(t *testing.T) {
	f := newFixture(t)
	certFile, keyFile := writeCertificate(t)
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	address := listener.Addr().String()
	_ = listener.Close()
	stop := make(chan struct{})
	stderr := &lockedBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- run(append([]string{"serve"}, f.args("-listen", address, "-tls-cert-file", certFile,
			"-tls-key-file", keyFile)...), io.Discard, stderr, stop)
	}()
	// The gateway's target client: verified TLS, HTTP/2 attempted.
	roots := x509.NewCertPool()
	certPEM, _ := os.ReadFile(certFile)
	roots.AppendCertsFromPEM(certPEM)
	client := &http.Client{Transport: &http.Transport{ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}}
	sum := sha256.Sum256([]byte("k"))
	key := base64.RawURLEncoding.EncodeToString(sum[:])
	var response *http.Response
	var err error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		request, _ := http.NewRequest(http.MethodPost, "https://"+address+service.PathPrefix+"restart-web", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Idempotency-Key", key)
		if response, err = client.Do(request); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.ProtoMajor != 2 || response.StatusCode != 200 || string(body) != `{"state":"completed","reference":"exit-0"}` {
		t.Fatalf("%s %d %s", response.Proto, response.StatusCode, body)
	}
	// Plaintext to the TLS listener is refused.
	if plain, err := http.Post("http://"+address+service.PathPrefix+"restart-web", "", nil); err == nil {
		if plain.StatusCode == 200 {
			t.Fatal("plaintext answered")
		}
		plain.Body.Close()
	}
	close(stop)
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"journal_bound_bytes"`) {
		t.Fatalf("no declared bound in the start line: %s", stderr.String())
	}
	var stdout bytes.Buffer
	if code := run([]string{"journal", "list", "-journal-dir", f.journal}, &stdout, io.Discard, nil); code != exitOK {
		t.Fatalf("list exit %d", code)
	}
	if lines := strings.Count(stdout.String(), "\n"); lines != 3 || strings.Contains(stdout.String(), `"stdout":`) {
		t.Fatalf("list: %s", stdout.String())
	}
	stdout.Reset()
	digest := service.KeyDigest(key)
	if code := run([]string{"journal", "show", "-journal-dir", f.journal, "-key", digest}, &stdout, io.Discard, nil); code != exitOK {
		t.Fatalf("show exit %d", code)
	}
	if strings.Count(stdout.String(), "\n") != 2 {
		t.Fatalf("show: %s", stdout.String())
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"journal"}, {"journal", "show", "-journal-dir", "x"},
		{"journal", "list"}} {
		if code := run(args, io.Discard, io.Discard, nil); code != exitUsage {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func writeCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "effects-ssh"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return certFile, keyFile
}
