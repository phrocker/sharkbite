package ingestservice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	clientgen "github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletingest"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

type fakeAuth struct {
	mu          sync.Mutex
	deniedTable string
}

func (a *fakeAuth) Authenticate(_ context.Context, credentials *security.TCredentials) error {
	if credentials == nil || credentials.Principal != "writer" {
		return errors.New("denied")
	}
	return nil
}

func (a *fakeAuth) AuthorizeWrite(_ context.Context, _ *security.TCredentials, table string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if table == a.deniedTable {
		return ErrPermissionDenied
	}
	return nil
}

type fakeDirectory struct {
	mu      sync.Mutex
	tablets map[string]*fakeTablet
	errs    map[string]error
}

func (d *fakeDirectory) Lookup(_ context.Context, extent ingestrouter.Extent) (ingestrouter.HostedTablet, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[extent.Key()]; err != nil {
		return nil, err
	}
	tablet := d.tablets[extent.Key()]
	if tablet == nil {
		return nil, ingestrouter.ErrNotHosted
	}
	return tablet, nil
}

type fakeTablet struct {
	mu      sync.Mutex
	extent  ingestrouter.Extent
	fence   ingestrouter.Fence
	commits []ingestrouter.CommitRequest
	err     error
	active  []ingestrouter.Cell
}

func (t *fakeTablet) ConditionalCommit(
	ctx context.Context,
	request ingestrouter.CommitRequest,
	evaluate ingestrouter.ConditionalEvaluator,
) (bool, error) {
	accepted, err := evaluate(ctx, t.active)
	if err != nil || !accepted {
		return accepted, err
	}
	return true, t.Commit(ctx, request)
}

type fakeConditionalReader struct {
	cells []ingestrouter.Cell
	err   error
}

func (r fakeConditionalReader) ReadConditionalRow(
	context.Context,
	*security.TCredentials,
	ingestrouter.Extent,
	[]byte,
	[][]byte,
) ([]ingestrouter.Cell, error) {
	return append([]ingestrouter.Cell(nil), r.cells...), r.err
}

func (t *fakeTablet) Extent() ingestrouter.Extent { return t.extent }
func (t *fakeTablet) Fence() ingestrouter.Fence   { return t.fence }
func (t *fakeTablet) Authority() ingestrouter.CommitAuthority {
	return ingestrouter.AuthorityAccumuloWAL
}
func (t *fakeTablet) Commit(_ context.Context, request ingestrouter.CommitRequest) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.commits = append(t.commits, request)
	return t.err
}

func newTestService(t *testing.T, cfg func(*Config), directory *fakeDirectory) *Service {
	t.Helper()
	router, err := ingestrouter.New(directory, ingestrouter.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	options := Config{Router: router, Authenticator: &fakeAuth{}}
	if cfg != nil {
		cfg(&options)
	}
	service, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testCredentials() *security.TCredentials {
	return &security.TCredentials{Principal: "writer", InstanceId: "iid", Token: []byte("token")}
}

func testExtent(table string, end string) *data.TKeyExtent {
	return &data.TKeyExtent{Table: []byte(table), EndRow: []byte(end)}
}

func testMutation(t *testing.T, row string) *data.TMutation {
	t.Helper()
	mutation, _ := cclient.NewMutation([]byte(row))
	mutation.PutLatest([]byte("cf"), []byte("cq"), []byte("A"), []byte("value"))
	wire, err := mutation.ToThrift()
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestUpdateSessionAppliesAndCloses(t *testing.T) {
	extent := ingestrouter.Extent{TableID: "1", EndRow: []byte("z")}
	tablet := &fakeTablet{
		extent: extent,
		fence:  ingestrouter.Fence{ServerGeneration: "s", ManagerGeneration: "m", Assignment: 1},
	}
	service := newTestService(t, nil, &fakeDirectory{
		tablets: map[string]*fakeTablet{extent.Key(): tablet}, errs: make(map[string]error),
	})
	id, err := service.StartUpdate(context.Background(), nil, testCredentials(), tabletingest.TDurability_SYNC)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ApplyUpdates(context.Background(), nil, id, testExtent("1", "z"),
		[]*data.TMutation{testMutation(t, "a")}); err != nil {
		t.Fatal(err)
	}
	result, err := service.CloseUpdate(context.Background(), nil, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FailedExtents) != 0 || len(result.ViolationSummaries) != 0 {
		t.Fatalf("close result = %#v", result)
	}
	tablet.mu.Lock()
	defer tablet.mu.Unlock()
	if len(tablet.commits) != 1 {
		t.Fatalf("commits = %#v", tablet.commits)
	}
	if tablet.commits[0].Mutations[0].Updates[0].Timestamp.Set {
		t.Fatal("PutLatest became an explicit timestamp before tablet authority")
	}
}

func TestUpdateSessionReportsRetryAndPartialFailure(t *testing.T) {
	first := ingestrouter.Extent{TableID: "1", EndRow: []byte("m")}
	second := ingestrouter.Extent{TableID: "1", PrevEndRow: []byte("m"), EndRow: []byte("z")}
	directory := &fakeDirectory{
		tablets: map[string]*fakeTablet{
			first.Key(): {
				extent: first,
				fence:  ingestrouter.Fence{ServerGeneration: "s", ManagerGeneration: "m", Assignment: 1},
			},
		},
		errs: map[string]error{
			second.Key(): &ingestrouter.RouteError{
				Cause: ingestrouter.ErrStaleExtent,
				RetryExtents: []ingestrouter.Extent{
					{TableID: "1", PrevEndRow: []byte("m"), EndRow: []byte("t")},
					{TableID: "1", PrevEndRow: []byte("t"), EndRow: []byte("z")},
				},
			},
		},
	}
	service := newTestService(t, nil, directory)
	id, _ := service.StartUpdate(context.Background(), nil, testCredentials(), tabletingest.TDurability_LOG)
	_ = service.ApplyUpdates(context.Background(), nil, id, testExtent("1", "m"),
		[]*data.TMutation{testMutation(t, "a")})
	secondWire := &data.TKeyExtent{Table: []byte("1"), PrevEndRow: []byte("m"), EndRow: []byte("z")}
	_ = service.ApplyUpdates(context.Background(), nil, id, secondWire,
		[]*data.TMutation{testMutation(t, "x")})
	result, err := service.CloseUpdate(context.Background(), nil, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FailedExtents) != 1 {
		t.Fatalf("failed extents = %#v, want only the submitted stale extent", result.FailedExtents)
	}
	for _, committed := range result.FailedExtents {
		if committed != 0 {
			t.Fatalf("committed prefix = %d, want 0", committed)
		}
	}
	if metrics := service.Metrics(); metrics.AppliedBatches != 1 || metrics.RetriedBatches != 1 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestUpdateSessionReportsCommittedPrefixForLaterExtentFailure(t *testing.T) {
	extent := ingestrouter.Extent{TableID: "1", EndRow: []byte("z")}
	tablet := &fakeTablet{
		extent: extent,
		fence:  ingestrouter.Fence{ServerGeneration: "s", ManagerGeneration: "m", Assignment: 1},
	}
	service := newTestService(t, nil, &fakeDirectory{
		tablets: map[string]*fakeTablet{extent.Key(): tablet},
		errs:    map[string]error{},
	})
	id, _ := service.StartUpdate(context.Background(), nil, testCredentials(), tabletingest.TDurability_LOG)
	wireExtent := testExtent("1", "z")
	_ = service.ApplyUpdates(context.Background(), nil, id, wireExtent,
		[]*data.TMutation{testMutation(t, "a"), testMutation(t, "b")})
	tablet.err = ingestrouter.ErrRetryable
	_ = service.ApplyUpdates(context.Background(), nil, id, wireExtent,
		[]*data.TMutation{testMutation(t, "c")})
	result, err := service.CloseUpdate(context.Background(), nil, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, committed := range result.FailedExtents {
		if committed != 2 {
			t.Fatalf("committed prefix = %d, want 2", committed)
		}
	}
}

func TestUnsupportedModesAndConditionalFailExplicitly(t *testing.T) {
	service := newTestService(t, nil, &fakeDirectory{
		tablets: make(map[string]*fakeTablet), errs: make(map[string]error),
	})
	_, err := service.StartUpdate(context.Background(), nil, testCredentials(), tabletingest.TDurability_NONE)
	var securityErr *clientgen.ThriftSecurityException
	if !errors.As(err, &securityErr) || securityErr.Code != clientgen.SecurityErrorCode_UNSUPPORTED_OPERATION {
		t.Fatalf("NONE error = %v", err)
	}
	_, err = service.StartConditionalUpdate(context.Background(), nil, testCredentials(), nil, "1",
		tabletingest.TDurability_SYNC, "")
	if !errors.As(err, &securityErr) || securityErr.Code != clientgen.SecurityErrorCode_UNSUPPORTED_OPERATION {
		t.Fatalf("conditional error = %v", err)
	}
	_, err = service.ConditionalUpdate(context.Background(), nil, 1, nil, nil)
	var noSession *tabletserver.NoSuchScanIDException
	if !errors.As(err, &noSession) {
		t.Fatalf("conditional update error = %v", err)
	}
}

func TestConditionalUpdateAcceptsRejectsAndCachesResults(t *testing.T) {
	extent := ingestrouter.Extent{TableID: "1", EndRow: []byte("z")}
	tablet := &fakeTablet{
		extent: extent,
		fence:  ingestrouter.Fence{ServerGeneration: "s", ManagerGeneration: "m", Assignment: 1},
	}
	service := newTestService(t, func(cfg *Config) {
		cfg.ConditionalReader = fakeConditionalReader{cells: []ingestrouter.Cell{{
			Row: []byte("a"), ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"),
			Value: []byte("old"), Timestamp: 7,
		}}}
		cfg.TserverLock = func() string { return "lock" }
	}, &fakeDirectory{
		tablets: map[string]*fakeTablet{extent.Key(): tablet}, errs: map[string]error{},
	})
	session, err := service.StartConditionalUpdate(
		context.Background(), nil, testCredentials(), nil, "1",
		tabletingest.TDurability_SYNC, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	accepted := &data.TConditionalMutation{
		ID: 11, Mutation: testMutation(t, "a"),
		Conditions: []*data.TCondition{{
			Cf: []byte("cf"), Cq: []byte("cq"), Val: []byte("old"),
		}},
	}
	rejected := &data.TConditionalMutation{
		ID: 12, Mutation: testMutation(t, "a"),
		Conditions: []*data.TCondition{{
			Cf: []byte("cf"), Cq: []byte("cq"), Val: []byte("wrong"),
		}},
	}
	batch := data.CMBatch{testExtent("1", "z"): {accepted, rejected}}
	results, err := service.ConditionalUpdate(
		context.Background(), nil, data.UpdateID(session.SessionId), batch, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != data.TCMStatus_ACCEPTED ||
		results[1].Status != data.TCMStatus_REJECTED {
		t.Fatalf("results = %#v", results)
	}
	results, err = service.ConditionalUpdate(
		context.Background(), nil, data.UpdateID(session.SessionId),
		data.CMBatch{testExtent("1", "z"): {accepted}}, nil,
	)
	if err != nil || len(results) != 1 || results[0].Status != data.TCMStatus_ACCEPTED {
		t.Fatalf("cached result = %#v, %v", results, err)
	}
	replacementMutation, _ := cclient.NewMutation([]byte("a"))
	replacementMutation.PutLatest(
		[]byte("cf"), []byte("cq2"), []byte("A"), []byte("replacement"),
	)
	replacementWire, err := replacementMutation.ToThrift()
	if err != nil {
		t.Fatal(err)
	}
	reusedID := &data.TConditionalMutation{
		ID: 11, Mutation: replacementWire,
		Conditions: []*data.TCondition{{
			Cf: []byte("cf"), Cq: []byte("cq"), Val: []byte("old"),
		}},
	}
	results, err = service.ConditionalUpdate(
		context.Background(), nil, data.UpdateID(session.SessionId),
		data.CMBatch{testExtent("1", "z"): {reusedID}}, nil,
	)
	if err != nil || len(results) != 1 || results[0].Status != data.TCMStatus_ACCEPTED {
		t.Fatalf("reused mutation ID result = %#v, %v", results, err)
	}
	results, err = service.ConditionalUpdate(
		context.Background(), nil, data.UpdateID(session.SessionId),
		data.CMBatch{testExtent("1", "z"): {accepted}}, nil,
	)
	if err != nil || len(results) != 1 || results[0].Status != data.TCMStatus_ACCEPTED {
		t.Fatalf("original cached result after ID reuse = %#v, %v", results, err)
	}
	tablet.mu.Lock()
	defer tablet.mu.Unlock()
	if len(tablet.commits) != 2 {
		t.Fatalf("commits = %d, want 2", len(tablet.commits))
	}
}

func TestConditionsMatchActiveTombstone(t *testing.T) {
	condition := &data.TCondition{
		Cf: []byte("cf"), Cq: []byte("cq"), Val: nil, Iterators: []byte{0},
	}
	cells := []ingestrouter.Cell{
		{Row: []byte("r"), ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"), Timestamp: 1, Value: []byte("v")},
		{Row: []byte("r"), ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"), Timestamp: 2, Deleted: true},
	}
	matched, err := conditionsMatch([]byte("r"), []*data.TCondition{condition}, cells, nil)
	if err != nil || !matched {
		t.Fatal("latest tombstone did not satisfy absent-value condition")
	}
}

func TestConditionsMatchSetEncodingIterator(t *testing.T) {
	symbols := []string{"set", setEncodingIteratorClass, "concat.value", "true"}
	iterators := []byte{1, 0, 1, 10, 1, 2, 3}
	var expected bytes.Buffer
	entry := []byte("session\x00tserver:9997")
	if err := binary.Write(&expected, binary.BigEndian, int32(len(entry))); err != nil {
		t.Fatal(err)
	}
	expected.Write(entry)
	if err := binary.Write(&expected, binary.BigEndian, int32(1)); err != nil {
		t.Fatal(err)
	}
	condition := &data.TCondition{
		Cf: []byte("future"), Val: expected.Bytes(), Iterators: iterators,
	}
	cells := []ingestrouter.Cell{{
		Row: []byte("r"), ColumnFamily: []byte("future"),
		ColumnQualifier: []byte("session"), Value: []byte("tserver:9997"), Timestamp: 1,
	}}
	matched, err := conditionsMatch([]byte("r"), []*data.TCondition{condition}, cells, symbols)
	if err != nil || !matched {
		t.Fatalf("set condition matched=%v err=%v", matched, err)
	}
}

func TestConditionsMatchSetEncodingIteratorEmptyFamily(t *testing.T) {
	symbols := []string{"set", setEncodingIteratorClass, "concat.value", "false"}
	condition := &data.TCondition{
		Cf: []byte("future"), Val: []byte{0, 0, 0, 0},
		Iterators: []byte{1, 0, 1, 10, 1, 2, 3},
	}
	matched, err := conditionsMatch([]byte("r"), []*data.TCondition{condition}, nil, symbols)
	if err != nil || !matched {
		t.Fatalf("empty set condition matched=%v err=%v", matched, err)
	}
}

func TestConditionsMatchRowExistsIterator(t *testing.T) {
	symbols := []string{"rowExists", rowExistsIteratorClass}
	condition := &data.TCondition{
		Iterators: []byte{1, 0, 1, 10, 0},
	}
	matched, err := conditionsMatch([]byte("r"), []*data.TCondition{condition}, nil, symbols)
	if err != nil || !matched {
		t.Fatalf("absent row matched=%v err=%v", matched, err)
	}
	matched, err = conditionsMatch([]byte("r"), []*data.TCondition{condition}, []ingestrouter.Cell{{
		Row: []byte("r"), ColumnFamily: []byte("tx"), ColumnQualifier: []byte("status"),
		Value: []byte("NEW"), Timestamp: 1,
	}}, symbols)
	if err != nil || matched {
		t.Fatalf("existing row matched=%v err=%v", matched, err)
	}
}

func TestConditionsMatchStatusMappingIterator(t *testing.T) {
	symbols := []string{"status", statusMappingIteratorClass, "statusSet", "NEW,IN_PROGRESS"}
	condition := &data.TCondition{
		Cf: []byte("tx"), Cq: []byte("status"), Val: []byte("present"),
		Iterators: []byte{1, 0, 1, 100, 1, 2, 3},
	}
	cells := []ingestrouter.Cell{{
		Row: []byte("r"), ColumnFamily: []byte("tx"), ColumnQualifier: []byte("status"),
		Value: []byte("IN_PROGRESS"), Timestamp: 1,
	}}
	matched, err := conditionsMatch([]byte("r"), []*data.TCondition{condition}, cells, symbols)
	if err != nil || !matched {
		t.Fatalf("accepted status matched=%v err=%v", matched, err)
	}
	cells[0].Value = []byte("FAILED")
	matched, err = conditionsMatch([]byte("r"), []*data.TCondition{condition}, cells, symbols)
	if err != nil || matched {
		t.Fatalf("rejected status matched=%v err=%v", matched, err)
	}
}

func TestWriteAuthorizationFailureIsReportedSeparately(t *testing.T) {
	router, err := ingestrouter.New(&fakeDirectory{
		tablets: make(map[string]*fakeTablet), errs: make(map[string]error),
	}, ingestrouter.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(Config{
		Router: router, Authenticator: &fakeAuth{deniedTable: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := service.StartUpdate(context.Background(), nil, testCredentials(), tabletingest.TDurability_SYNC)
	if err != nil {
		t.Fatal(err)
	}
	extent := testExtent("1", "z")
	_ = service.ApplyUpdates(context.Background(), nil, id, extent,
		[]*data.TMutation{testMutation(t, "a")})
	result, err := service.CloseUpdate(context.Background(), nil, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.AuthorizationFailures) != 1 || len(result.FailedExtents) != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestSessionExpiryBackpressureAndDrain(t *testing.T) {
	now := time.Unix(100, 0)
	service := newTestService(t, func(cfg *Config) {
		cfg.MaxSessions = 1
		cfg.SessionTTL = time.Second
		cfg.Now = func() time.Time { return now }
	}, &fakeDirectory{tablets: make(map[string]*fakeTablet), errs: make(map[string]error)})
	first, err := service.StartUpdate(context.Background(), nil, testCredentials(), tabletingest.TDurability_SYNC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartUpdate(context.Background(), nil, testCredentials(), tabletingest.TDurability_SYNC); err == nil {
		t.Fatal("second session should encounter backpressure")
	}
	now = now.Add(2 * time.Second)
	if _, err := service.StartUpdate(context.Background(), nil, testCredentials(), tabletingest.TDurability_SYNC); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CloseUpdate(context.Background(), nil, first); err == nil {
		t.Fatal("expired session unexpectedly existed")
	}
	service.BeginDrain()
	if service.Accepting() {
		t.Fatal("service remained accepting during drain")
	}
	if _, err := service.StartUpdate(context.Background(), nil, testCredentials(), tabletingest.TDurability_SYNC); err == nil {
		t.Fatal("draining service accepted a session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := service.Drain(ctx); err != nil {
		t.Fatalf("bounded drain: %v", err)
	}
}

func TestSessionExpirySkipsActiveConditionalSession(t *testing.T) {
	now := time.Unix(100, 0)
	service := newTestService(t, func(cfg *Config) {
		cfg.ConditionalReader = fakeConditionalReader{}
		cfg.TserverLock = func() string { return "lock" }
		cfg.SessionTTL = time.Second
		cfg.Now = func() time.Time { return now }
	}, &fakeDirectory{tablets: make(map[string]*fakeTablet), errs: make(map[string]error)})
	first, err := service.StartConditionalUpdate(
		context.Background(), nil, testCredentials(), nil, "1",
		tabletingest.TDurability_SYNC, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	session := service.conditionalSessions[data.UpdateID(first.SessionId)]
	session.mu.Lock()
	now = now.Add(2 * time.Second)

	started := make(chan error, 1)
	go func() {
		_, err := service.StartConditionalUpdate(
			context.Background(), nil, testCredentials(), nil, "1",
			tabletingest.TDurability_SYNC, "",
		)
		started <- err
	}()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("starting a nested conditional session blocked on active-session expiration")
	}
	session.mu.Unlock()
}

func TestConcurrentCancelAndApply(t *testing.T) {
	extent := ingestrouter.Extent{TableID: "1", EndRow: []byte("z")}
	service := newTestService(t, nil, &fakeDirectory{
		tablets: map[string]*fakeTablet{extent.Key(): {
			extent: extent,
			fence:  ingestrouter.Fence{ServerGeneration: "s", ManagerGeneration: "m", Assignment: 1},
		}},
		errs: make(map[string]error),
	})
	id, _ := service.StartUpdate(context.Background(), nil, testCredentials(), tabletingest.TDurability_SYNC)
	mutation := testMutation(t, "a")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = service.ApplyUpdates(context.Background(), nil, id, testExtent("1", "z"),
				[]*data.TMutation{mutation})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = service.CancelUpdate(context.Background(), nil, id)
	}()
	wg.Wait()
}
