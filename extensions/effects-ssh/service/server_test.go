// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/extensions/effects-ssh/sshclient"
)

// TestAnswersAreThePinnedBytes: every constructor writes exactly its fixture.
// The gateway's test classifies the same fixtures with the shipped route.
func TestAnswersAreThePinnedBytes(t *testing.T) {
	answers := loadAnswers(t)
	built := map[string]Answer{
		"completed":                       Completed("exit-0", false),
		"completed_replayed":              Completed("exit-0", true),
		"completed_exit_status_truncated": Completed("exit-3-truncated", false),
		"completed_signal":                Completed("signal-KILL", false),
		"outcome_unknown":                 OutcomeUnknownAnswer(false),
		"outcome_unknown_replayed":        OutcomeUnknownAnswer(true),
		"in_progress":                     InProgressAnswer(),
		"busy":                            BusyAnswer(),
		"not_run":                         NotRunAnswer(false),
		"not_run_replayed":                NotRunAnswer(true),
		"key_conflict":                    KeyConflictAnswer(),
	}
	for reason := range refusalStatus {
		built["refused_"+reason] = Refused(reason)
	}
	if len(built) != len(answers) {
		t.Fatalf("%d answers built, %d pinned", len(built), len(answers))
	}
	for name, answer := range built {
		fixture, ok := answers[name]
		if !ok {
			t.Errorf("%s is not pinned", name)
			continue
		}
		if answer.Status != fixture.Status || answer.Body != fixture.Body || len(answer.Header) != len(fixture.Header) {
			t.Errorf("%s: built %d %q %v, pinned %d %q %v", name, answer.Status, answer.Body,
				answer.Header, fixture.Status, fixture.Body, fixture.Header)
			continue
		}
		for header, value := range fixture.Header {
			if answer.Header[header] != value {
				t.Errorf("%s: header %s = %q, pinned %q", name, header, answer.Header[header], value)
			}
		}
		// Every body is JSON whose state is one of the closed set, and only
		// the completed answer is ever 2xx.
		var body struct{ State string }
		if err := json.Unmarshal([]byte(answer.Body), &body); err != nil {
			t.Errorf("%s: body is not JSON", name)
		}
		if (answer.Status/100 == 2) != (body.State == StateCompleted) {
			t.Errorf("%s: status %d with state %s", name, answer.Status, body.State)
		}
		// The done marker the route's conflict rule matches is never sent
		// under the conflict status: a 409 is always outcome unknown.
		if answer.Status == http.StatusConflict && body.State == StateCompleted {
			t.Errorf("%s: 409 carries the done marker", name)
		}
	}
}

// TestRouteFixtureMatchesTheContract: the route the gateway is configured
// with uses this service's path, reference pattern and statuses.
func TestRouteFixtureMatchesTheContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "contract", "route.json"))
	if err != nil {
		t.Fatal(err)
	}
	var routes []struct {
		Path        string   `json:"path"`
		Idempotency string   `json:"idempotency"`
		Effects     []string `json:"effects"`
		Conflict    struct {
			Status  []int    `json:"status"`
			Pointer string   `json:"pointer"`
			Equals  []string `json:"equals"`
		} `json:"conflict"`
		Retryable []int `json:"retryable"`
		Reference struct {
			Pointer string `json:"pointer"`
			Pattern string `json:"pattern"`
		} `json:"reference"`
	}
	if err := json.Unmarshal(raw, &routes); err != nil || len(routes) != 1 {
		t.Fatalf("route fixture: %v", err)
	}
	route := routes[0]
	name, ok := strings.CutPrefix(route.Path, PathPrefix)
	if !ok || !ValidCommandName(name) || route.Idempotency != "key" {
		t.Fatalf("route %+v", route)
	}
	if len(route.Conflict.Status) != 1 || route.Conflict.Status[0] != http.StatusConflict ||
		route.Conflict.Pointer != "/state" || len(route.Conflict.Equals) != 1 ||
		route.Conflict.Equals[0] != StateCompleted {
		t.Fatalf("conflict %+v", route.Conflict)
	}
	if len(route.Retryable) != 1 || route.Retryable[0] != http.StatusServiceUnavailable {
		t.Fatalf("retryable %v", route.Retryable)
	}
	if route.Reference.Pointer != "/reference" || route.Reference.Pattern != ReferencePattern {
		t.Fatalf("reference %+v", route.Reference)
	}
}

// TestDocumentedRouteIsTheFixture: the route docs/effects-ssh.md tells an
// operator to configure is byte for byte the one both sides test.
func TestDocumentedRouteIsTheFixture(t *testing.T) {
	route, err := os.ReadFile(filepath.Join("..", "testdata", "contract", "route.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "effects-ssh.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "```json\n"+string(route)+"```\n") {
		t.Fatal("docs/effects-ssh.md does not carry testdata/contract/route.json verbatim")
	}
}

func TestReferenceRendering(t *testing.T) {
	for _, c := range []struct {
		result sshclient.Result
		want   string
	}{
		{sshclient.Result{Outcome: sshclient.OutcomeCompleted}, "exit-0"},
		{sshclient.Result{Outcome: sshclient.OutcomeCompleted, ExitStatus: 255}, "exit-255"},
		{sshclient.Result{Outcome: sshclient.OutcomeCompleted, ExitStatus: 4294967295}, "exit-4294967295"},
		{sshclient.Result{Outcome: sshclient.OutcomeCompleted, Signal: "TERM",
			Stderr: sshclient.Output{Truncated: true}}, "signal-TERM-truncated"},
		{sshclient.Result{Outcome: sshclient.OutcomeCompleted, Signal: "kill\"; x"}, "signal-OTHER"},
		{sshclient.Result{Outcome: sshclient.OutcomeCompleted, Signal: strings.Repeat("A", 17)}, "signal-OTHER"},
	} {
		got := Reference(c.result)
		if got != c.want || !validReference(got) {
			t.Errorf("Reference(%+v) = %q, want %q", c.result, got, c.want)
		}
	}
}

// TestRunOnceAndReplay: a new key runs the command once and answers its
// exit status; every repeat replays it and runs nothing.
func TestRunOnceAndReplay(t *testing.T) {
	e := newEnv(t)
	answers := loadAnswers(t)
	key := testKey("a1")
	e.post("restart-web", key).is(t, answers, "completed")
	for i := 0; i < 3; i++ {
		e.post("restart-web", key).is(t, answers, "completed_replayed")
	}
	// The padded standard spelling of the same key is the same key.
	decoded, _ := base64.RawURLEncoding.DecodeString(key)
	e.post("restart-web", base64.StdEncoding.EncodeToString(decoded)).is(t, answers, "completed_replayed")
	if n := e.host.ssh.Execs("systemctl restart web"); n != 1 {
		t.Fatalf("ran %d times", n)
	}
	// And after a restart.
	e.restart(JournalOptions{Retention: time.Hour, MaxEntries: 100})
	e.post("restart-web", key).is(t, answers, "completed_replayed")
	if n := e.host.ssh.Execs("systemctl restart web"); n != 1 {
		t.Fatalf("ran %d times after restart", n)
	}
	// The journal holds the bounded output for the operator.
	records, torn, err := ReadJournal(e.dir)
	if err != nil || torn || len(records) != 3 || records[2].Finished == nil ||
		string(records[2].Finished.Stdout) != "restarted\n" || *records[2].Finished.ExitStatus != 0 {
		t.Fatalf("journal %+v torn %v err %v", records, torn, err)
	}
	if strings.Contains(e.logs.String(), key) || strings.Contains(e.logs.String(), "restarted") ||
		strings.Contains(e.logs.String(), testToken) || strings.Contains(e.logs.String(), "systemctl") {
		t.Fatalf("the log carries a key, output, the token or a command string:\n%s", e.logs.String())
	}
}

func TestExitStatusSignalAndTruncation(t *testing.T) {
	e := newEnv(t)
	answers := loadAnswers(t)
	e.post("loud", testKey("l")).is(t, answers, "completed_exit_status_truncated")
	e.post("die", testKey("d")).is(t, answers, "completed_signal")
	reply := e.post("weird", testKey("w"))
	if reply.status != 200 || reply.body != `{"state":"completed","reference":"signal-OTHER"}` {
		t.Fatalf("weird signal: %d %s", reply.status, reply.body)
	}
	records, _, _ := ReadJournal(e.dir)
	for _, record := range records {
		if record.Finished == nil || record.Command != "" {
			continue
		}
		if record.Finished.Reference == "exit-3-truncated" &&
			(len(record.Finished.Stdout) != 16 || record.Finished.StdoutBytes != 100 ||
				!record.Finished.StdoutTruncated || string(record.Finished.Stderr) != "bad") {
			t.Fatalf("loud record %+v", record.Finished)
		}
	}
}

// TestNotRunIsReplayed: a refused host key opens no session; the answer is
// not_run, and it is final for the key.
func TestNotRunIsReplayed(t *testing.T) {
	e := newEnv(t)
	answers := loadAnswers(t)
	key := testKey("n")
	e.post("wrong-host", key).is(t, answers, "not_run")
	e.post("wrong-host", key).is(t, answers, "not_run_replayed")
	if e.host.ssh.TotalExecs() != 0 {
		t.Fatal("a command ran on a host whose key is not pinned")
	}
}

// TestUnknownIsFinal: a run that times out, or ends with no exit status, is
// outcome unknown, now and on every repeat; it never runs again.
func TestUnknownIsFinal(t *testing.T) {
	e := newEnv(t)
	answers := loadAnswers(t)
	for _, name := range []string{"hang", "quiet"} {
		key := testKey(name)
		e.post(name, key).is(t, answers, "outcome_unknown")
		e.post(name, key).is(t, answers, "outcome_unknown_replayed")
	}
	if e.host.ssh.Execs("hang") != 1 || e.host.ssh.Execs("quiet") != 1 {
		t.Fatalf("hang ran %d, quiet ran %d", e.host.ssh.Execs("hang"), e.host.ssh.Execs("quiet"))
	}
}

// TestRefusalsHappenBeforeAnyConnection: an unregistered command, a missing
// or wrong credential, a wrong method, path, query, body or key: each is
// refused with nothing journaled and no connection opened.
func TestRefusalsHappenBeforeAnyConnection(t *testing.T) {
	e := newEnv(t)
	answers := loadAnswers(t)
	key := testKey("r")
	e.post("not-registered", key).is(t, answers, "refused_unknown_command")
	for name, headers := range map[string]map[string][]string{
		"no credential":    {"Authorization": nil},
		"wrong credential": {"Authorization": {"Bearer " + strings.Repeat("x", len(testToken))}},
		"token, no scheme": {"Authorization": {testToken}},
		"two credentials":  {"Authorization": {"Bearer " + testToken, "Bearer " + testToken}},
		"basic":            {"Authorization": {"Basic " + testToken}},
		"lowercase scheme": {"Authorization": {"bearer " + testToken}},
		"longer token":     {"Authorization": {"Bearer " + testToken + "x"}},
	} {
		reply := e.do(http.MethodPost, PathPrefix+"restart-web", key, "", headers)
		if reply.status != http.StatusUnauthorized {
			t.Fatalf("%s: %d", name, reply.status)
		}
		reply.is(t, answers, "refused_unauthorized")
	}
	// Unauthenticated requests learn nothing, not even whether a name exists.
	e.do(http.MethodPost, PathPrefix+"not-registered", key, "", map[string][]string{"Authorization": nil}).
		is(t, answers, "refused_unauthorized")
	e.do(http.MethodGet, PathPrefix+"restart-web", key, "", nil).is(t, answers, "refused_method_not_allowed")
	e.do(http.MethodPut, PathPrefix+"restart-web", key, "", nil).is(t, answers, "refused_method_not_allowed")
	for _, path := range []string{"/v1/commands/", "/v1/commands/a/b", "/v1/commands/..",
		"/v1/commands/restart%2Dweb", "/v1/command/restart-web", "/", "/v1/commands/-x"} {
		e.do(http.MethodPost, path, key, "", nil).is(t, answers, "refused_not_found")
	}
	e.do(http.MethodPost, PathPrefix+"restart-web?x=1", key, "", nil).is(t, answers, "refused_query_not_allowed")
	e.do(http.MethodPost, PathPrefix+"restart-web?", key, "", nil).is(t, answers, "refused_query_not_allowed")
	for _, body := range []string{`{"a":1}`, `[]`, `null`, `{}{}`, `{} x`, `"{}"`, `{"":0}`,
		`{` + strings.Repeat(" ", 2000) + `}`} {
		e.do(http.MethodPost, PathPrefix+"restart-web", key, body, nil).is(t, answers, "refused_body_not_allowed")
	}
	for name, headers := range map[string]map[string][]string{
		"missing":    {"Idempotency-Key": nil},
		"two":        {"Idempotency-Key": {key, key}},
		"short":      {"Idempotency-Key": {key[:20]}},
		"not b64":    {"Idempotency-Key": {strings.Repeat("!", 43)}},
		"padded url": {"Idempotency-Key": {key + "="}},
	} {
		reply := e.do(http.MethodPost, PathPrefix+"restart-web", "", "", headers)
		if reply.status != http.StatusBadRequest {
			t.Fatalf("key %s: %d", name, reply.status)
		}
		reply.is(t, answers, "refused_idempotency_key_invalid")
	}
	if accepts := e.host.ssh.Accepts(); accepts != 0 {
		t.Fatalf("%d connections were opened for refused requests", accepts)
	}
	if e.journal.Len() != 0 {
		t.Fatalf("%d keys journaled for refused requests", e.journal.Len())
	}
	// The accepted spellings of "no parameters" run.
	for i, body := range []string{"", "{}", " {\n} "} {
		e.do(http.MethodPost, PathPrefix+"restart-web", testKey("ok"+string(rune('a'+i))), body, nil).
			is(t, answers, "completed")
	}
}

// TestUnreadableTokenRefusesEverything: a token file that disappears or is
// emptied refuses every request; it never falls open.
func TestUnreadableTokenRefusesEverything(t *testing.T) {
	e := newEnv(t)
	answers := loadAnswers(t)
	if err := os.WriteFile(e.tokenFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	e.do(http.MethodPost, PathPrefix+"restart-web", testKey("t"), "",
		map[string][]string{"Authorization": {"Bearer "}}).is(t, answers, "refused_unauthorized")
	e.post("restart-web", testKey("t")).is(t, answers, "refused_unauthorized")
	_ = os.Remove(e.tokenFile)
	e.post("restart-web", testKey("t")).is(t, answers, "refused_unauthorized")
	if e.host.ssh.Accepts() != 0 {
		t.Fatal("a connection was opened")
	}
	// Rotation: a new token takes effect without a restart.
	rotated := strings.Repeat("r", MinCallerTokenBytes)
	_ = os.WriteFile(e.tokenFile, []byte(rotated), 0o600)
	e.do(http.MethodPost, PathPrefix+"restart-web", testKey("t"), "",
		map[string][]string{"Authorization": {"Bearer " + rotated}}).is(t, answers, "completed")
}

// TestKnownKeyIsNeverRefused: a resend of a key whose command ran, refused
// for its credential, header, method, path, query or body, is answered
// outcome_unknown, never a refusal that would read as "nothing ran".
func TestKnownKeyIsNeverRefused(t *testing.T) {
	e := newEnv(t)
	answers := loadAnswers(t)
	key := testKey("ran")
	e.post("restart-web", key).is(t, answers, "completed")
	path := PathPrefix + "restart-web"
	for name, request := range map[string]func() reply{
		"rotated token": func() reply {
			return e.do(http.MethodPost, path, key, "", map[string][]string{"Authorization": {"Bearer " + strings.Repeat("z", 40)}})
		},
		"no token": func() reply {
			return e.do(http.MethodPost, path, key, "", map[string][]string{"Authorization": nil})
		},
		"method":   func() reply { return e.do(http.MethodPut, path, key, "", nil) },
		"bad path": func() reply { return e.do(http.MethodPost, "/v2/commands/restart-web", key, "", nil) },
		"query":    func() reply { return e.do(http.MethodPost, path+"?x=1", key, "", nil) },
		"body":     func() reply { return e.do(http.MethodPost, path, key, `{"a":1}`, nil) },
		"renamed header": func() reply {
			return e.do(http.MethodPost, path, "", "", map[string][]string{"X-Executor-Key": {key}})
		},
		"duplicated header": func() reply {
			return e.do(http.MethodPost, path, "", "", map[string][]string{"Idempotency-Key": {key, key}})
		},
		"joined header": func() reply {
			return e.do(http.MethodPost, path, "", "", map[string][]string{"Idempotency-Key": {key + ", " + key}})
		},
	} {
		got := request()
		if got.status != http.StatusConflict {
			t.Fatalf("%s: %d %s", name, got.status, got.body)
		}
		got.is(t, answers, "outcome_unknown")
	}
	// An unknown key is still refused.
	e.do(http.MethodPost, path, testKey("new"), "", map[string][]string{"Authorization": nil}).
		is(t, answers, "refused_unauthorized")
	if n := e.host.ssh.Execs("systemctl restart web"); n != 1 {
		t.Fatalf("ran %d times", n)
	}
}

// TestKeyConflictIsRefusedAndRunsNothing: a key first used for one command
// is refused for another; the first's result is untouched.
func TestKeyConflictIsRefusedAndRunsNothing(t *testing.T) {
	e := newEnv(t)
	answers := loadAnswers(t)
	key := testKey("k")
	e.post("restart-web", key).is(t, answers, "completed")
	e.post("restart-web2", key).is(t, answers, "key_conflict")
	e.post("not-registered", key).is(t, answers, "key_conflict")
	e.post("restart-web", key).is(t, answers, "completed_replayed")
	if n := e.host.ssh.Execs("systemctl restart web"); n != 1 {
		t.Fatalf("ran %d times", n)
	}
}

// TestKnownKeyAnswersAfterItsCommandIsRemoved: the journal answers a known
// key whatever the configuration now says; a command removed after it ran
// replays, it is not "unknown command" (which would read as never ran).
func TestKnownKeyAnswersAfterItsCommandIsRemoved(t *testing.T) {
	e := newEnv(t)
	answers := loadAnswers(t)
	key := testKey("removed")
	e.post("restart-web", key).is(t, answers, "completed")
	raw, _ := json.Marshal(map[string]any{"commands": map[string]any{
		"die": commandJSON(e.host, "die", "5s", nil)}})
	config, err := ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	e.options.Config = config
	e.restart(JournalOptions{Retention: time.Hour, MaxEntries: 100})
	e.post("restart-web", key).is(t, answers, "completed_replayed")
	e.post("restart-web", testKey("new")).is(t, answers, "refused_unknown_command")
}

// TestRepeatAttachesToARunningCommand: a repeat of a key this process is
// running waits for it and answers its result; it does not run it again.
// Past the attach wait it answers in_progress.
func TestRepeatAttachesToARunningCommand(t *testing.T) {
	e := newEnv(t, func(o *Options, _ *JournalOptions) { o.AttachWait = 300 * time.Millisecond })
	answers := loadAnswers(t)
	key := testKey("b")
	first := make(chan reply, 1)
	go func() { first <- e.post("block", key) }()
	<-e.host.entered
	e.post("block", key).is(t, answers, "in_progress")
	second := make(chan reply, 1)
	go func() { second <- e.post("block", key) }()
	time.Sleep(50 * time.Millisecond)
	close(e.host.release)
	(<-first).is(t, answers, "completed")
	(<-second).is(t, answers, "completed_replayed")
	if n := e.host.ssh.Execs("block"); n != 1 {
		t.Fatalf("ran %d times", n)
	}
}

// TestCallerDisconnectDoesNotStopTheRun: the gateway gives up waiting; the
// command runs to its end and is journaled, and the repeat replays it.
func TestCallerDisconnectDoesNotStopTheRun(t *testing.T) {
	e := newEnv(t)
	answers := loadAnswers(t)
	key := testKey("disconnect")
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.http.URL+PathPrefix+"block", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Idempotency-Key", key)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if response, err := e.http.Client().Do(request); err == nil {
			response.Body.Close()
		}
	}()
	<-e.host.entered
	cancel()
	<-done
	close(e.host.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		entry, _ := e.journal.Lookup(key)
		if entry.Finished != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.post("block", key).is(t, answers, "completed_replayed")
	if n := e.host.ssh.Execs("block"); n != 1 {
		t.Fatalf("ran %d times", n)
	}
}

// TestReadTimeoutDoesNotCancelARunsAnswer: the binary's server has a
// ReadTimeout far shorter than a run; the request that started the run still
// gets its result, not in_progress.
func TestReadTimeoutDoesNotCancelARunsAnswer(t *testing.T) {
	e := newEnv(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: e.server, ReadTimeout: 100 * time.Millisecond, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	e.http.URL = "http://" + listener.Addr().String()
	first := make(chan reply, 1)
	go func() { first <- e.post("block", testKey("slow")) }()
	<-e.host.entered
	time.Sleep(400 * time.Millisecond)
	close(e.host.release)
	(<-first).is(t, loadAnswers(t), "completed")
}

// TestBusyJournalsNothing: past the concurrency bound a new key is answered
// busy with nothing journaled, so its retry is a first attempt.
func TestBusyJournalsNothing(t *testing.T) {
	e := newEnv(t, func(o *Options, _ *JournalOptions) { o.MaxRunning = 1 })
	answers := loadAnswers(t)
	first := make(chan reply, 1)
	go func() { first <- e.post("block", testKey("x")) }()
	<-e.host.entered
	e.post("restart-web", testKey("y")).is(t, answers, "busy")
	if _, known := e.journal.Lookup(testKey("y")); known {
		t.Fatal("a busy key was journaled")
	}
	close(e.host.release)
	(<-first).is(t, answers, "completed")
	e.post("restart-web", testKey("y")).is(t, answers, "completed")
}

// TestJournalFullRefusesNewKeysOnly: at the entry bound a new key is refused
// and known keys still answer.
func TestJournalFullRefusesNewKeysOnly(t *testing.T) {
	e := newEnv(t, func(_ *Options, j *JournalOptions) { j.MaxEntries = 1 })
	answers := loadAnswers(t)
	e.post("restart-web", testKey("1")).is(t, answers, "completed")
	e.post("restart-web", testKey("2")).is(t, answers, "refused_journal_full")
	e.post("restart-web", testKey("1")).is(t, answers, "completed_replayed")
	if n := e.host.ssh.Execs("systemctl restart web"); n != 1 {
		t.Fatalf("ran %d times", n)
	}
}

// TestUnloadableKeyIsNotRun: a private key file that vanished after start is
// a run that opened nothing.
func TestUnloadableKeyIsNotRun(t *testing.T) {
	host := newFakeHost(t)
	keyCopy := filepath.Join(t.TempDir(), "key")
	raw, _ := os.ReadFile(host.ssh.ClientKeyFile)
	_ = os.WriteFile(keyCopy, raw, 0o600)
	configRaw, _ := json.Marshal(map[string]any{"commands": map[string]any{
		"restart-web": commandJSON(host, "systemctl restart web", "5s", map[string]any{"private_key_file": keyCopy})}})
	config, err := ParseConfig(configRaw)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(o *Options, _ *JournalOptions) { o.Config = config })
	_ = os.Remove(keyCopy)
	e.post("restart-web", testKey("gone")).is(t, loadAnswers(t), "not_run")
	if host.ssh.Accepts() != 0 {
		t.Fatal("dialed without a key")
	}
}

// TestShutdownJournalsCancelledRunsAsUnknown: a run cut off by the shutdown
// grace is recorded unknown, and answers so after the restart.
func TestShutdownJournalsCancelledRunsAsUnknown(t *testing.T) {
	e := newEnv(t)
	answers := loadAnswers(t)
	key := testKey("shutdown")
	first := make(chan reply, 1)
	go func() { first <- e.post("block", key) }()
	<-e.host.entered
	e.server.Shutdown(expiredContext())
	(<-first).is(t, answers, "outcome_unknown")
	entry, _ := e.journal.Lookup(key)
	if entry.Finished == nil || entry.Finished.Outcome != OutcomeUnknown || entry.Finished.Reason != string(sshclient.ReasonCanceled) {
		t.Fatalf("entry %+v", entry.Finished)
	}
	e.restart(JournalOptions{Retention: time.Hour, MaxEntries: 100})
	e.post("block", key).is(t, answers, "outcome_unknown_replayed")
	if n := e.host.ssh.Execs("block"); n != 1 {
		t.Fatalf("ran %d times", n)
	}
}

// TestNoRunStartsAfterShutdown: once Shutdown has begun, a new key is
// answered busy and nothing is dialed or journaled, so Shutdown's wait covers
// every run; a known key is still answered from the journal, never with a
// refusal that would read as "nothing ran".
func TestNoRunStartsAfterShutdown(t *testing.T) {
	e := newEnv(t)
	answers := loadAnswers(t)
	e.post("restart-web", testKey("done")).is(t, answers, "completed")
	e.post("hang", testKey("unknown")).is(t, answers, "outcome_unknown")
	accepts := e.host.ssh.Accepts()
	e.server.Shutdown(expiredContext())
	e.post("restart-web", testKey("late")).is(t, answers, "busy")
	e.post("restart-web", testKey("done")).is(t, answers, "completed_replayed")
	e.post("hang", testKey("unknown")).is(t, answers, "outcome_unknown_replayed")
	if e.host.ssh.Accepts() != accepts || e.journal.Len() != 2 {
		t.Fatal("a run started after shutdown")
	}
}

// TestRunningKeyDuringShutdownIsNotRefused: a repeat of a key still running
// in the shutdown grace attaches to it.
func TestRunningKeyDuringShutdownIsNotRefused(t *testing.T) {
	e := newEnv(t, func(o *Options, _ *JournalOptions) { o.AttachWait = 200 * time.Millisecond })
	answers := loadAnswers(t)
	key := testKey("grace")
	first := make(chan reply, 1)
	go func() { first <- e.post("block", key) }()
	<-e.host.entered
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stopped := make(chan struct{})
	go func() { e.server.Shutdown(ctx); close(stopped) }()
	for !e.server.isStopped() {
		time.Sleep(5 * time.Millisecond)
	}
	e.post("block", key).is(t, answers, "in_progress")
	close(e.host.release)
	(<-first).is(t, answers, "completed")
	<-stopped
	e.post("block", key).is(t, answers, "completed_replayed")
}

func TestNewServerRefusals(t *testing.T) {
	e := newEnv(t)
	base := e.options
	for name, mutate := range map[string]func(*Options){
		"no token file":      func(o *Options) { o.CallerTokenFile = "" },
		"missing token file": func(o *Options) { o.CallerTokenFile = filepath.Join(t.TempDir(), "x") },
		"short token": func(o *Options) {
			path := filepath.Join(t.TempDir(), "short")
			_ = os.WriteFile(path, []byte("short"), 0o600)
			o.CallerTokenFile = path
		},
		"spaced token": func(o *Options) {
			path := filepath.Join(t.TempDir(), "spaced")
			_ = os.WriteFile(path, []byte(strings.Repeat("a", 20)+" "+strings.Repeat("b", 20)), 0o600)
			o.CallerTokenFile = path
		},
		"no journal":         func(o *Options) { o.Journal = nil },
		"zero concurrency":   func(o *Options) { o.MaxRunning = 0 },
		"no attach wait":     func(o *Options) { o.AttachWait = 0 },
		"auth header as key": func(o *Options) { o.IdempotencyHeader = "authorization" },
		"bad header":         func(o *Options) { o.IdempotencyHeader = "Idem Key" },
		"host header":        func(o *Options) { o.IdempotencyHeader = "host" },
	} {
		options := base
		mutate(&options)
		if _, err := NewServer(options); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
