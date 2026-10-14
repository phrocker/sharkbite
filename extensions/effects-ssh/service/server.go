// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package service is the effects-ssh HTTP service: POST /v1/commands/{name}
// runs one operator-registered command over SSH, at most once per
// idempotency key, and answers in the contract the effects gateway's route
// classifies (contract.go).
package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"

	"github.com/phrocker/shoal-oss/extensions/effects-ssh/sshclient"
)

const (
	// PathPrefix is the route prefix; the command name is the one segment
	// after it.
	PathPrefix = "/v1/commands/"
	// DefaultIdempotencyHeader is what the gateway's -idempotency-header is
	// conventionally set to.
	DefaultIdempotencyHeader = "Idempotency-Key"
	// maxBody bounds what is read of a request body. v1 accepts only an empty
	// body or {}.
	maxBody = 1024
	// MinCallerTokenBytes is the shortest caller token accepted.
	MinCallerTokenBytes = 32
	maxCallerTokenBytes = 4096
	// MaxRunningLimit bounds -max-concurrent.
	MaxRunningLimit = 64
	// reasonCredential: the private key could not be loaded for this run.
	reasonCredential = "credential"
)

// RunFunc runs one registered command. The default dials over SSH; tests
// substitute one.
type RunFunc func(ctx context.Context, command *Command) sshclient.Result

// Options configure a Server.
type Options struct {
	Config  *Config
	Journal *Journal
	// IdempotencyHeader is the header the gateway sends the action's
	// ExecutorKey in (the gateway's -idempotency-header).
	IdempotencyHeader string
	// CallerTokenFile holds the token the gateway presents as
	// "Authorization: Bearer <token>". It is read on every request, so a
	// rotated file takes effect at once.
	CallerTokenFile string
	// ClientAuth admits the gateway by its TLS client certificate: a chain
	// to a configured CA and a pinned identity (#662). With both this and
	// CallerTokenFile, a request must pass both; with neither, NewServer
	// refuses. There is no unauthenticated mode.
	ClientAuth *ClientAuth
	// MaxRunning bounds concurrent runs.
	MaxRunning int
	// AttachWait is how long a repeat of a running key waits for the run
	// before answering in_progress. Keep it below the gateway's
	// -operation-timeout.
	AttachWait time.Duration
	Log        io.Writer
	Run        RunFunc
	Now        func() time.Time
}

// Server is the HTTP handler.
type Server struct {
	opts    Options
	header  string
	runCtx  context.Context
	cancel  context.CancelFunc
	runs    sync.WaitGroup
	logMu   sync.Mutex
	stopped bool
	stopMu  sync.Mutex
}

// NewServer validates the options.
func NewServer(opts Options) (*Server, error) {
	switch {
	case opts.Config == nil || opts.Journal == nil:
		return nil, errors.New("server requires a configuration and a journal")
	case opts.MaxRunning < 1 || opts.MaxRunning > MaxRunningLimit:
		return nil, fmt.Errorf("max concurrent runs must be 1 to %d", MaxRunningLimit)
	case opts.AttachWait <= 0:
		return nil, errors.New("attach wait must be positive")
	case opts.CallerTokenFile == "" && opts.ClientAuth == nil:
		return nil, errors.New("a caller token file or a pinned client certificate " +
			"identity is required; there is no unauthenticated mode")
	}
	if opts.IdempotencyHeader == "" {
		opts.IdempotencyHeader = DefaultIdempotencyHeader
	}
	header := textproto.CanonicalMIMEHeaderKey(opts.IdempotencyHeader)
	if !validHeaderName(opts.IdempotencyHeader) || header == "Authorization" || header == "Host" {
		// net/http moves Host out of the header map, so a key there would
		// never be read.
		return nil, errors.New("idempotency header must be a valid header name " +
			"other than Authorization and Host")
	}
	if opts.CallerTokenFile != "" {
		if _, err := readCallerToken(opts.CallerTokenFile); err != nil {
			return nil, err
		}
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if opts.Run == nil {
		opts.Run = runOverSSH
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{opts: opts, header: header}
	s.runCtx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

func runOverSSH(ctx context.Context, command *Command) sshclient.Result {
	target, err := command.Target()
	if err != nil {
		return sshclient.Result{Outcome: sshclient.OutcomeNotRun, Reason: reasonCredential}
	}
	return sshclient.Run(ctx, target, command.Command, command.Limits())
}

// ServeHTTP answers one request. Every refusal is decided before anything is
// journaled or dialed.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The body is read first and the read deadline cleared: the request is
	// then complete, and a server ReadTimeout shorter than a run must not end
	// the request while the answer is awaited. net/http already clears the
	// deadline for HTTP/1.1 once the body is read; this states it for both
	// protocols (TestReadTimeoutDoesNotCancelARunsAnswer).
	body, bodyOK := readBody(r.Body)
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	answer, line := s.handle(r, body, bodyOK)
	answer.write(w)
	line.Event, line.Status = "request", answer.Status
	s.log(line)
}

type logLine struct {
	Time       string `json:"time"`
	Event      string `json:"event"`
	Command    string `json:"command,omitempty"`
	Key        string `json:"key,omitempty"`
	Status     int    `json:"status,omitempty"`
	Answer     string `json:"answer,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Reference  string `json:"reference,omitempty"`
	Durable    *bool  `json:"durable,omitempty"`
	StdoutSize int64  `json:"stdout_bytes,omitempty"`
	StderrSize int64  `json:"stderr_bytes,omitempty"`
	Millis     int64  `json:"duration_ms,omitempty"`
}

// LogEvent writes one event line through the server's serialized log, for
// the command's own events (a failed certificate reload). event and reason
// are fixed strings the caller chooses; never pass a value read from a file.
func (s *Server) LogEvent(event, reason string) {
	s.log(logLine{Event: event, Reason: reason})
}

// log writes one JSON line. It never carries a key (only its digest), a
// command string, output, a credential or any request header.
func (s *Server) log(line logLine) {
	line.Time = s.opts.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(line)
	if err != nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	_, _ = s.opts.Log.Write(append(encoded, '\n'))
}

func (s *Server) handle(r *http.Request, body []byte, bodyOK bool) (Answer, logLine) {
	var line logLine
	refuse := func(reason string) (Answer, logLine) {
		// A refusal says nothing ran. For a key the journal holds that may
		// be false (the command ran under an earlier attempt, and this one
		// arrives with a rotated credential, a changed header or route), so
		// a known key is answered outcome_unknown instead, which the gateway
		// records as a possible effect and never as a rejection. To a caller
		// without the token it reveals whether a key it presents has an
		// entry, never the result; executor keys are visible on action
		// records, so that is accepted as the price of never answering a
		// rejection for a command that ran.
		if key, known := s.knownKeyIn(r.Header); known {
			line.Key, line.Answer = KeyDigest(key), StateOutcomeUnknown+":"+reason
			return OutcomeUnknownAnswer(false), line
		}
		line.Answer = StateRefused + ":" + reason
		return Refused(reason), line
	}
	if !s.authorized(r) {
		return refuse(RefusedUnauthorized)
	}
	if r.Method != http.MethodPost {
		return refuse(RefusedMethod)
	}
	name, ok := strings.CutPrefix(r.URL.EscapedPath(), PathPrefix)
	if !ok || !ValidCommandName(name) {
		return refuse(RefusedPath)
	}
	line.Command = name
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		return refuse(RefusedQuery)
	}
	if !bodyOK || !noParameters(body) {
		return refuse(RefusedBody)
	}
	key, ok := s.idempotencyKey(r.Header)
	if !ok {
		return refuse(RefusedKey)
	}
	line.Key = KeyDigest(key)

	command, registered := s.opts.Config.Lookup(name)
	// Begin and the run's registration happen under stopMu, so Shutdown's
	// wait covers every run that is ever started.
	//
	// Once stopping, a known key is still answered from the journal (its
	// replay, its run, or unknown): refusing it would read as "nothing ran".
	// A new key is answered busy, which starts nothing, journals nothing and
	// is retried, by which time another instance may be serving.
	s.stopMu.Lock()
	maxRunning := s.opts.MaxRunning
	if s.stopped {
		maxRunning = 0
	}
	begin := s.opts.Journal.Begin(key, Fingerprint(name), name, registered, maxRunning)
	if begin.Kind == BeginRun {
		s.runs.Add(1)
	}
	s.stopMu.Unlock()
	switch begin.Kind {
	case BeginConflict:
		line.Answer = StateKeyConflict
		return KeyConflictAnswer(), line
	case BeginUnregistered:
		return refuse(RefusedUnknownCommand)
	case BeginFull:
		return refuse(RefusedJournalFull)
	case BeginUnavailable:
		return refuse(RefusedJournalUnavailable)
	case BeginBusy:
		line.Answer = StateBusy
		return BusyAnswer(), line
	case BeginFinished:
		line.Answer = string(begin.Entry.Finished.Outcome) + ":replayed"
		return FinishedAnswer(begin.Entry.Finished, true), line
	case BeginUnknown:
		line.Answer = StateOutcomeUnknown
		return OutcomeUnknownAnswer(false), line
	case BeginRunning:
		answer := s.await(r.Context(), key, begin.Done, s.opts.AttachWait, true)
		line.Answer = answerState(answer)
		return answer, line
	case BeginRun:
		s.start(key, command)
		answer := s.await(r.Context(), key, begin.Done, 0, false)
		line.Answer = answerState(answer)
		return answer, line
	}
	return refuse(RefusedJournalUnavailable)
}

func answerState(answer Answer) string {
	var body struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal([]byte(answer.Body), &body)
	return body.State
}

// start runs the command on a context of the server's, not the request's: a
// caller that disconnects does not stop a command that may already be
// running, and the run's end is journaled whoever is waiting.
func (s *Server) start(key string, command *Command) {
	go func() {
		defer s.runs.Done()
		began := s.opts.Now()
		result := s.opts.Run(s.runCtx, command)
		finished := toFinished(result, s.opts.Now().Sub(began))
		entry := s.opts.Journal.Finish(key, finished)
		durable := entry.Finished != nil
		s.log(logLine{Event: "run", Command: command.Name, Key: KeyDigest(key),
			Outcome: string(finished.Outcome), Reason: finished.Reason,
			Reference: finished.Reference, Durable: &durable,
			StdoutSize: finished.StdoutBytes, StderrSize: finished.StderrBytes,
			Millis: finished.DurationMillis})
	}()
}

// await waits for a run to end and answers from the journal. limit > 0 is the
// attach bound for a repeat; the request that started the run waits until
// the run ends or its caller goes away.
func (s *Server) await(ctx context.Context, key string, done <-chan struct{}, limit time.Duration, replayed bool) Answer {
	var expired <-chan time.Time
	if limit > 0 {
		timer := time.NewTimer(limit)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-done:
	case <-expired:
		return InProgressAnswer()
	case <-ctx.Done():
		return InProgressAnswer()
	}
	entry, ok := s.opts.Journal.Lookup(key)
	if !ok || entry.Finished == nil {
		// The run ended and its result is not durable: the command may have
		// run, so the answer is unknown, now and on every repeat.
		return OutcomeUnknownAnswer(false)
	}
	return FinishedAnswer(entry.Finished, replayed)
}

func toFinished(result sshclient.Result, elapsed time.Duration) Finished {
	finished := Finished{
		Reason:          string(result.Reason),
		Stdout:          result.Stdout.Data,
		StdoutBytes:     result.Stdout.Bytes,
		StdoutTruncated: result.Stdout.Truncated,
		Stderr:          result.Stderr.Data,
		StderrBytes:     result.Stderr.Bytes,
		StderrTruncated: result.Stderr.Truncated,
		DurationMillis:  elapsed.Milliseconds(),
	}
	switch result.Outcome {
	case sshclient.OutcomeCompleted:
		finished.Outcome = OutcomeCompleted
		finished.Reference = Reference(result)
		if result.Signal != "" {
			finished.Signal = SignalLabel(result.Signal)
		} else {
			status := max(result.ExitStatus, 0)
			finished.ExitStatus = &status
		}
	case sshclient.OutcomeNotRun:
		finished.Outcome = OutcomeNotRun
	default:
		finished.Outcome = OutcomeUnknown
	}
	return finished
}

// Shutdown waits for runs in flight until ctx ends, then cancels them; a
// cancelled run is journaled as unknown. Call it after the HTTP server has
// stopped accepting requests.
func (s *Server) Shutdown(ctx context.Context) {
	s.stopMu.Lock()
	s.stopped = true
	s.stopMu.Unlock()
	done := make(chan struct{})
	go func() {
		s.runs.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.cancel()
		<-done
	}
	s.cancel()
}

// readBody reads at most maxBody bytes; more is not a v1 request.
func readBody(body io.Reader) ([]byte, bool) {
	if body == nil {
		return nil, true
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxBody+1))
	return raw, err == nil && len(raw) <= maxBody
}

// noParameters reports whether a body is empty or exactly one JSON object
// with no members. v1 takes nothing from the request.
func noParameters(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return true
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil || object == nil || len(object) != 0 {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

// knownKeyIn looks for a key the journal holds anywhere in the request's
// headers, not only the configured one: a resend whose header was renamed
// or duplicated between attempts still carries the key. Authorization is
// skipped, and a value is a key only if it decodes to exactly an executor
// key's length.
func (s *Server) knownKeyIn(header http.Header) (string, bool) {
	for name, values := range header {
		if name == "Authorization" {
			continue
		}
		for _, value := range values {
			for _, part := range strings.Split(value, ",") {
				decoded, err := dispatchapi.ParseExecutorKey(strings.TrimSpace(part))
				if err != nil || len(decoded) != dispatchapi.ExecutorKeyBytes {
					continue
				}
				key := base64.RawURLEncoding.EncodeToString(decoded)
				if _, known := s.opts.Journal.Lookup(key); known {
					return key, true
				}
			}
		}
	}
	return "", false
}

// idempotencyKey reads exactly one key in either spelling the platform emits
// and returns its canonical form, unpadded base64url.
func (s *Server) idempotencyKey(header http.Header) (string, bool) {
	values := header.Values(s.header)
	if len(values) != 1 {
		return "", false
	}
	decoded, err := dispatchapi.ParseExecutorKey(values[0])
	if err != nil || len(decoded) != dispatchapi.ExecutorKeyBytes {
		return "", false
	}
	return base64.RawURLEncoding.EncodeToString(decoded), true
}

// authorized applies every configured factor: the client certificate when
// ClientAuth is set, and the bearer token when CallerTokenFile is set. A
// request passes only if each configured one passes; NewServer guarantees at
// least one is.
func (s *Server) authorized(r *http.Request) bool {
	if s.opts.ClientAuth == nil && s.opts.CallerTokenFile == "" {
		return false
	}
	if s.opts.ClientAuth != nil {
		if reason, ok := s.opts.ClientAuth.Verify(r.TLS); !ok {
			s.log(logLine{Event: "caller_certificate_refused", Reason: reason})
			return false
		}
	}
	if s.opts.CallerTokenFile == "" {
		return true
	}
	return s.tokenAuthorized(r)
}

func (s *Server) tokenAuthorized(r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	token, err := readCallerToken(s.opts.CallerTokenFile)
	if err != nil {
		s.log(logLine{Event: "caller_token_unreadable"})
		return false
	}
	presented := sha256.Sum256([]byte(values[0]))
	expected := sha256.Sum256([]byte("Bearer " + token))
	return subtle.ConstantTimeCompare(presented[:], expected[:]) == 1
}

// readCallerToken reads the token file: one printable token of at least
// MinCallerTokenBytes, surrounding whitespace ignored.
func readCallerToken(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("caller token file cannot be opened")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxCallerTokenBytes+1))
	if err != nil || len(raw) > maxCallerTokenBytes {
		return "", errors.New("caller token file cannot be read or is too large")
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < MinCallerTokenBytes {
		return "", fmt.Errorf("caller token must be at least %d bytes", MinCallerTokenBytes)
	}
	for _, c := range []byte(token) {
		if c <= ' ' || c >= 0x7f {
			return "", errors.New("caller token must be printable ASCII with no spaces")
		}
	}
	return token, nil
}

// validHeaderName applies the RFC 9110 token grammar.
func validHeaderName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}
