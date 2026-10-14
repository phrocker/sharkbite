// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package sshclient is the hardened SSH client the effects-ssh extension
// runs commands through, written so the session gateway (#447) can share it.
//
// It depends on nothing else in this module, only on golang.org/x/crypto/ssh
// and the standard library, so moving it into a module of its own when a
// second consumer exists is a file move (TestSSHClientIsSelfContained holds
// that).
//
// What it will not do, by construction rather than by configuration:
//
//   - accept a host key that is not pinned (no known_hosts, no trust on first
//     use, no InsecureIgnoreHostKey);
//   - authenticate other than by one private key (no password, no
//     keyboard-interactive, no agent);
//   - request a PTY, an environment variable, X11, agent forwarding or a port
//     forward; channels the server opens towards the client are rejected, and
//     global requests are answered false;
//   - send anything on stdin (it is closed at once);
//   - buffer more output than the configured bound, or run without a timeout.
package sshclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// MaxPrivateKeyBytes bounds a private key file.
const MaxPrivateKeyBytes = 64 << 10

// LoadSigner reads one unencrypted private key. An encrypted key is refused:
// there is no passphrase source, and prompting is not a mode a service has.
func LoadSigner(path string) (ssh.Signer, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("private key file cannot be opened")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxPrivateKeyBytes+1))
	if err != nil {
		return nil, errors.New("private key file cannot be read")
	}
	if len(raw) > MaxPrivateKeyBytes {
		return nil, fmt.Errorf("private key file exceeds %d bytes", MaxPrivateKeyBytes)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		return nil, errors.New("private key is encrypted; provide an unencrypted " +
			"key through a secret volume")
	}
	if err != nil {
		return nil, errors.New("private key file holds no parseable private key")
	}
	return signer, nil
}

// Target is one host and the identity used on it.
type Target struct {
	// Address is host:port.
	Address  string
	User     string
	HostKeys *HostKeys
	Signer   ssh.Signer
	// ConnectTimeout bounds the TCP connect, the SSH handshake and
	// authentication together.
	ConnectTimeout time.Duration
	// Dial replaces the TCP dialer (tests count connections through it).
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// ClientConfig is the hardened configuration for t.
func (t Target) ClientConfig() (*ssh.ClientConfig, error) {
	switch {
	case t.HostKeys == nil:
		return nil, errors.New("target has no pinned host keys")
	case t.Signer == nil:
		return nil, errors.New("target has no private key")
	case t.User == "":
		return nil, errors.New("target has no user")
	}
	return &ssh.ClientConfig{
		User:              t.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(t.Signer)},
		HostKeyCallback:   t.HostKeys.Callback(),
		HostKeyAlgorithms: t.HostKeys.Algorithms(),
		// The banner is target-controlled text; it is not logged or kept.
		BannerCallback: func(string) error { return nil },
		Timeout:        t.ConnectTimeout,
	}, nil
}

// Dial connects and authenticates. Every error it returns means no session
// was opened, so nothing can have run.
func Dial(ctx context.Context, t Target) (*ssh.Client, error) {
	config, err := t.ClientConfig()
	if err != nil {
		return nil, err
	}
	if t.ConnectTimeout <= 0 {
		return nil, errors.New("connect timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, t.ConnectTimeout)
	defer cancel()
	dial := t.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", t.Address)
	if err != nil {
		return nil, fmt.Errorf("ssh: connect: %w", err)
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh: connect: %w", err)
	}
	// The deadline bounds a peer that stalls the handshake; a cancelled
	// context closes the connection at once.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	sshConn, channels, requests, err := ssh.NewClientConn(conn, t.Address, config)
	if !stop() {
		if err == nil {
			_ = sshConn.Close()
		}
		return nil, fmt.Errorf("ssh: handshake: %w", context.Cause(ctx))
	}
	if err != nil {
		_ = conn.Close()
		if errors.Is(err, ErrHostKeyMismatch) {
			return nil, ErrHostKeyMismatch
		}
		return nil, fmt.Errorf("ssh: handshake: %w", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = sshConn.Close()
		return nil, fmt.Errorf("ssh: handshake: %w", err)
	}
	// NewClient rejects every channel the server opens (no handler is ever
	// registered: no forwarding, no agent) and answers global requests false.
	return ssh.NewClient(sshConn, channels, requests), nil
}

// Outcome is what is known about whether the command ran.
type Outcome string

const (
	// OutcomeCompleted: the command ran and the host reported how it ended
	// (an exit status or a signal).
	OutcomeCompleted Outcome = "completed"
	// OutcomeNotRun: no session was opened, so the command cannot have run.
	OutcomeNotRun Outcome = "not_run"
	// OutcomeUnknown: the command may have started and how it ended is not
	// known: the exec request's answer was lost, the run timed out, the
	// transport failed, or the host closed the session without a status.
	OutcomeUnknown Outcome = "unknown"
)

// Reason is a closed set saying why a run ended as it did, for the journal
// and the log. It never carries target-controlled text.
type Reason string

const (
	ReasonNone         Reason = ""
	ReasonConnect      Reason = "connect"
	ReasonHostKey      Reason = "host_key"
	ReasonSession      Reason = "session"
	ReasonStart        Reason = "start"
	ReasonTimeout      Reason = "timeout"
	ReasonCanceled     Reason = "canceled"
	ReasonNoExitStatus Reason = "no_exit_status"
	ReasonTransport    Reason = "transport"
)

// Limits bound one run.
type Limits struct {
	// RunTimeout bounds the command, from the exec request to its exit.
	RunTimeout time.Duration
	// MaxOutputBytes bounds what is kept of stdout and of stderr, each. The
	// rest is read and counted, never buffered, so the remote end is never
	// stalled by flow control.
	MaxOutputBytes int
	// KeepaliveInterval, when positive, sends keepalive@openssh.com that
	// often; a keepalive that fails closes the connection, which ends the
	// run as unknown rather than leaving it to the run timeout.
	KeepaliveInterval time.Duration
}

// Result is one run.
type Result struct {
	Outcome Outcome
	Reason  Reason
	// ExitStatus is set when Outcome is completed and Signal is empty.
	ExitStatus int
	// Signal is the signal name the host reported, when the command was
	// killed by one. It is target-controlled text; callers constrain it.
	Signal string
	Stdout Output
	Stderr Output
}

// Output is a bounded capture of one stream.
type Output struct {
	Data      []byte
	Bytes     int64
	Truncated bool
}

// Run connects, runs exactly one command and waits for it, within limits.
// The context ends the run early (as unknown once the command may have
// started); it is not the request's context, which may end for reasons that
// do not stop a command already running.
func Run(ctx context.Context, t Target, command string, limits Limits) Result {
	if limits.RunTimeout <= 0 || limits.MaxOutputBytes <= 0 {
		return Result{Outcome: OutcomeNotRun, Reason: ReasonConnect}
	}
	client, err := Dial(ctx, t)
	if err != nil {
		reason := ReasonConnect
		if errors.Is(err, ErrHostKeyMismatch) {
			reason = ReasonHostKey
		}
		return Result{Outcome: OutcomeNotRun, Reason: reason}
	}
	defer client.Close()
	// Opening the session waits on the host with no timeout of its own, so
	// the connect timeout bounds it: when it ends the connection is closed,
	// which makes NewSession return. No exec request has been sent then.
	openCtx, cancelOpen := context.WithTimeout(ctx, t.ConnectTimeout)
	stopOpen := context.AfterFunc(openCtx, func() { _ = client.Close() })
	session, err := client.NewSession()
	opened := stopOpen()
	cancelOpen()
	if err != nil || !opened {
		// The session channel was refused, never confirmed, or not confirmed
		// in time: no exec request was sent on it.
		reason := ReasonSession
		if ctx.Err() != nil {
			reason = ReasonCanceled
		}
		return Result{Outcome: OutcomeNotRun, Reason: reason}
	}
	defer session.Close()
	stdout := &boundedBuffer{limit: limits.MaxOutputBytes}
	stderr := &boundedBuffer{limit: limits.MaxOutputBytes}
	session.Stdout, session.Stderr = stdout, stderr
	// Stdin stays nil: the library sends EOF at once. No RequestPty, no
	// Setenv, no RequestSubsystem, no agent request: exec is the only
	// request this client makes on a session.

	runCtx, cancel := context.WithTimeout(ctx, limits.RunTimeout)
	defer cancel()
	// The exec request's reply is also awaited with no timeout of its own:
	// the run's end closes the connection whatever the client is waiting on.
	context.AfterFunc(runCtx, func() { _ = client.Close() })
	endReason := func() Reason {
		if ctx.Err() != nil {
			return ReasonCanceled
		}
		return ReasonTimeout
	}
	if limits.KeepaliveInterval > 0 {
		go keepalive(runCtx, client, limits.KeepaliveInterval)
	}
	if err := session.Start(command); err != nil {
		// The exec request may have reached the host and its answer been
		// lost; there is no way to tell that from a refusal, so it is unknown.
		reason := ReasonStart
		if runCtx.Err() != nil {
			reason = endReason()
		}
		return finish(Result{Outcome: OutcomeUnknown, Reason: reason}, stdout, stderr)
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		result := waited(err)
		if result.Outcome == OutcomeUnknown && runCtx.Err() != nil {
			result.Reason = endReason()
		}
		return finish(result, stdout, stderr)
	case <-runCtx.Done():
		// Closing does not stop a command already running on the host. The
		// result says unknown; what the command did is not known.
		reason := endReason()
		_ = session.Close()
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		return finish(Result{Outcome: OutcomeUnknown, Reason: reason}, stdout, stderr)
	}
}

func waited(err error) Result {
	if err == nil {
		return Result{Outcome: OutcomeCompleted, ExitStatus: 0}
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		if signal := exit.Signal(); signal != "" {
			return Result{Outcome: OutcomeCompleted, Signal: signal}
		}
		return Result{Outcome: OutcomeCompleted, ExitStatus: exit.ExitStatus()}
	}
	var missing *ssh.ExitMissingError
	if errors.As(err, &missing) {
		return Result{Outcome: OutcomeUnknown, Reason: ReasonNoExitStatus}
	}
	return Result{Outcome: OutcomeUnknown, Reason: ReasonTransport}
}

func finish(result Result, stdout, stderr *boundedBuffer) Result {
	result.Stdout, result.Stderr = stdout.output(), stderr.output()
	return result
}

func keepalive(ctx context.Context, client *ssh.Client, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		replied := make(chan error, 1)
		go func() {
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			replied <- err
		}()
		select {
		case err := <-replied:
			if err != nil {
				_ = client.Close()
				return
			}
		case <-time.After(interval):
			_ = client.Close()
			return
		case <-ctx.Done():
			return
		}
	}
}

// boundedBuffer keeps the first limit bytes written and counts the rest. It
// never returns an error, so the copy from the channel keeps draining.
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
	total int64
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += int64(len(p))
	if room := b.limit - len(b.data); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		b.data = append(b.data, p[:room]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) output() Output {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Output{
		Data:      append([]byte(nil), b.data...),
		Bytes:     b.total,
		Truncated: b.total > int64(len(b.data)),
	}
}
