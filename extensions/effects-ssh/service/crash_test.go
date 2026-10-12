// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package service

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The crash test kills a real process with SIGKILL while its command runs,
// between the durable started record and the finished one, and restarts on
// the same journal. The SSH host is this test process's in-process server on
// 127.0.0.1; the child dials it.

const helperEnv = "EFFECTS_SSH_CRASH_HELPER"

// TestCrashHelper is the child: it serves the journal in $helperEnv until it
// is killed. It does nothing when run as an ordinary test.
func TestCrashHelper(t *testing.T) {
	dir := os.Getenv(helperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(filepath.Join(dir, "journal"), JournalOptions{Retention: time.Hour, MaxEntries: 100})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Options{Config: config, Journal: journal,
		CallerTokenFile: filepath.Join(dir, "token"), MaxRunning: 4, AttachWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "addr.tmp"), []byte(listener.Addr().String()), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Rename(filepath.Join(dir, "addr.tmp"), filepath.Join(dir, "addr"))
	_ = http.Serve(listener, server)
}

// TestKillBetweenStartedAndFinishedNeverRunsAgain: kill -9 mid-command;
// after the restart the repeat answers outcome_unknown and runs nothing.
func TestKillBetweenStartedAndFinishedNeverRunsAgain(t *testing.T) {
	host := newFakeHost(t)
	dir := t.TempDir()
	journalDir := filepath.Join(dir, "journal")
	if err := os.Mkdir(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := InitJournal(journalDir, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), testConfigJSON(t, host), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(testToken), 0o600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.v")
	child.Env = append(os.Environ(), helperEnv+"="+dir)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() })
	var address string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if raw, err := os.ReadFile(filepath.Join(dir, "addr")); err == nil {
			address = string(raw)
			break
		}
	}
	if address == "" {
		t.Fatal("the helper did not start")
	}
	key := testKey("crash")
	sent := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, "http://"+address+PathPrefix+"block", nil)
		request.Header.Set("Authorization", "Bearer "+testToken)
		request.Header.Set("Idempotency-Key", key)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			response.Body.Close()
		}
		sent <- err
	}()
	select {
	case command := <-host.entered:
		if command != "block" {
			t.Fatalf("ran %q", command)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the command did not start")
	}
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_, _ = child.Process.Wait()
	if err := <-sent; err == nil {
		t.Fatal("the killed process answered")
	}
	records, torn, err := ReadJournal(journalDir)
	if err != nil || torn || len(records) != 2 || records[1].Op != recordStarted {
		t.Fatalf("journal after the kill: %+v torn %v err %v", records, torn, err)
	}

	// Restart on the same journal, in this process.
	e := &env{t: t, host: host, dir: journalDir, tokenFile: filepath.Join(dir, "token"), logs: &lockedBuffer{}}
	config, err := ParseConfig(testConfigJSON(t, host))
	if err != nil {
		t.Fatal(err)
	}
	e.options = Options{Config: config, CallerTokenFile: e.tokenFile, MaxRunning: 4,
		AttachWait: time.Second, Log: e.logs}
	e.open(JournalOptions{Retention: time.Hour, MaxEntries: 100})
	answers := loadAnswers(t)
	for i := 0; i < 3; i++ {
		e.post("block", key).is(t, answers, "outcome_unknown")
	}
	close(host.release)
	if n := host.ssh.Execs("block"); n != 1 {
		t.Fatalf("the command ran %d times", n)
	}
	// Other keys run normally.
	e.post("restart-web", testKey("after")).is(t, answers, "completed")
	if !strings.Contains(e.logs.String(), `"answer":"outcome_unknown"`) {
		t.Fatalf("log: %s", e.logs.String())
	}
}
