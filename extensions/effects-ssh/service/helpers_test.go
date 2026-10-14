// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/extensions/effects-ssh/internal/sshtest"
)

const testToken = "caller-token-0123456789abcdef0123456789"

type answerFixture struct {
	Name   string            `json:"name"`
	Status int               `json:"status"`
	Header map[string]string `json:"header"`
	Body   string            `json:"body"`
}

func loadAnswers(t *testing.T) map[string]answerFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "contract", "answers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list []answerFixture
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]answerFixture{}
	for _, answer := range list {
		out[answer.Name] = answer
	}
	return out
}

func testKey(name string) string {
	sum := sha256.Sum256([]byte("key|" + name))
	return base64.RawURLEncoding.EncodeToString(sum[:])
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

// fakeHost answers the test commands. "block" waits for release.
type fakeHost struct {
	ssh     *sshtest.Server
	release chan struct{}
	entered chan string
}

func newFakeHost(t *testing.T) *fakeHost {
	host := &fakeHost{ssh: sshtest.New(t), release: make(chan struct{}), entered: make(chan string, 16)}
	host.ssh.Handle(func(command string, stdout, stderr io.Writer, closed <-chan struct{}) sshtest.Exit {
		host.entered <- command
		switch command {
		case "systemctl restart web":
			_, _ = io.WriteString(stdout, "restarted\n")
			return sshtest.Exit{}
		case "exit three loudly":
			_, _ = stdout.Write(bytes.Repeat([]byte("o"), 100))
			_, _ = io.WriteString(stderr, "bad")
			return sshtest.Exit{Status: 3}
		case "die":
			return sshtest.Exit{Signal: "KILL"}
		case "weird signal":
			return sshtest.Exit{Signal: "kill; rm -rf /"}
		case "quiet":
			return sshtest.Exit{NoStatus: true}
		case "hang":
			<-closed
			return sshtest.Exit{}
		case "block":
			select {
			case <-host.release:
			case <-closed:
			}
			return sshtest.Exit{}
		}
		return sshtest.Exit{Status: 127}
	})
	return host
}

func commandJSON(host *fakeHost, command, runTimeout string, extra map[string]any) map[string]any {
	entry := map[string]any{
		"address": host.ssh.Addr(), "host_keys": []string{host.ssh.AuthorizedHostKey()},
		"user": host.ssh.User, "private_key_file": host.ssh.ClientKeyFile,
		"command": command, "run_timeout": runTimeout, "connect_timeout": "5s",
	}
	for k, v := range extra {
		entry[k] = v
	}
	return entry
}

func testConfigJSON(t *testing.T, host *fakeHost) []byte {
	t.Helper()
	other := sshtest.New(t)
	commands := map[string]any{
		"restart-web":  commandJSON(host, "systemctl restart web", "5s", nil),
		"loud":         commandJSON(host, "exit three loudly", "5s", map[string]any{"max_output_bytes": 16}),
		"die":          commandJSON(host, "die", "5s", nil),
		"weird":        commandJSON(host, "weird signal", "5s", nil),
		"quiet":        commandJSON(host, "quiet", "5s", nil),
		"hang":         commandJSON(host, "hang", "300ms", nil),
		"block":        commandJSON(host, "block", "30s", nil),
		"block-2":      commandJSON(host, "block", "30s", nil),
		"wrong-host":   commandJSON(host, "systemctl restart web", "5s", map[string]any{"host_keys": []string{other.AuthorizedHostKey()}}),
		"restart-web2": commandJSON(host, "systemctl restart web", "5s", nil),
	}
	raw, err := json.Marshal(map[string]any{"commands": commands})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type env struct {
	t         *testing.T
	host      *fakeHost
	dir       string
	tokenFile string
	config    *Config
	journal   *Journal
	server    *Server
	http      *httptest.Server
	logs      *lockedBuffer
	options   Options
	// serverTLS, when set before open, serves TLS; client is then the
	// client requests are sent with.
	serverTLS *tls.Config
	client    *http.Client
}

type envOption func(*Options, *JournalOptions)

func newEnv(t *testing.T, options ...envOption) *env {
	t.Helper()
	return newEnvWith(t, nil, options...)
}

// newEnvWith is newEnv with serverTLS set before the server opens.
func newEnvWith(t *testing.T, serverTLS *tls.Config, options ...envOption) *env {
	t.Helper()
	host := newFakeHost(t)
	e := &env{t: t, host: host, dir: t.TempDir(), logs: &lockedBuffer{}, serverTLS: serverTLS}
	e.tokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(e.tokenFile, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := ParseConfig(testConfigJSON(t, host))
	if err != nil {
		t.Fatal(err)
	}
	e.config = config
	e.options = Options{Config: config, CallerTokenFile: e.tokenFile, MaxRunning: 4,
		AttachWait: 2 * time.Second, Log: e.logs}
	journalOptions := JournalOptions{Retention: time.Hour, MaxEntries: 100}
	for _, option := range options {
		option(&e.options, &journalOptions)
	}
	e.open(journalOptions)
	return e
}

func (e *env) open(journalOptions JournalOptions) {
	e.t.Helper()
	ensureJournal(e.t, e.dir)
	journal, err := OpenJournal(e.dir, journalOptions)
	if err != nil {
		e.t.Fatal(err)
	}
	e.journal = journal
	e.options.Journal = journal
	server, err := NewServer(e.options)
	if err != nil {
		e.t.Fatal(err)
	}
	e.server = server
	if e.serverTLS != nil {
		e.http = httptest.NewUnstartedServer(server)
		e.http.TLS = e.serverTLS.Clone()
		e.http.StartTLS()
	} else {
		e.http = httptest.NewServer(server)
	}
	e.t.Cleanup(func() {
		e.http.Close()
		server.Shutdown(expiredContext())
		_ = journal.Close()
	})
}

// restart stops this process's server and journal, as a clean exit would,
// and opens new ones on the same directory.
func (e *env) restart(journalOptions JournalOptions) {
	e.t.Helper()
	e.http.Close()
	e.server.Shutdown(expiredContext())
	if err := e.journal.Close(); err != nil {
		e.t.Fatal(err)
	}
	e.open(journalOptions)
}

// ensureJournal initializes dir's journal unless it already holds one.
func ensureJournal(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, JournalFileName)); err == nil {
		return
	}
	if _, err := InitJournal(dir, nil); err != nil {
		t.Fatal(err)
	}
}

// headerLine is a valid journal header, for tests that write files by hand.
func headerLine(t *testing.T) []byte {
	t.Helper()
	line, err := encodeLine(record{Op: recordHeader, JournalID: "0123456789abcdef0123456789abcdef",
		At: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func (s *Server) isStopped() bool {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	return s.stopped
}

func expiredContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

type reply struct {
	status int
	header http.Header
	body   string
}

func (e *env) do(method, path, key, body string, headers map[string][]string) reply {
	e.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, e.http.URL+path, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	for name, values := range headers {
		request.Header.Del(name)
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	client := e.client
	if client == nil {
		client = e.http.Client()
	}
	response, err := client.Do(request)
	if err != nil {
		e.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return reply{status: response.StatusCode, header: response.Header, body: string(raw)}
}

func (e *env) post(name, key string) reply {
	e.t.Helper()
	return e.do(http.MethodPost, PathPrefix+name, key, "", nil)
}

// is asserts a reply is exactly a pinned answer: status, every header the
// service sets and no other, and the body bytes.
func (r reply) is(t *testing.T, answers map[string]answerFixture, name string) {
	t.Helper()
	fixture, ok := answers[name]
	if !ok {
		t.Fatalf("no fixture %q", name)
	}
	var problems []string
	if r.status != fixture.Status {
		problems = append(problems, fmt.Sprintf("status %d, want %d", r.status, fixture.Status))
	}
	if r.body != fixture.Body {
		problems = append(problems, fmt.Sprintf("body %q, want %q", r.body, fixture.Body))
	}
	var extra []string
	for name := range r.header {
		if name == "Date" || name == "Content-Length" {
			continue
		}
		if _, pinned := fixture.Header[name]; !pinned {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		problems = append(problems, fmt.Sprintf("unpinned headers %q", extra))
	}
	for name, value := range fixture.Header {
		if got := r.header.Values(name); len(got) != 1 || got[0] != value {
			problems = append(problems, fmt.Sprintf("header %s = %q, want %q", name, got, value))
		}
	}
	if len(problems) > 0 {
		t.Fatalf("answer is not %s: %s", name, strings.Join(problems, "; "))
	}
}
