/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package authorized

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// catalogSnapshot is every entry under a directory with its mode, size,
// modification time and (for a file) its bytes, so a comparison catches any
// write: a rewritten file with the same bytes still moves its mtime, and a
// created or removed entry moves its directory's.
func catalogSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := snapshotDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// snapshotDirectory is catalogSnapshot for a goroutine that may not call
// t.Fatal.
func snapshotDirectory(dir string) ([]string, error) {
	var entries []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %v %d %d", path, info.Mode(), info.Size(),
			info.ModTime().UnixNano())
		if !entry.IsDir() {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			line += " " + string(content)
		}
		entries = append(entries, line)
		return nil
	})
	return entries, err
}

func requireUnchanged(t *testing.T, dir string, before []string) {
	t.Helper()
	if after := catalogSnapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("the policy directory changed:\n%q\n->\n%q", before, after)
	}
}

// closedCatalog returns a directory holding a cleanly closed catalog with a
// label migration record of version.
func closedCatalog(t *testing.T, version uint32) string {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutLabelMigration(context.Background(),
		LabelMigrationRecord{Version: version}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func requireMigration(t *testing.T, store *DurablePolicyStore, version uint32) {
	t.Helper()
	record, ok, err := store.LabelMigration(context.Background())
	if err != nil || !ok || record.Version != version {
		t.Fatalf("label migration = %+v %v %v, want version %d", record, ok, err, version)
	}
}

func requireAlreadyOpen(t *testing.T, store *DurablePolicyStore, err error) {
	t.Helper()
	if err == nil {
		_ = store.Close()
		t.Fatal("a second opener was not refused")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
		!strings.Contains(err.Error(), "is already open") {
		t.Fatalf("second opener = %v, want unavailable: already open", err)
	}
}

// TestDurablePolicyStoreOwnsItsDirectory pins #686 within one process: while
// a store is open, neither a second store nor an offline listing opens the
// directory (the listing even while the WAL is empty, which its unflushed
// write check alone lets through), the refusals write nothing, and Close
// hands the directory on.
func TestDurablePolicyStoreOwnsItsDirectory(t *testing.T) {
	dir := closedCatalog(t, 1)
	live, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := catalogSnapshot(t, dir)
	second, err := OpenDurablePolicyStore(dir)
	requireAlreadyOpen(t, second, err)
	listing, err := OpenExistingDurablePolicyStore(dir)
	requireAlreadyOpen(t, listing, err)
	requireUnchanged(t, dir, before)
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	listing, err = OpenExistingDurablePolicyStore(dir)
	if err != nil {
		t.Fatalf("listing after the owner closed = %v", err)
	}
	requireMigration(t, listing, 1)
	// The listing owns the directory too, so a workspace cannot start under
	// it and write what the listing would then flush.
	second, err = OpenDurablePolicyStore(dir)
	requireAlreadyOpen(t, second, err)
	if err := listing.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatalf("open after the listing closed = %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenExistingDurablePolicyStoreWritesNothing pins that a listing of a
// cleanly closed catalog, open through Close, leaves every byte, mode and
// modification time under the directory as it was: no WAL replay, flush,
// manifest rewrite or truncation.
func TestOpenExistingDurablePolicyStoreWritesNothing(t *testing.T) {
	dir := closedCatalog(t, 3)
	before := catalogSnapshot(t, dir)
	for range 2 {
		store, err := OpenExistingDurablePolicyStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		requireMigration(t, store, 3)
		if _, err := store.Declassifications(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		requireUnchanged(t, dir, before)
	}
}

// TestOpenExistingDurablePolicyStoreRefusesWithoutWriting pins that every
// directory the listing refuses is left as it was, the lock file included:
// the checks run before the lock is taken.
func TestOpenExistingDurablePolicyStoreRefusesWithoutWriting(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := OpenExistingDurablePolicyStore(missing); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("missing directory = %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("the listing created the directory: %v", err)
	}
	bare := t.TempDir()
	if err := os.Mkdir(filepath.Join(bare, policyTable), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{t.TempDir(), bare} {
		before := catalogSnapshot(t, dir)
		if store, err := OpenExistingDurablePolicyStore(dir); err == nil {
			_ = store.Close()
			t.Fatalf("%s opened", dir)
		}
		requireUnchanged(t, dir, before)
	}
	// A catalog last closed by a version that took no lock has no lock
	// file. The listing does not create one: it refuses.
	unlocked := closedCatalog(t, 1)
	if err := os.Remove(filepath.Join(unlocked, policyLockFile)); err != nil {
		t.Fatal(err)
	}
	before := catalogSnapshot(t, unlocked)
	store, err := OpenExistingDurablePolicyStore(unlocked)
	if err == nil {
		_ = store.Close()
		t.Fatal("a catalog without a lock file opened")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
		!strings.Contains(err.Error(), "has no "+policyLockFile) {
		t.Fatalf("listing a catalog without a lock file = %v", err)
	}
	requireUnchanged(t, unlocked, before)
	// A workspace start creates it, after which the catalog lists.
	started, err := OpenDurablePolicyStore(unlocked)
	if err != nil {
		t.Fatal(err)
	}
	if err := started.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenExistingDurablePolicyStore(unlocked)
	if err != nil {
		t.Fatal(err)
	}
	requireMigration(t, store, 1)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenExistingDurablePolicyStoreRechecksUnderTheLock pins the second
// round of checks: a writer that opens the catalog after the unlocked checks
// passed, writes, and dies (its lock released, its WAL unflushed) before the
// listing takes the lock is still refused, and nothing is written.
func TestOpenExistingDurablePolicyStoreRechecksUnderTheLock(t *testing.T) {
	dir := closedCatalog(t, 1)
	var writer *DurablePolicyStore
	openExistingBeforeLock = func() {
		var err error
		writer, err = OpenDurablePolicyStore(dir)
		if err != nil {
			t.Error(err)
			return
		}
		if err := writer.PutLabelMigration(context.Background(),
			LabelMigrationRecord{Version: 2}); err != nil {
			t.Error(err)
		}
		// Die without flushing: the kernel drops the lock, the WAL stays.
		if err := writer.lock.Close(); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { openExistingBeforeLock = func() {} })
	before := catalogSnapshot(t, dir)
	store, err := OpenExistingDurablePolicyStore(dir)
	openExistingBeforeLock = func() {}
	if writer == nil {
		t.FailNow()
	}
	defer func() {
		_ = writer.engine.Close()
		writer.closed = true
	}()
	if err == nil {
		_ = store.Close()
		t.Fatal("the listing opened a catalog with unflushed writes")
	}
	if !strings.Contains(err.Error(), "unflushed writes") {
		t.Fatalf("listing = %v, want unflushed writes", err)
	}
	if reflect.DeepEqual(before, catalogSnapshot(t, dir)) {
		t.Fatal("the writer wrote nothing; the fixture is broken")
	}
	walBefore := catalogSnapshot(t, dir)
	if _, err := OpenExistingDurablePolicyStore(dir); err == nil {
		t.Fatal("a second listing opened the catalog")
	}
	requireUnchanged(t, dir, walBefore)
}

// TestDurablePolicyStoreReleasesItsLockWhenOpenFails pins that a refused
// open (a corrupt record) leaves the directory openable: the next attempt
// reports the corruption again, not a held lock.
func TestDurablePolicyStoreReleasesItsLockWhenOpenFails(t *testing.T) {
	dir := closedCatalog(t, 1)
	corruptPolicyRow(t, dir, []byte(policyRowLabelMigration), []byte("not a record"))
	for _, open := range []func(string) (*DurablePolicyStore, error){
		OpenDurablePolicyStore, OpenDurablePolicyStore,
		OpenExistingDurablePolicyStore, OpenDurablePolicyStore,
	} {
		store, err := open(dir)
		if err == nil {
			_ = store.Close()
			t.Fatal("a corrupt catalog opened")
		}
		if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
			t.Fatalf("open = %v, want the corruption", err)
		}
	}
}

// holderEnv names the catalog directory a holder process opens.
const holderEnv = "SHOAL_POLICY_CATALOG_HOLDER_DIR"

// TestPolicyCatalogHolderProcess is not a test: run by startHolder, it opens
// the catalog as a live workspace does and follows commands on stdin. "write
// N" persists label migration version N (to the WAL, unflushed) and "close"
// closes the store; each command is acknowledged with "ok".
func TestPolicyCatalogHolderProcess(t *testing.T) {
	dir := os.Getenv(holderEnv)
	if dir == "" {
		return
	}
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println("ok")
	commands := bufio.NewScanner(os.Stdin)
	for commands.Scan() {
		command := commands.Text()
		if command == "close" {
			if err := store.Close(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(3)
			}
		} else if version, ok := strings.CutPrefix(command, "write "); ok {
			parsed, err := strconv.ParseUint(version, 10, 32)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(4)
			}
			if err := store.PutLabelMigration(context.Background(),
				LabelMigrationRecord{Version: uint32(parsed)}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(5)
			}
		} else {
			fmt.Fprintln(os.Stderr, "unknown command", command)
			os.Exit(6)
		}
		fmt.Println("ok")
	}
	os.Exit(0)
}

type holder struct {
	t       *testing.T
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stderr  *bytes.Buffer
}

// startHolder runs TestPolicyCatalogHolderProcess on dir in a child process
// and waits until it holds the catalog.
func startHolder(t *testing.T, dir string) *holder {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestPolicyCatalogHolderProcess$")
	command.Env = append(os.Environ(), holderEnv+"="+dir)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	h := &holder{t: t, command: command, stdin: stdin,
		stdout: bufio.NewReader(stdout), stderr: &stderr}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	h.ack()
	return h
}

func (h *holder) ack() {
	h.t.Helper()
	line, err := h.stdout.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ok" {
		_ = h.command.Process.Kill()
		_ = h.command.Wait()
		h.t.Fatalf("holder = %q %v, stderr %q", line, err, h.stderr.String())
	}
}

func (h *holder) send(command string) {
	h.t.Helper()
	if _, err := fmt.Fprintln(h.stdin, command); err != nil {
		h.t.Fatal(err)
	}
	h.ack()
}

// crash kills the holder outright, as SIGKILL or a power cut would: nothing
// is flushed and nothing releases the lock but the kernel.
func (h *holder) crash() {
	h.t.Helper()
	if err := h.command.Process.Kill(); err != nil {
		h.t.Fatal(err)
	}
	_ = h.command.Wait()
}

// TestOfflineListingRefusesALiveWorkspace is #686's scenario across
// processes: a live workspace, idle with an empty WAL, then writing. The
// listing is refused both times and writes nothing, and neither does a second
// workspace's start; once the workspace stops, the listing reads all it wrote.
func TestOfflineListingRefusesALiveWorkspace(t *testing.T) {
	dir := closedCatalog(t, 1)
	live := startHolder(t, dir)
	for _, version := range []uint32{0, 2} {
		if version != 0 {
			live.send(fmt.Sprintf("write %d", version))
		}
		before := catalogSnapshot(t, dir)
		listing, err := OpenExistingDurablePolicyStore(dir)
		if err == nil {
			_ = listing.Close()
			t.Fatalf("the listing opened a live catalog (write %d)", version)
		}
		if version == 0 && !strings.Contains(err.Error(), "is already open") {
			t.Fatalf("listing an idle live catalog = %v, want already open", err)
		}
		second, err := OpenDurablePolicyStore(dir)
		requireAlreadyOpen(t, second, err)
		requireUnchanged(t, dir, before)
	}
	live.send("close")
	listing, err := OpenExistingDurablePolicyStore(dir)
	if err != nil {
		t.Fatalf("listing a stopped catalog = %v", err)
	}
	requireMigration(t, listing, 2)
	if err := listing.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCrashedHolderDoesNotWedgeTheCatalog pins that the lock file a killed
// process leaves behind blocks nobody: the kernel released its lock. An idle
// holder's catalog lists at once; a holder that died with unflushed writes is
// refused by the listing (the WAL check) but reopens for the workspace, which
// replays the writes, and then lists.
func TestCrashedHolderDoesNotWedgeTheCatalog(t *testing.T) {
	dir := closedCatalog(t, 1)
	startHolder(t, dir).crash()
	if _, err := os.Stat(filepath.Join(dir, policyLockFile)); err != nil {
		t.Fatalf("no lock file left behind: %v", err)
	}
	listing, err := OpenExistingDurablePolicyStore(dir)
	if err != nil {
		t.Fatalf("listing after an idle holder crashed = %v", err)
	}
	requireMigration(t, listing, 1)
	if err := listing.Close(); err != nil {
		t.Fatal(err)
	}

	holder := startHolder(t, dir)
	holder.send("write 5")
	holder.crash()
	before := catalogSnapshot(t, dir)
	if listing, err := OpenExistingDurablePolicyStore(dir); err == nil ||
		!strings.Contains(err.Error(), "unflushed writes") {
		if err == nil {
			_ = listing.Close()
		}
		t.Fatalf("listing after a writing holder crashed = %v, want unflushed writes", err)
	}
	requireUnchanged(t, dir, before)
	restarted, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatalf("restart after a crash = %v", err)
	}
	requireMigration(t, restarted, 5)
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	listing, err = OpenExistingDurablePolicyStore(dir)
	if err != nil {
		t.Fatalf("listing after the restart closed = %v", err)
	}
	requireMigration(t, listing, 5)
	if err := listing.Close(); err != nil {
		t.Fatal(err)
	}
}

// opened is what the racing opener of TestPolicyCatalogLockOutlivesTheFlush
// saw.
type opened struct {
	store    *DurablePolicyStore
	snapshot []string
	err      error
}

// TestPolicyCatalogLockOutlivesTheFlush pins Close's order: the lock is
// released only after the engine has flushed and closed. An opener spinning
// on the lock while the owner closes over a full WAL must find the directory
// exactly as the finished Close leaves it; were the lock released first, it
// would open (replaying the WAL) in the middle of the flush.
func TestPolicyCatalogLockOutlivesTheFlush(t *testing.T) {
	for attempt := range 5 {
		dir := closedCatalog(t, 1)
		owner, err := OpenDurablePolicyStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		for version := range uint32(100) {
			if err := owner.PutLabelMigration(context.Background(),
				LabelMigrationRecord{Version: version + 2}); err != nil {
				t.Fatal(err)
			}
		}
		spinning := make(chan struct{})
		result := make(chan opened, 1)
		go func() {
			for first := true; ; first = false {
				store, err := OpenDurablePolicyStore(dir)
				if first {
					close(spinning)
				}
				if err == nil {
					snapshot, err := snapshotDirectory(dir)
					result <- opened{store: store, snapshot: snapshot, err: err}
					return
				}
				if !strings.Contains(err.Error(), "is already open") {
					result <- opened{err: err}
					return
				}
			}
		}()
		<-spinning
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
		second := <-result
		if second.err != nil {
			if second.store != nil {
				_ = second.store.Close()
			}
			t.Fatalf("attempt %d: the racing opener = %v", attempt, second.err)
		}
		closed := catalogSnapshot(t, dir)
		requireMigration(t, second.store, 101)
		if err := second.store.Close(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(second.snapshot, closed) {
			t.Fatalf("attempt %d: the racing opener saw the directory mid-close:\n%q\nclosed:\n%q",
				attempt, second.snapshot, closed)
		}
	}
}

// TestPolicyCatalogLockFailureNamesItsCause pins that a lock file this user
// may not open is reported with its cause, and not as a running workspace to
// stop.
func TestPolicyCatalogLockFailureNamesItsCause(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions that bind the caller")
	}
	dir := closedCatalog(t, 1)
	lockFile := filepath.Join(dir, policyLockFile)
	if err := os.Chmod(lockFile, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockFile, 0o600) })
	for name, open := range map[string]func(string) (*DurablePolicyStore, error){
		"workspace": OpenDurablePolicyStore,
		"listing":   OpenExistingDurablePolicyStore,
	} {
		store, err := open(dir)
		if err == nil {
			_ = store.Close()
			t.Fatalf("%s opened over an unopenable lock file", name)
		}
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
			strings.Contains(err.Error(), "already open") ||
			!strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("%s = %v, want the permission error itself", name, err)
		}
	}
}
