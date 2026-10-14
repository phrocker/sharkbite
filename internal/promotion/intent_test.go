package promotion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

type fakeDurablePromoter struct {
	mu            sync.Mutex
	tableID       string
	allocations   int
	submissions   []accumulo.BulkImportFateID
	submitErrOnce error
	waits         int
	finishes      int
}

func (f *fakeDurablePromoter) ResolveTableID(context.Context, string) (string, error) {
	return f.tableID, nil
}
func (*fakeDurablePromoter) AddTableSplitsForTable(context.Context, accumulo.Table, [][]byte) error {
	return nil
}
func (*fakeDurablePromoter) ListTableSplits(context.Context, string) ([][]byte, error) {
	return nil, nil
}
func (*fakeDurablePromoter) BulkImport(context.Context, string, string, accumulo.BulkImportOptions) error {
	panic("durable state machine must not use one-shot BulkImport")
}
func (f *fakeDurablePromoter) AllocateBulkImport(context.Context, string) (accumulo.BulkImportFateID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allocations++
	return accumulo.BulkImportFateID{Type: 1, UUID: "fate-1"}, nil
}
func (f *fakeDurablePromoter) SubmitBulkImport(_ context.Context, id accumulo.BulkImportFateID, _, _, _ string, _ accumulo.BulkImportOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submissions = append(f.submissions, id)
	if f.submitErrOnce != nil {
		err := f.submitErrOnce
		f.submitErrOnce = nil
		return err
	}
	return nil
}
func (f *fakeDurablePromoter) WaitBulkImport(context.Context, string, accumulo.BulkImportFateID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waits++
	return "SUCCESS", nil
}
func (f *fakeDurablePromoter) FinishBulkImport(context.Context, string, accumulo.BulkImportFateID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishes++
	return nil
}

type fakeAuthority struct {
	mu         sync.Mutex
	fences     int
	freezes    int
	verifies   int
	retires    int
	activates  int
	fanins     int
	verifyHook func(*Intent)
}

func (f *fakeAuthority) FenceDestination(_ context.Context, intent *Intent) (AuthorityToken, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fences++
	return AuthorityToken{Domain: "accumulo", Epoch: intent.SourceAuthority.Epoch + 1, Generation: "lock-2", Attempt: "attempt-2"}, "42", nil
}
func (f *fakeAuthority) FreezeSource(context.Context, *Intent) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freezes++
	return "checkpoint-7", nil
}
func (f *fakeAuthority) VerifyImport(_ context.Context, intent *Intent, _ LoadMapping) error {
	f.mu.Lock()
	f.verifies++
	hook := f.verifyHook
	f.mu.Unlock()
	if hook != nil {
		hook(intent)
	}
	return nil
}
func (f *fakeAuthority) RetireSource(context.Context, *Intent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retires++
	return nil
}
func (f *fakeAuthority) ActivateDestination(context.Context, *Intent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activates++
	return nil
}
func (f *fakeAuthority) CompleteFanIn(context.Context, *Intent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fanins++
	return nil
}

func durableFixture(t *testing.T, mode Mode) (*Machine, Request, *fakeDurablePromoter, *fakeAuthority) {
	t.Helper()
	data, file := testRFile(t, 0, "export/events/t-0000/F0001.rf", []byte("value"))
	src := memory.New()
	src.Put(file.DestinationPath, data)
	manifest := &engine.RFileExportManifest{
		Version: engine.RFileExportManifestVersion, SourceTable: "events",
		Tablets: []engine.RFileExportTablet{{Index: 0}},
		RFiles:  []engine.RFileExportFile{file},
	}
	req := Request{
		Mode: mode, ProducerID: "agent-a", SourceGeneration: 7,
		SourceAuthority:  AuthorityToken{Domain: "embedded", Epoch: 4, Generation: "manifest-7", Attempt: "writer-a"},
		DestinationTable: "events", Manifest: manifest,
	}
	if mode == ModeFanIn {
		req.ParentPromotionID = "parent-cutover"
	}
	hash, err := strictPromotionPreflight(manifest)
	if err != nil {
		t.Fatal(err)
	}
	req.ID = deterministicIntentID(req, hash)
	req.BulkDir = "hdfs://nn/promotions/" + req.ID
	promoter := &fakeDurablePromoter{tableID: "42"}
	authority := &fakeAuthority{}
	return &Machine{
		Store: NewMemoryIntentStore(), Authority: authority, Promoter: promoter,
		Source: src, Stage: memory.New(),
	}, req, promoter, authority
}

func TestDurablePromotionRecoversAmbiguousSubmitUsingSameFateID(t *testing.T) {
	machine, req, promoter, authority := durableFixture(t, ModeCutover)
	promoter.submitErrOnce = errors.New("timeout after manager accepted execute")

	first, err := machine.Run(context.Background(), req)
	if err == nil {
		t.Fatal("first run error = nil, want ambiguous submit error")
	}
	if first == nil {
		t.Fatalf("first intent = nil after ambiguous submit: %v", err)
	}
	if first.State != StateFateAllocated || first.SubmissionAttempts != 1 {
		t.Fatalf("first state = %s attempts=%d", first.State, first.SubmissionAttempts)
	}

	final, err := machine.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != StateAccumuloWritable || !final.FateFinished || !final.CleanupComplete {
		t.Fatalf("final intent = %#v", final)
	}
	if promoter.allocations != 1 {
		t.Fatalf("FATE allocations = %d, want 1", promoter.allocations)
	}
	if len(promoter.submissions) != 2 || promoter.submissions[0] != promoter.submissions[1] {
		t.Fatalf("submissions = %#v, want same persisted FATE ID twice", promoter.submissions)
	}
	if authority.retires != 1 || authority.activates != 1 {
		t.Fatalf("retire/activate = %d/%d, want 1/1", authority.retires, authority.activates)
	}
}

func TestDurablePromotionConcurrentRetriesDeduplicateSideEffects(t *testing.T) {
	machine, req, promoter, authority := durableFixture(t, ModeCutover)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			intent, err := machine.Run(context.Background(), req)
			if err == nil && intent.State != StateAccumuloWritable {
				err = errors.New("non-terminal state")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if promoter.allocations != 1 || len(promoter.submissions) != 1 || promoter.waits != 1 || promoter.finishes != 1 {
		t.Fatalf("FATE lifecycle allocations/submits/waits/finishes = %d/%d/%d/%d", promoter.allocations, len(promoter.submissions), promoter.waits, promoter.finishes)
	}
	if authority.fences != 1 || authority.freezes != 1 || authority.retires != 1 || authority.activates != 1 {
		t.Fatalf("authority lifecycle = fence %d freeze %d retire %d activate %d", authority.fences, authority.freezes, authority.retires, authority.activates)
	}
}

func TestPromotionStrictPreflightRejectsParquetAndDerivedBeforeSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name, format, role, path string
	}{
		{"parquet", engine.ExportFormatParquet, engine.ExportRoleDerived, "export/events/F0001.parquet"},
		{"derived rfile", engine.ExportFormatRFile, engine.ExportRoleDerived, "export/events/F0001.rf"},
		{"mixed extension", engine.ExportFormatRFile, engine.ExportRoleAuthoritative, "export/events/F0001.parquet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine, req, promoter, authority := durableFixture(t, ModeCutover)
			req.Manifest.RFiles[0].Format, req.Manifest.RFiles[0].Role = tc.format, tc.role
			req.Manifest.RFiles[0].DestinationPath = tc.path
			if _, err := machine.Run(context.Background(), req); err == nil {
				t.Fatal("Run error = nil, want strict RFile preflight rejection")
			}
			if promoter.allocations != 0 || authority.fences != 0 {
				t.Fatalf("side effects occurred: allocations=%d fences=%d", promoter.allocations, authority.fences)
			}
		})
	}
}

func TestFanInCompletesWithoutRetiringOrActivatingAuthority(t *testing.T) {
	machine, req, _, authority := durableFixture(t, ModeFanIn)
	final, err := machine.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if final.State != StateAccumuloWritable || authority.fanins != 1 {
		t.Fatalf("state/fanins = %s/%d", final.State, authority.fanins)
	}
	if authority.retires != 0 || authority.activates != 0 {
		t.Fatalf("fan-in retired/activated authority = %d/%d", authority.retires, authority.activates)
	}
}

func TestCleanupNeverDeletesConcurrentlyReplacedDestination(t *testing.T) {
	machine, req, _, authority := durableFixture(t, ModeCutover)
	stage := machine.Stage.(*memory.Backend)
	authority.verifyHook = func(intent *Intent) {
		stage.Put(intent.Staged[0].Path, []byte("concurrent-owner"))
	}
	final, err := machine.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := stage.Open(context.Background(), final.Staged[0].Path)
	if err != nil {
		t.Fatalf("concurrent replacement was deleted: %v", err)
	}
	_ = data.Close()
}

func TestLocalIntentStorePersistsAndRejectsStaleCAS(t *testing.T) {
	store, err := NewLocalIntentStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	intent := &Intent{ID: "intent-1", Revision: 1, State: StateHandoffIntent}
	if err := store.Create(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	next := cloneIntent(intent)
	next.Revision, next.State = 2, StateDestinationFenced
	if err := store.CompareAndSwap(context.Background(), intent.ID, 1, next); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwap(context.Background(), intent.ID, 1, next); !errors.Is(err, ErrIntentConflict) {
		t.Fatalf("stale CAS error = %v, want conflict", err)
	}
	reopened, err := NewLocalIntentStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Load(context.Background(), intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 || got.State != StateDestinationFenced {
		t.Fatalf("reloaded intent = %#v", got)
	}
}

func TestLocalIntentStoreRejectsCommittedCorruptionButIgnoresCrashTail(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalIntentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	intent := &Intent{ID: "intent-1", Revision: 1, State: StateHandoffIntent}
	if err := store.Create(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, "intent-1.jsonl")
	f, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"incomplete"`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := store.Load(context.Background(), intent.ID); err != nil {
		t.Fatalf("truncated crash tail should preserve prior revision: %v", err)
	}
	if err := os.WriteFile(journal, append([]byte("{bad}\n"), []byte(`{"incomplete"`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), intent.ID); err == nil {
		t.Fatal("committed corrupt journal line was silently ignored")
	}
}
