// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package sshclient_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"go/parser"
	"go/token"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/phrocker/shoal-oss/extensions/effects-ssh/internal/sshtest"
	"github.com/phrocker/shoal-oss/extensions/effects-ssh/sshclient"
)

func target(t *testing.T, server *sshtest.Server) sshclient.Target {
	t.Helper()
	keys, err := sshclient.ParseHostKeys([]string{server.AuthorizedHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := sshclient.LoadSigner(server.ClientKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	return sshclient.Target{Address: server.Addr(), User: server.User, HostKeys: keys,
		Signer: signer, ConnectTimeout: 5 * time.Second}
}

var limits = sshclient.Limits{RunTimeout: 5 * time.Second, MaxOutputBytes: 64}

func TestRunCompletesAndCapturesBoundedOutput(t *testing.T) {
	server := sshtest.New(t)
	server.Handle(func(command string, stdout, stderr io.Writer, _ <-chan struct{}) sshtest.Exit {
		_, _ = io.WriteString(stdout, "hello\n")
		_, _ = stderr.Write(bytes.Repeat([]byte("e"), 1000))
		return sshtest.Exit{Status: 0}
	})
	result := sshclient.Run(context.Background(), target(t, server), "uptime", limits)
	if result.Outcome != sshclient.OutcomeCompleted || result.ExitStatus != 0 || result.Signal != "" {
		t.Fatalf("result = %+v", result)
	}
	if string(result.Stdout.Data) != "hello\n" || result.Stdout.Truncated || result.Stdout.Bytes != 6 {
		t.Fatalf("stdout = %+v", result.Stdout)
	}
	if len(result.Stderr.Data) != 64 || !result.Stderr.Truncated || result.Stderr.Bytes != 1000 {
		t.Fatalf("stderr = %d bytes kept of %d, truncated %v",
			len(result.Stderr.Data), result.Stderr.Bytes, result.Stderr.Truncated)
	}
	if server.Execs("uptime") != 1 {
		t.Fatalf("execs = %d", server.Execs("uptime"))
	}
}

func TestRunRecordsExitStatusAndSignal(t *testing.T) {
	server := sshtest.New(t)
	server.Handle(func(command string, _, _ io.Writer, _ <-chan struct{}) sshtest.Exit {
		if command == "fail" {
			return sshtest.Exit{Status: 3}
		}
		return sshtest.Exit{Signal: "KILL"}
	})
	if result := sshclient.Run(context.Background(), target(t, server), "fail", limits); result.Outcome != sshclient.OutcomeCompleted || result.ExitStatus != 3 {
		t.Fatalf("exit: %+v", result)
	}
	if result := sshclient.Run(context.Background(), target(t, server), "die", limits); result.Outcome != sshclient.OutcomeCompleted || result.Signal != "KILL" {
		t.Fatalf("signal: %+v", result)
	}
}

func TestRunWithoutAnExitStatusIsUnknown(t *testing.T) {
	server := sshtest.New(t)
	server.Handle(func(command string, _, _ io.Writer, _ <-chan struct{}) sshtest.Exit {
		switch command {
		case "drop":
			return sshtest.Exit{Drop: true}
		case "malformed":
			return sshtest.Exit{Malformed: true}
		}
		return sshtest.Exit{NoStatus: true}
	})
	if result := sshclient.Run(context.Background(), target(t, server), "malformed", limits); result.Outcome != sshclient.OutcomeUnknown || result.Reason != sshclient.ReasonTransport {
		t.Fatalf("malformed status: %+v", result)
	}
	if result := sshclient.Run(context.Background(), target(t, server), "quiet", limits); result.Outcome != sshclient.OutcomeUnknown || result.Reason != sshclient.ReasonNoExitStatus {
		t.Fatalf("no status: %+v", result)
	}
	if result := sshclient.Run(context.Background(), target(t, server), "drop", limits); result.Outcome != sshclient.OutcomeUnknown {
		t.Fatalf("drop: %+v", result)
	}
}

// TestExecWhoseReplyIsNotSuccessIsUnknown: once the exec request has been
// sent, a refusal or a lost reply is unknown, never not_run: the host may
// have started the command either way.
func TestExecWhoseReplyIsNotSuccessIsUnknown(t *testing.T) {
	for _, answer := range []sshtest.ExecAnswer{sshtest.ExecRefuse, sshtest.ExecDrop} {
		server := sshtest.New(t)
		server.AnswerExec(answer)
		result := sshclient.Run(context.Background(), target(t, server), "uptime", limits)
		if result.Outcome != sshclient.OutcomeUnknown || result.Reason != sshclient.ReasonStart {
			t.Fatalf("answer %d: %+v", answer, result)
		}
		if server.Execs("uptime") != 1 {
			t.Fatalf("answer %d: the exec request did not reach the host", answer)
		}
	}
}

// TestStalledHostIsBounded: a host that authenticates and then never
// confirms the session, or never replies to exec, while answering
// keepalives, is bounded by the connect timeout and the run timeout. A
// stalled session open is not_run (no exec was sent); a stalled exec reply
// is unknown.
func TestStalledHostIsBounded(t *testing.T) {
	for _, c := range []struct {
		answer  sshtest.ExecAnswer
		outcome sshclient.Outcome
		reason  sshclient.Reason
	}{
		{sshtest.SessionStall, sshclient.OutcomeNotRun, sshclient.ReasonSession},
		{sshtest.ExecStall, sshclient.OutcomeUnknown, sshclient.ReasonTimeout},
	} {
		server := sshtest.New(t)
		server.AnswerExec(c.answer)
		tgt := target(t, server)
		tgt.ConnectTimeout = 500 * time.Millisecond
		start := time.Now()
		done := make(chan sshclient.Result, 1)
		go func() {
			done <- sshclient.Run(context.Background(), tgt, "uptime", sshclient.Limits{
				RunTimeout: 500 * time.Millisecond, MaxOutputBytes: 64, KeepaliveInterval: 100 * time.Millisecond})
		}()
		select {
		case result := <-done:
			if result.Outcome != c.outcome || result.Reason != c.reason {
				t.Fatalf("answer %d: %+v", c.answer, result)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("answer %d: Run is still blocked after %v", c.answer, time.Since(start))
		}
	}
}

func TestRunTimesOutAsUnknown(t *testing.T) {
	server := sshtest.New(t)
	server.Handle(func(_ string, _, _ io.Writer, closed <-chan struct{}) sshtest.Exit {
		<-closed
		return sshtest.Exit{}
	})
	start := time.Now()
	result := sshclient.Run(context.Background(), target(t, server), "hang",
		sshclient.Limits{RunTimeout: 200 * time.Millisecond, MaxOutputBytes: 64})
	if result.Outcome != sshclient.OutcomeUnknown || result.Reason != sshclient.ReasonTimeout {
		t.Fatalf("result = %+v", result)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
}

func TestCancelledRunIsUnknown(t *testing.T) {
	server := sshtest.New(t)
	entered := make(chan struct{})
	server.Handle(func(_ string, _, _ io.Writer, closed <-chan struct{}) sshtest.Exit {
		close(entered)
		<-closed
		return sshtest.Exit{}
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-entered; cancel() }()
	result := sshclient.Run(ctx, target(t, server), "hang", limits)
	if result.Outcome != sshclient.OutcomeUnknown || result.Reason != sshclient.ReasonCanceled {
		t.Fatalf("result = %+v", result)
	}
}

// TestUnpinnedHostKeyIsRefusedBeforeAnySession: a host presenting a key that
// is not pinned gets no session, no exec, and the run is not_run.
func TestUnpinnedHostKeyIsRefusedBeforeAnySession(t *testing.T) {
	server := sshtest.New(t)
	other := sshtest.New(t)
	tgt := target(t, server)
	tgt.HostKeys, _ = sshclient.ParseHostKeys([]string{other.AuthorizedHostKey()})
	result := sshclient.Run(context.Background(), tgt, "uptime", limits)
	if result.Outcome != sshclient.OutcomeNotRun || result.Reason != sshclient.ReasonHostKey {
		t.Fatalf("result = %+v", result)
	}
	if server.TotalExecs() != 0 || len(server.Opened()) != 0 {
		t.Fatalf("execs %d, opened %q", server.TotalExecs(), server.Opened())
	}
}

func TestAuthenticationFailureAndUnreachableAreNotRun(t *testing.T) {
	server := sshtest.New(t)
	tgt := target(t, server)
	tgt.User = "someone-else"
	if result := sshclient.Run(context.Background(), tgt, "uptime", limits); result.Outcome != sshclient.OutcomeNotRun || result.Reason != sshclient.ReasonConnect {
		t.Fatalf("auth: %+v", result)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	tgt = target(t, server)
	tgt.Address = address
	if result := sshclient.Run(context.Background(), tgt, "uptime", limits); result.Outcome != sshclient.OutcomeNotRun {
		t.Fatalf("unreachable: %+v", result)
	}
	if server.TotalExecs() != 0 {
		t.Fatal("a command ran")
	}
}

// TestHandshakeStallIsBounded: a peer that accepts TCP and never speaks SSH
// is abandoned at the connect timeout.
func TestHandshakeStallIsBounded(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	server := sshtest.New(t)
	tgt := target(t, server)
	tgt.Address = listener.Addr().String()
	tgt.ConnectTimeout = 200 * time.Millisecond
	start := time.Now()
	if result := sshclient.Run(context.Background(), tgt, "uptime", limits); result.Outcome != sshclient.OutcomeNotRun {
		t.Fatalf("result = %+v", result)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("stalled handshake took %v", elapsed)
	}
}

// TestClientAsksForExecOnly: one session channel, one exec request; no PTY,
// env, agent, X11 or subsystem request; and every channel the server opens
// towards the client is rejected.
func TestClientAsksForExecOnly(t *testing.T) {
	server := sshtest.New(t)
	reverse := []string{"auth-agent@openssh.com", "forwarded-tcpip", "x11", "direct-tcpip", "session"}
	server.ReverseOpen(reverse...)
	if result := sshclient.Run(context.Background(), target(t, server), "uptime", limits); result.Outcome != sshclient.OutcomeCompleted {
		t.Fatalf("result = %+v", result)
	}
	if requests := server.Requests(); !slices.Equal(requests, []string{"exec"}) {
		t.Fatalf("session requests = %q", requests)
	}
	if opened := server.Opened(); !slices.Equal(opened, []string{"session"}) {
		t.Fatalf("channels opened = %q", opened)
	}
	for _, channelType := range reverse {
		if !server.Rejected(channelType) {
			t.Fatalf("client accepted a %s channel", channelType)
		}
	}
}

func TestTransportFailureMidRunIsUnknown(t *testing.T) {
	server := sshtest.New(t)
	var conn atomic.Pointer[net.Conn]
	server.Handle(func(_ string, _, _ io.Writer, closed <-chan struct{}) sshtest.Exit {
		if c := conn.Load(); c != nil {
			_ = (*c).Close()
		}
		<-closed
		return sshtest.Exit{}
	})
	tgt := target(t, server)
	tgt.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			conn.Store(&c)
		}
		return c, err
	}
	result := sshclient.Run(context.Background(), tgt, "uptime",
		sshclient.Limits{RunTimeout: 5 * time.Second, MaxOutputBytes: 64, KeepaliveInterval: 50 * time.Millisecond})
	if result.Outcome != sshclient.OutcomeUnknown {
		t.Fatalf("result = %+v", result)
	}
}

func TestParseHostKeysRefusals(t *testing.T) {
	server := sshtest.New(t)
	good := server.AuthorizedHostKey()
	for name, lines := range map[string][]string{
		"none":       nil,
		"garbage":    {"not a key"},
		"options":    {`from="10.0.0.1" ` + good},
		"duplicate":  {good, good + " comment"},
		"two lines":  {good + "\n" + good},
		"too many":   slices.Repeat([]string{good}, sshclient.MaxHostKeys+1),
		"cert":       {certificateLine(t)},
		"sk-ed25519": {"sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIEX/dQ0v4127bEo8eeG1EV0ApO2lWbSnN6RWusn/NjqIAAAABHNzaDo="},
	} {
		if _, err := sshclient.ParseHostKeys(lines); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	keys, err := sshclient.ParseHostKeys([]string{good + " host-comment"})
	if err != nil || !slices.Equal(keys.Algorithms(), []string{ssh.KeyAlgoED25519}) {
		t.Fatalf("good key: %v %v", keys, err)
	}
}

func certificateLine(t *testing.T) string {
	t.Helper()
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewPublicKey(public)
	signer, _ := ssh.NewSignerFromKey(private)
	cert := &ssh.Certificate{Key: key, CertType: ssh.HostCert, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert)))
}

func TestHostKeyCallbackRefusesCertificatesAndOtherKeys(t *testing.T) {
	server := sshtest.New(t)
	keys, err := sshclient.ParseHostKeys([]string{server.AuthorizedHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	callback := keys.Callback()
	if err := callback("h", nil, server.HostKey.PublicKey()); err != nil {
		t.Fatalf("pinned key refused: %v", err)
	}
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewPublicKey(public)
	if err := callback("h", nil, other); err == nil {
		t.Fatal("unpinned key accepted")
	}
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(private)
	cert := &ssh.Certificate{Key: server.HostKey.PublicKey(), CertType: ssh.HostCert, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}
	if err := callback("h", nil, cert); err == nil {
		t.Fatal("a certificate over the pinned key was accepted")
	}
}

func TestLoadSignerRefusesEncryptedAndGarbage(t *testing.T) {
	dir := t.TempDir()
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(private, "", []byte("pass"))
	if err != nil {
		t.Fatal(err)
	}
	encrypted := filepath.Join(dir, "encrypted")
	_ = os.WriteFile(encrypted, pem.EncodeToMemory(block), 0o600)
	if _, err := sshclient.LoadSigner(encrypted); err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("encrypted: %v", err)
	}
	garbage := filepath.Join(dir, "garbage")
	_ = os.WriteFile(garbage, []byte("nope"), 0o600)
	if _, err := sshclient.LoadSigner(garbage); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := sshclient.LoadSigner(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing accepted")
	}
}

// TestSSHClientIsSelfContained: the package imports only the standard
// library and golang.org/x/crypto/ssh, so it can move to a shared module for
// #447 without taking any of effects-ssh with it.
func TestSSHClientIsSelfContained(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			if path == "golang.org/x/crypto/ssh" || !strings.Contains(strings.Split(path, "/")[0], ".") {
				continue
			}
			t.Errorf("%s imports %s", name, path)
		}
	}
}
