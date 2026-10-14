// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/disclosureconformance"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type admissionHarness struct {
	now       time.Time
	clock     func() time.Time
	authority *auth.Authority
	service   *AdmissionService
	dispatch  *DispatchService
	store     *memoryDispatchStore
	events    *controlledDispatchEvents
	recorder  *dispatchRecorder
	registry  *Service
	// registryStore is exposed so a test can lapse the descriptor's lease
	// without advancing the clock, which would also expire the admission
	// token and the action deadline and refuse for those reasons instead.
	registryStore *memoryStore
	executor      *runnableCeilingExecutor
}

type stubRestrictor struct {
	calls   int
	seen    [][]shoal.ID
	allowed []shoal.ID
	err     error
}

func (r *stubRestrictor) RestrictDisclosure(
	_ context.Context, references []shoal.ID,
) ([]shoal.ID, error) {
	r.calls++
	r.seen = append(r.seen, append([]shoal.ID(nil), references...))
	if r.err != nil {
		return nil, r.err
	}
	return append([]shoal.ID(nil), r.allowed...), nil
}

// wideningRestrictor admits one more reference on every call, standing in for
// the co-occurrence budget's window moving between a grant and a retry. A
// service that re-adjudicates a replay hands the later, weaker answer back.
type wideningRestrictor struct {
	calls int
}

func (r *wideningRestrictor) RestrictDisclosure(
	_ context.Context, references []shoal.ID,
) ([]shoal.ID, error) {
	r.calls++
	if r.calls > len(references) {
		return append([]shoal.ID(nil), references...), nil
	}
	return append([]shoal.ID(nil), references[:r.calls-1]...), nil
}

// runnableCeilingExecutor is bound with a ceiling *and* an Execute method.
//
// The Execute method is what makes the ExecuteClaim guard reachable at all:
// without it resolveAction refuses the executor before ExecuteClaim ever loads
// the record, so a test would pass on an error that says nothing about
// admissions and the guard would go unexercised. A host that binds a runnable
// executor for an action an admission is granted against reaches the guard, and
// that is the configuration worth testing.
type runnableCeilingExecutor struct {
	ceiling Effects
	calls   int
}

func (e *runnableCeilingExecutor) MaxEffects() Effects { return e.ceiling }

func (e *runnableCeilingExecutor) Execute(
	context.Context, Invocation,
) (ExecutionResult, error) {
	e.calls++
	return ExecutionResult{Output: json.RawMessage(`{"ok":true}`)}, nil
}

// admissionDescriptor declares one action per effect class combination the
// tests need, bound to an executor whose ceiling covers all of them, so a
// denial in these tests is always the action's declaration and never the
// executor's.
func admissionDescriptor(now time.Time) Descriptor {
	schema := json.RawMessage(`{"type":"object"}`)
	return Descriptor{
		ID: "agent", Generation: 1, Subject: "owner", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		Scopes: []Scope{
			{SourceID: []byte("source"), PolicyID: []byte("policy")},
		},
		ExecutorRef: "exec", LeaseExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		Capabilities: []Capability{{Name: "model", Actions: []Action{
			{
				Name: "complete", InputSchema: schema, OutputSchema: schema,
				Effects: Effects{EffectEgressesContent, EffectReadsCorpus},
			},
			{
				Name: "summarize", InputSchema: schema, OutputSchema: schema,
				Effects: Effects{EffectReadsCorpus},
			},
			{
				Name: "publish", InputSchema: schema, OutputSchema: schema,
				Effects: Effects{EffectMutatesExternal},
			},
		}}},
	}
}

func newAdmissionHarness(
	t *testing.T, restrictor DisclosureRestrictor,
) *admissionHarness {
	t.Helper()
	harness := &admissionHarness{
		now: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	}
	harness.clock = func() time.Time { return harness.now }
	authority, err := auth.NewAuthorityWithClock(harness.clock)
	if err != nil {
		t.Fatal(err)
	}
	registryStore := newMemoryStore()
	executor := &runnableCeilingExecutor{ceiling: Effects{
		EffectReadsCorpus, EffectEgressesContent, EffectMutatesExternal,
	}}
	registry, err := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{harness.now},
		Executors: executorMap{"exec": executor},
		Clock:     harness.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	registryStore.records["agent"] = Stored{
		Descriptor: admissionDescriptor(harness.now),
	}
	harness.registry = registry
	harness.registryStore = registryStore
	harness.executor = executor
	harness.store = newMemoryDispatchStore()
	harness.recorder = &dispatchRecorder{}
	harness.events = &controlledDispatchEvents{}
	dispatch, err := NewDispatchService(DispatchConfig{
		Store: harness.store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: harness.recorder, Events: harness.events, Clock: harness.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.dispatch = dispatch
	service, err := NewAdmissionService(AdmissionConfig{
		Dispatch: dispatch, Restrictor: restrictor,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.service = service
	harness.authority = authority
	return harness
}

func (h *admissionHarness) context(t *testing.T, request string) context.Context {
	t.Helper()
	return bindDecision(t, h.authority, dispatchDecision(
		t, "owner", "actor", request, auth.OperationInvoke,
		auth.OperationRetrieve))
}

// invokeOnlyContext holds no retrieve authority, which is the posture of a
// caller that may act but may not read.
func (h *admissionHarness) invokeOnlyContext(
	t *testing.T, request string,
) context.Context {
	t.Helper()
	return bindDecision(t, h.authority, dispatchDecision(
		t, "owner", "actor", request, auth.OperationInvoke))
}

// storedID is where an admission this harness's principal names lands in the
// store. The durable ID is derived from the principal, so a test that looks for
// the caller-supplied name finds nothing.
func (h *admissionHarness) storedID(t *testing.T, id string) []byte {
	t.Helper()
	return admissionActionID(
		dispatchDecision(t, "owner", "actor", "request", auth.OperationInvoke),
		[]byte(id))
}

// stored reads the record for an admission by the name its caller gave it.
func (h *admissionHarness) stored(t *testing.T, id string) (ActionRecord, error) {
	t.Helper()
	return h.store.GetAction(context.Background(), h.storedID(t, id))
}

func (h *admissionHarness) request(
	request, id, action string,
	effects Effects,
	disclosures []shoal.ID,
) AdmissionRequest {
	return AdmissionRequest{
		ID: []byte(id), IdempotencyKey: []byte("idempotency-" + id),
		TokenID: []byte("token-" + id), AgentID: "agent", AgentGeneration: 1,
		Capability: "model", Action: action,
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object", Effects: effects,
		Input: json.RawMessage(`{"prompt_digest":"abc"}`), Disclosures: disclosures,
		Lease: time.Minute, Context: dispatchContext(h.now, request),
	}
}

// TestAdmissionDeniesEffectBeyondDeclaredCapability pins the stop: a caller
// asking to egress corpus content through an action that declares only a corpus
// read is refused, the refusal is durable, and it names nothing.
func TestAdmissionDeniesEffectBeyondDeclaredCapability(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	ctx := harness.context(t, "request")
	grant, err := harness.service.Request(ctx, harness.request(
		"request", "admission", "summarize",
		Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatalf("admission request = %v", err)
	}
	if grant.Outcome != AdmissionDenied {
		t.Fatalf("outcome = %q", grant.Outcome)
	}
	if len(grant.Token.ActionID) != 0 || len(grant.Token.TokenID) != 0 ||
		grant.Token.Version != 0 {
		t.Fatalf("denial issued a token: %#v", grant.Token)
	}
	stored, err := harness.stored(t, "admission")
	if err != nil {
		t.Fatalf("stored denial = %v", err)
	}
	if stored.State != DispatchCanceled {
		t.Fatalf("denial state = %q", stored.State)
	}
	var canceled bool
	for _, kind := range harness.events.kinds {
		if kind == "action.canceled" {
			canceled = true
		}
	}
	if !canceled {
		t.Fatalf("denial published no lifecycle event: %v", harness.events.kinds)
	}
}

// TestAdmissionDenialAnswersFromTheRecordOnReplay pins that a refused admission
// stays refused. Re-adjudicating a replay would let a caller retry a denial
// until whatever it depended on moved.
func TestAdmissionDenialAnswersFromTheRecordOnReplay(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	first := harness.request(
		"request", "admission", "summarize", Effects{EffectEgressesContent}, nil)
	if _, err := harness.service.Request(
		harness.context(t, "request"), first,
	); err != nil {
		t.Fatal(err)
	}
	replay := harness.request(
		"replay", "admission", "summarize", Effects{EffectEgressesContent}, nil)
	replay.IdempotencyKey = first.IdempotencyKey
	replay.Context = dispatchContext(harness.now, "request")
	grant, err := harness.service.Request(harness.context(t, "request"), replay)
	if err != nil {
		t.Fatalf("replayed denial = %v", err)
	}
	if grant.Outcome != AdmissionDenied {
		t.Fatalf("replayed outcome = %q", grant.Outcome)
	}
}

// TestAdmissionRequiresADeclaredEffect pins the fail-closed reading of the
// least classifiable request there is.
func TestAdmissionRequiresADeclaredEffect(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	// The message matters, not only the code. An empty declaration is also
	// refused downstream by ActionRecord.Validate, which rejects a scheme
	// marker with no admitted effect — same code, but a message about record
	// coherence rather than about what the caller sent. Asserting the message
	// keeps the early refusal load-bearing instead of shadowed by the later
	// one, and tells the caller what it actually got wrong.
	_, err := harness.service.Request(
		harness.context(t, "request"),
		harness.request("request", "admission", "complete", nil, nil))
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("empty effect set = %v", err)
	}
	if !strings.Contains(err.Error(), "must declare an effect") {
		t.Fatalf("empty effect set refused for the wrong reason: %v", err)
	}
	if _, err := harness.service.Request(
		harness.context(t, "request"),
		harness.request(
			"request", "admission", "complete", Effects{"invented"}, nil),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("unrecognised effect = %v", err)
	}
}

// TestAdmissionAllowsDeclaredEffectAndIssuesALiveToken pins the allow path and
// that the token names the durable claim it was granted under.
func TestAdmissionAllowsDeclaredEffectAndIssuesALiveToken(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	ctx := harness.context(t, "request")
	grant, err := harness.service.Request(ctx, harness.request(
		"request", "admission", "complete",
		Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatalf("admission request = %v", err)
	}
	if grant.Outcome != AdmissionAllowed {
		t.Fatalf("outcome = %q", grant.Outcome)
	}
	// Version one, not two. A grant is a single durable write: the record is
	// born claimed and never passes through the queued state where an
	// ungranted admission would be claimable work.
	if !bytes.Equal(grant.Token.ActionID, harness.storedID(t, "admission")) ||
		string(grant.Token.TokenID) != "token-admission" ||
		grant.Token.Version != 1 {
		t.Fatalf("token = %#v", grant.Token)
	}
	if !grant.Token.ExpiresAt.Equal(harness.now.Add(time.Minute)) {
		t.Fatalf("token expiry = %s", grant.Token.ExpiresAt)
	}
	stored, err := harness.stored(t, "admission")
	if err != nil || stored.State != DispatchClaimed {
		t.Fatalf("stored grant = %#v, %v", stored, err)
	}
}

// TestAdmissionObligationsWithholdWithoutSayingWhy pins the obligation path:
// references the caller is not authorized for and references the restrictor
// withheld come back in one undifferentiated list, and the call is allowed
// rather than refused.
func TestAdmissionObligationsWithholdWithoutSayingWhy(t *testing.T) {
	restrictor := &stubRestrictor{allowed: []shoal.ID{"doc-a"}}
	harness := newAdmissionHarness(t, restrictor)
	ctx := harness.context(t, "request")
	// Declared out of order and with a repeat, because the obligation has to be
	// the same answer either way: a caller comparing two responses must be
	// comparing decisions, not its own iteration order, and a reference named
	// twice is one reference.
	grant, err := harness.service.Request(ctx, harness.request(
		"request", "admission", "complete", Effects{EffectEgressesContent},
		[]shoal.ID{"doc-c", "doc-a", "doc-b", "doc-c"}))
	if err != nil {
		t.Fatalf("admission request = %v", err)
	}
	if grant.Outcome != AdmissionObligated {
		t.Fatalf("outcome = %q", grant.Outcome)
	}
	if len(grant.Token.ActionID) == 0 {
		t.Fatal("obligated admission issued no token")
	}
	if len(grant.Obligations.Withhold) != 2 ||
		grant.Obligations.Withhold[0] != "doc-b" ||
		grant.Obligations.Withhold[1] != "doc-c" {
		t.Fatalf("withhold = %#v", grant.Obligations.Withhold)
	}
	if restrictor.calls != 1 || len(restrictor.seen[0]) != 3 {
		t.Fatalf("restrictor saw %#v", restrictor.seen)
	}
}

// TestAdmissionWithholdsEveryReferenceWithoutRetrieveAuthority pins that a
// caller which may act but may not read is obliged to withhold everything
// rather than being handed permission to transmit content it cannot read.
func TestAdmissionWithholdsEveryReferenceWithoutRetrieveAuthority(t *testing.T) {
	restrictor := &stubRestrictor{allowed: []shoal.ID{"doc-a", "doc-b"}}
	harness := newAdmissionHarness(t, restrictor)
	grant, err := harness.service.Request(
		harness.invokeOnlyContext(t, "request"), harness.request(
			"request", "admission", "complete", Effects{EffectEgressesContent},
			[]shoal.ID{"doc-a", "doc-b"}))
	if err != nil {
		t.Fatalf("admission request = %v", err)
	}
	if grant.Outcome != AdmissionObligated ||
		len(grant.Obligations.Withhold) != 2 {
		t.Fatalf("grant = %#v", grant)
	}
	// The restrictor is consulted, and learns about nothing. A reference
	// authorization already withheld must not be handed on to a downstream
	// control, which would charge a budget for a disclosure that cannot happen.
	if restrictor.calls != 1 || len(restrictor.seen[0]) != 0 {
		t.Fatalf("restrictor saw unauthorized references: %#v", restrictor.seen)
	}
}

// TestAdmissionRestrictorCannotWidenAuthorization pins the direction of the
// intersection. A restrictor is a narrowing control: a reference it returns
// that authorization already refused must stay refused, and a reference the
// caller never declared must not appear at all.
func TestAdmissionRestrictorCannotWidenAuthorization(t *testing.T) {
	restrictor := &stubRestrictor{
		allowed: []shoal.ID{"doc-a", "doc-elsewhere"},
	}
	harness := newAdmissionHarness(t, restrictor)
	grant, err := harness.service.Request(
		harness.invokeOnlyContext(t, "request"), harness.request(
			"request", "admission", "complete", Effects{EffectEgressesContent},
			[]shoal.ID{"doc-a"}))
	if err != nil {
		t.Fatalf("admission request = %v", err)
	}
	if grant.Outcome != AdmissionObligated {
		t.Fatalf("outcome = %q", grant.Outcome)
	}
	if len(grant.Obligations.Withhold) != 1 ||
		grant.Obligations.Withhold[0] != "doc-a" {
		t.Fatalf("withhold = %#v", grant.Obligations.Withhold)
	}
}

// TestAdmissionRestrictorFailureIsNotAnEmptyObligation pins that a restrictor
// which cannot answer stops the request rather than being read as "withhold
// nothing".
func TestAdmissionRestrictorFailureIsNotAnEmptyObligation(t *testing.T) {
	restrictor := &stubRestrictor{err: errors.New("ledger unavailable")}
	harness := newAdmissionHarness(t, restrictor)
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete", Effects{EffectEgressesContent},
			[]shoal.ID{"doc-a"}),
	); err == nil {
		t.Fatal("restrictor failure produced an answer")
	}
}

// TestAdmissionRefusesAGrantHeldUnderAnotherToken pins that a live admission is
// not handed to a second caller replaying the same admission identity with a
// token of its own.
func TestAdmissionRefusesAGrantHeldUnderAnotherToken(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	first := harness.request(
		"request", "admission", "complete", Effects{EffectEgressesContent}, nil)
	if _, err := harness.service.Request(
		harness.context(t, "request"), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.TokenID = []byte("token-stolen")
	if _, err := harness.service.Request(
		harness.context(t, "request"), second,
	); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("second token = %v", err)
	}
}

// TestReportClosesTheLoopAndSpendsItsToken pins the report path: the outcome is
// recorded durably, an identical replay is idempotent, and a differing report
// against the same token is refused.
func TestReportClosesTheLoopAndSpendsItsToken(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	ctx := harness.context(t, "request")
	grant, err := harness.service.Request(ctx, harness.request(
		"request", "admission", "complete",
		Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatal(err)
	}
	report := AdmissionReport{
		Token: grant.Token, Outcome: json.RawMessage(`{"tokens":41}`),
		Context: dispatchContext(harness.now, "report"),
	}
	record, err := harness.service.Report(
		harness.context(t, "report"), report)
	if err != nil || record.State != DispatchSucceeded {
		t.Fatalf("report = %#v, %v", record, err)
	}
	replay, err := harness.service.Report(
		harness.context(t, "report"), report)
	if err != nil || replay.Version != record.Version {
		t.Fatalf("identical replay = %#v, %v", replay, err)
	}
	different := report
	different.Outcome = json.RawMessage(`{"tokens":9999}`)
	if _, err := harness.service.Report(
		harness.context(t, "report"), different,
	); !errors.Is(err, ErrAdmissionSpent) {
		t.Fatalf("differing replay = %v", err)
	}
	failed := report
	failed.Failed = true
	failed.ErrorCode = "upstream_refused"
	failed.Outcome = nil
	if _, err := harness.service.Report(
		harness.context(t, "report"), failed,
	); !errors.Is(err, ErrAdmissionSpent) {
		t.Fatalf("failure against a spent token = %v", err)
	}
	// A malformed report is refused as malformed even against a spent token.
	// Answering "spent" here would tell a caller its report was well formed and
	// merely late, and it would send the same malformed body to the next
	// admission expecting it to land.
	reasonless := failed
	reasonless.ErrorCode = ""
	if _, err := harness.service.Report(
		harness.context(t, "report"), reasonless,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("reasonless failure against a spent token = %v", err)
	}
}

// TestReportAfterTheTokenExpiresIsSpent pins that a caller which held a grant,
// went silent past its lease, and came back cannot write the record. By then
// the admission is abandoned and whether the effect happened is unknown.
func TestReportAfterTheTokenExpiresIsSpent(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	grant, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete",
			Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatal(err)
	}
	harness.now = harness.now.Add(2 * time.Minute)
	if _, err := harness.service.Report(
		harness.context(t, "report"), AdmissionReport{
			Token: grant.Token, Outcome: json.RawMessage(`{"tokens":1}`),
			Context: dispatchContext(harness.now, "report"),
		},
	); !errors.Is(err, ErrAdmissionSpent) {
		t.Fatalf("report past the lease = %v", err)
	}
}

// TestReportRejectsATokenSpentOnADenial pins that a refused admission's record
// cannot be reported against. A report is a statement that an effect occurred.
func TestReportRejectsATokenSpentOnADenial(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "summarize",
			Effects{EffectEgressesContent}, nil),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.Report(
		harness.context(t, "report"), AdmissionReport{
			Token: AdmissionToken{
				ActionID: harness.storedID(t, "admission"),
				TokenID:  []byte("token-admission"), Version: 1,
			},
			Outcome: json.RawMessage(`{"tokens":1}`),
			Context: dispatchContext(harness.now, "report"),
		},
	); !errors.Is(err, ErrAdmissionSpent) {
		t.Fatalf("report against a denial = %v", err)
	}
}

// TestReportRefusesAFailureWithoutAReasonOrAnOutcomeWithOne pins that a report
// is either an outcome or a failure, never both and never neither.
func TestReportRefusesAFailureWithoutAReasonOrAnOutcomeWithOne(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	grant, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete",
			Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.Report(
		harness.context(t, "report"), AdmissionReport{
			Token: grant.Token, Failed: true,
			Context: dispatchContext(harness.now, "report"),
		},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("failure with no reason = %v", err)
	}
	if _, err := harness.service.Report(
		harness.context(t, "report"), AdmissionReport{
			Token: grant.Token, ErrorCode: "quiet_failure",
			Outcome: json.RawMessage(`{"tokens":1}`),
			Context: dispatchContext(harness.now, "report"),
		},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("outcome carrying an error code = %v", err)
	}
}

// TestReportDoesNotDistinguishAbsentFromAnotherPrincipals pins the disclosure
// shape: a token naming someone else's admission is not-found, exactly as a
// token naming nothing is.
func TestReportDoesNotDistinguishAbsentFromAnotherPrincipals(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete",
			Effects{EffectEgressesContent}, nil)); err != nil {
		t.Fatal(err)
	}
	intruder := bindDecision(t, harness.authority, dispatchDecision(
		t, "mallory", "mallory", "probe", auth.OperationInvoke))
	present := AdmissionReport{
		Token: AdmissionToken{
			ActionID: []byte("admission"),
			TokenID:  []byte("token-admission"), Version: 2,
		},
		Outcome: json.RawMessage(`{"tokens":1}`),
		Context: dispatchContext(harness.now, "probe"),
	}
	absent := present
	absent.Token.ActionID = []byte("never-existed")
	_, presentErr := harness.service.Report(intruder, present)
	_, absentErr := harness.service.Report(intruder, absent)
	if !shoal.IsErrorCode(presentErr, shoal.ErrorNotFound) ||
		!shoal.IsErrorCode(absentErr, shoal.ErrorNotFound) {
		t.Fatalf("probe errors = %v / %v", presentErr, absentErr)
	}
	if presentErr.Error() != absentErr.Error() {
		t.Fatalf("probe distinguished existence: %q vs %q",
			presentErr, absentErr)
	}
}

// TestOutstandingShowsAGrantNobodyReported pins the observability requirement:
// an admitted call that never comes back is visible, stays visible after its
// token expires, and disappears only when it is reported.
func TestOutstandingShowsAGrantNobodyReported(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	grant, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete",
			Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatal(err)
	}
	listRequest := OutstandingAdmissionsRequest{
		Limit: 10, Context: dispatchContext(harness.now, "list"),
	}
	page, err := harness.service.Outstanding(
		harness.context(t, "list"), listRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Admissions) != 1 ||
		!bytes.Equal(page.Admissions[0].ActionID,
			harness.storedID(t, "admission")) ||
		string(page.Admissions[0].TokenID) != "token-admission" ||
		page.Admissions[0].Expired {
		t.Fatalf("outstanding = %#v", page.Admissions)
	}

	harness.now = harness.now.Add(2 * time.Minute)
	listRequest.Context = dispatchContext(harness.now, "list")
	page, err = harness.service.Outstanding(
		harness.context(t, "list"), listRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Admissions) != 1 || !page.Admissions[0].Expired {
		t.Fatalf("expired outstanding = %#v", page.Admissions)
	}

	harness.now = harness.now.Add(-2 * time.Minute)
	if _, err := harness.service.Report(
		harness.context(t, "report"), AdmissionReport{
			Token: grant.Token, Outcome: json.RawMessage(`{"tokens":1}`),
			Context: dispatchContext(harness.now, "report"),
		}); err != nil {
		t.Fatal(err)
	}
	listRequest.Context = dispatchContext(harness.now, "list")
	page, err = harness.service.Outstanding(
		harness.context(t, "list"), listRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Admissions) != 0 {
		t.Fatalf("reported admission still outstanding: %#v", page.Admissions)
	}
}

// TestOutstandingHidesOtherPrincipalsGrants pins that the outstanding list is
// not a window on to another caller's admissions.
func TestOutstandingHidesOtherPrincipalsGrants(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete",
			Effects{EffectEgressesContent}, nil)); err != nil {
		t.Fatal(err)
	}
	intruder := bindDecision(t, harness.authority, dispatchDecision(
		t, "mallory", "mallory", "probe", auth.OperationInvoke))
	page, err := harness.service.Outstanding(
		intruder, OutstandingAdmissionsRequest{
			Limit: 10, Context: dispatchContext(harness.now, "probe"),
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Admissions) != 0 {
		t.Fatalf("leaked admissions: %#v", page.Admissions)
	}
}

// TestAdmissionReplayCannotChangeWhatWasAdmitted pins the durable declaration.
//
// The laundering it prevents: ask with corpus references, receive obligations
// restricting them, then replay the same action ID, idempotency key and token
// with the references removed. Before the declaration was part of the record's
// identity that replay was recognised as the same request, obligations were
// recomputed over nothing, and the answer was an unrestricted allow for a token
// that was already live.
func TestAdmissionReplayCannotChangeWhatWasAdmitted(t *testing.T) {
	restrictor := &stubRestrictor{allowed: []shoal.ID{"doc-a"}}
	harness := newAdmissionHarness(t, restrictor)
	first := harness.request(
		"request", "admission", "complete", Effects{EffectEgressesContent},
		[]shoal.ID{"doc-a", "doc-b"})
	grant, err := harness.service.Request(harness.context(t, "request"), first)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Outcome != AdmissionObligated ||
		len(grant.Obligations.Withhold) != 1 {
		t.Fatalf("first grant = %#v", grant)
	}

	laundered := first
	laundered.Disclosures = nil
	if _, err := harness.service.Request(
		harness.context(t, "request"), laundered,
	); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("replay without disclosures = %v", err)
	}
	widened := first
	widened.Effects = Effects{EffectEgressesContent, EffectReadsCorpus}
	if _, err := harness.service.Request(
		harness.context(t, "request"), widened,
	); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("replay with a wider effect set = %v", err)
	}
	// Same size, different contents. A comparison that stopped at the length
	// would let a caller admitted to egress replay as a corpus read and keep
	// the live token, so the audit record would name an effect nobody was
	// granted.
	swapped := first
	swapped.Effects = Effects{EffectReadsCorpus}
	if _, err := harness.service.Request(
		harness.context(t, "request"), swapped,
	); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("replay with a swapped effect set = %v", err)
	}
	narrowed := first
	narrowed.Disclosures = []shoal.ID{"doc-a"}
	if _, err := harness.service.Request(
		harness.context(t, "request"), narrowed,
	); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("replay with fewer disclosures = %v", err)
	}

	// The identical request still replays on to its own grant, or a caller that
	// lost a response could never recover its token.
	again, err := harness.service.Request(harness.context(t, "request"), first)
	if err != nil {
		t.Fatalf("identical replay = %v", err)
	}
	if again.Outcome != AdmissionObligated ||
		!bytes.Equal(again.Token.TokenID, grant.Token.TokenID) ||
		again.Token.Version != grant.Token.Version {
		t.Fatalf("identical replay = %#v", again)
	}
}

// TestDispatchEnqueueCannotReachAnAdmissionRecord pins that the two surfaces do
// not share a name space at all.
//
// An earlier version of this test asserted that a dispatch enqueue at the
// admission's name conflicted with it, which was true when the caller's name
// was the durable ID. It is not any more: the durable ID is derived from the
// principal, so the same name is two different records and the dispatch enqueue
// simply succeeds beside the admission rather than colliding with it. The
// guarantee is stronger — there is no name a dispatch caller can supply that
// addresses an admission — so the test asserts that instead.
func TestDispatchEnqueueCannotReachAnAdmissionRecord(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	grant, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete",
			Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := bindDecision(t, harness.authority, dispatchDecision(
		t, "owner", "actor", "request", auth.OperationDispatch))
	queued, err := harness.dispatch.Enqueue(dispatcher, EnqueueRequest{
		ID:      []byte("admission"),
		AgentID: "agent", AgentGeneration: 1,
		IdempotencyKey: []byte("idempotency-admission"),
		Capability:     "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object",
		Input:    json.RawMessage(`{"prompt_digest":"abc"}`),
		Context:  dispatchContext(harness.now, "request"),
	})
	if err != nil {
		t.Fatalf("dispatch enqueue beside an admission = %v", err)
	}
	if bytes.Equal(queued.ID, grant.Token.ActionID) {
		t.Fatal("a dispatch enqueue landed on the admission's record")
	}
	if queued.State != DispatchQueued || queued.isAdmission() {
		t.Fatalf("dispatch record = %#v", queued)
	}
	// The admission is untouched and still claimed under its own token.
	admission, err := harness.stored(t, "admission")
	if err != nil || admission.State != DispatchClaimed ||
		!bytes.Equal(admission.ClaimID, grant.Token.TokenID) {
		t.Fatalf("admission record = %#v, %v", admission, err)
	}
}

// TestAdmissionNeverLeavesClaimableWork pins the stop.
//
// No admission outcome — granted, refused, or abandoned part way through
// adjudication — may leave a record Pull will hand back. A queued record is
// claimable through the dispatch surface, and under the execution boundary a
// claim is permission to perform the declared effect out of process, so a
// refused or never-decided admission sitting in the queue is the refusal being
// laundered into permission through a different door.
func TestAdmissionNeverLeavesClaimableWork(t *testing.T) {
	failing := &stubRestrictor{err: errors.New("ledger unavailable")}
	harness := newAdmissionHarness(t, failing)

	// Adjudication fails after every check that precedes the write.
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "aborted", "complete", Effects{EffectEgressesContent},
			[]shoal.ID{"doc-a"}),
	); err == nil {
		t.Fatal("restrictor failure produced an answer")
	}
	if _, err := harness.stored(t, "aborted"); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("aborted admission left a record: %v", err)
	}

	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "refused", "summarize", Effects{EffectEgressesContent},
			nil)); err != nil {
		t.Fatal(err)
	}
	refused, err := harness.stored(t, "refused")
	if err != nil || refused.State != DispatchCanceled || refused.Version != 1 {
		t.Fatalf("refusal record = %#v, %v", refused, err)
	}

	harness.service.restrictor = nil
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "granted", "complete", Effects{EffectEgressesContent},
			nil)); err != nil {
		t.Fatal(err)
	}

	page, err := harness.dispatch.Pull(
		harness.context(t, "request"), PullActionsRequest{
			Limit: 16, Context: dispatchContext(harness.now, "request"),
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Actions) != 0 {
		t.Fatalf("admission left claimable work: %#v", page.Actions)
	}
}

// TestReportRefusesAFailureCarryingAnOutcome pins the half of "either an
// outcome or a failure" that the completion path cannot enforce for us.
//
// The completion path discards a failed report's outcome, so the durable record
// says nothing about it and the replay comparison would read any two failures
// with the same error code as the same report. A caller could then report a
// failure, then replace the outcome it carried and be told the second landed.
func TestReportRefusesAFailureCarryingAnOutcome(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	grant, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete",
			Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.service.Report(
		harness.context(t, "report"), AdmissionReport{
			Token: grant.Token, Failed: true, ErrorCode: "upstream_refused",
			Outcome: json.RawMessage(`{"tokens":41}`),
			Context: dispatchContext(harness.now, "report"),
		},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("failure carrying an outcome = %v", err)
	}
	if stored, err := harness.stored(t, "admission"); err != nil ||
		stored.State != DispatchClaimed {
		t.Fatalf("refused report moved the record: %#v, %v", stored, err)
	}
}

// TestReportingAFailureIsASuccessfulReport pins that the first response and the
// retry agree.
//
// The completion path is built for an executor, where a failed outcome is the
// executor's error and comes back alongside the committed record. Propagating
// it here made the first response an error and the identical retry a receipt,
// so what a caller saw depended on whether its own report had committed — the
// exact confusion the one-shot token exists to remove.
//
// The guarantee has since moved to where it belongs: completeClaim answers a
// recorded failure with the record and no error (#492), so this surface no
// longer converts anything and this test now proves the service's contract
// rather than a local correction of it. It still fails if that contract
// regresses, which is why it stayed here when the conversion was removed.
func TestReportingAFailureIsASuccessfulReport(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	grant, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete",
			Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatal(err)
	}
	report := AdmissionReport{
		Token: grant.Token, Failed: true, ErrorCode: "upstream_refused",
		Context: dispatchContext(harness.now, "report"),
	}
	record, err := harness.service.Report(harness.context(t, "report"), report)
	if err != nil {
		t.Fatalf("reported failure = %v", err)
	}
	if record.State != DispatchFailed ||
		record.ErrorCode != "upstream_refused" {
		t.Fatalf("failure record = %#v", record)
	}
	replay, err := harness.service.Report(harness.context(t, "report"), report)
	if err != nil {
		t.Fatalf("replayed failure = %v", err)
	}
	if replay.Version != record.Version || replay.State != record.State {
		t.Fatalf("replay disagreed with the first response: %#v vs %#v",
			replay, record)
	}
}

// TestReportKeepsAnUnconfirmedOutcomeAnError pins the boundary of that
// conversion. Only a record that is this report, committed, becomes a receipt;
// an outcome the service cannot confirm it wrote must stay an error, or a
// caller learns its report landed when nothing says it did.
func TestReportKeepsAnUnconfirmedOutcomeAnError(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	grant, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete",
			Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatal(err)
	}
	harness.recorder.failPhase = "effect_outcome"
	if _, err := harness.service.Report(
		harness.context(t, "report"), AdmissionReport{
			Token: grant.Token, Failed: true, ErrorCode: "upstream_refused",
			Context: dispatchContext(harness.now, "report"),
		},
	); !errors.Is(err, ErrExecutionAmbiguous) {
		t.Fatalf("unrecordable failure = %v", err)
	}
}

// TestReplayReturnsTheObligationTheTokenWasGrantedUnder pins that the decision
// is replayed, not re-made.
//
// The restrictor is windowed and observes intervening calls, so recomputing on
// a retry can return a weaker obligation for a token that is already live — a
// caller could replay its way out of a restriction it was told to honour. The
// stub here returns a narrower answer on every call, which a recomputing
// implementation would hand straight back.
func TestReplayReturnsTheObligationTheTokenWasGrantedUnder(t *testing.T) {
	restrictor := &wideningRestrictor{}
	harness := newAdmissionHarness(t, restrictor)
	request := harness.request(
		"request", "admission", "complete", Effects{EffectEgressesContent},
		[]shoal.ID{"doc-a", "doc-b"})
	grant, err := harness.service.Request(
		harness.context(t, "request"), request)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Outcome != AdmissionObligated ||
		len(grant.Obligations.Withhold) != 2 {
		t.Fatalf("first grant = %#v", grant)
	}

	replayed, err := harness.service.Request(
		harness.context(t, "request"), request)
	if err != nil {
		t.Fatalf("replay = %v", err)
	}
	if replayed.Outcome != grant.Outcome ||
		len(replayed.Obligations.Withhold) != 2 ||
		replayed.Obligations.Withhold[0] != grant.Obligations.Withhold[0] ||
		replayed.Obligations.Withhold[1] != grant.Obligations.Withhold[1] {
		t.Fatalf("replay weakened the obligation: %#v, was %#v",
			replayed.Obligations, grant.Obligations)
	}
	if restrictor.calls != 1 {
		t.Fatalf("replay re-adjudicated: restrictor called %d times",
			restrictor.calls)
	}
}

// TestReplayRecoversAGrantWhileTheRestrictorIsDown pins the other half. An
// admission that was granted must stay recoverable: a caller that lost its
// response has an outstanding token it cannot report against until it can read
// the grant back, and a transient outage in a control that has already been
// consulted must not extend into one.
func TestReplayRecoversAGrantWhileTheRestrictorIsDown(t *testing.T) {
	restrictor := &stubRestrictor{allowed: []shoal.ID{"doc-a"}}
	harness := newAdmissionHarness(t, restrictor)
	request := harness.request(
		"request", "admission", "complete", Effects{EffectEgressesContent},
		[]shoal.ID{"doc-a", "doc-b"})
	grant, err := harness.service.Request(
		harness.context(t, "request"), request)
	if err != nil {
		t.Fatal(err)
	}

	restrictor.err = errors.New("ledger unavailable")
	replayed, err := harness.service.Request(
		harness.context(t, "request"), request)
	if err != nil {
		t.Fatalf("replay during a restrictor outage = %v", err)
	}
	if !bytes.Equal(replayed.Token.TokenID, grant.Token.TokenID) ||
		replayed.Token.Version != grant.Token.Version {
		t.Fatalf("replay = %#v, want the original token %#v",
			replayed.Token, grant.Token)
	}
	if len(replayed.Obligations.Withhold) != 1 ||
		replayed.Obligations.Withhold[0] != "doc-b" {
		t.Fatalf("replayed obligation = %#v", replayed.Obligations)
	}
}

// TestObligationBitmapRoundTripsPositions pins the encoding the replay depends
// on: the stored positions must name the same references when indexed back
// against the declared list, and nothing must be stored when nothing is
// withheld.
func TestObligationBitmapRoundTripsPositions(t *testing.T) {
	declared := make([]shoal.ID, 20)
	for i := range declared {
		declared[i] = shoal.ID("doc-" + string(rune('a'+i)))
	}
	withheld := Obligations{Withhold: []shoal.ID{
		declared[0], declared[7], declared[8], declared[19],
	}}
	bitmap := obligationBitmap(declared, withheld)
	if len(bitmap) != 3 {
		t.Fatalf("bitmap length = %d, want one bit per declared reference", len(bitmap))
	}
	rebuilt := obligationFromBitmap(bitmap, declared)
	if len(rebuilt.Withhold) != len(withheld.Withhold) {
		t.Fatalf("rebuilt = %#v, want %#v", rebuilt, withheld)
	}
	for i := range withheld.Withhold {
		if rebuilt.Withhold[i] != withheld.Withhold[i] {
			t.Fatalf("rebuilt[%d] = %q, want %q",
				i, rebuilt.Withhold[i], withheld.Withhold[i])
		}
	}
	if obligationBitmap(declared, Obligations{}) != nil {
		t.Fatal("an empty obligation must store nothing")
	}
	if len(obligationFromBitmap(nil, declared).Withhold) != 0 {
		t.Fatal("no stored obligation must rebuild as no obligation")
	}
}

// TestReportRefusesAMalformedFailureWithoutSpendingTheToken pins that a report
// the service will not record leaves the token usable.
//
// The completion path is written for an executor that has already performed the
// work, so it records a malformed result as invalid_executor_error rather than
// refusing it. Reaching that from here would spend a live one-shot token on a
// failure the caller was never told about, and leave it unable to report the
// outcome it actually has.
func TestReportRefusesAMalformedFailureWithoutSpendingTheToken(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	grant, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete",
			Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{
		strings.Repeat("x", MaxActionErrorBytes+1),
		" untrimmed",
	} {
		if _, err := harness.service.Report(
			harness.context(t, "report"), AdmissionReport{
				Token: grant.Token, Failed: true, ErrorCode: code,
				Context: dispatchContext(harness.now, "report"),
			},
		); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("malformed error code %q = %v", code, err)
		}
	}
	// A malformed outcome is the same shape of problem and gets the same
	// answer: the completion path would commit invalid_executor_output.
	if _, err := harness.service.Report(
		harness.context(t, "report"), AdmissionReport{
			Token: grant.Token, Outcome: json.RawMessage(`"not-an-object"`),
			Context: dispatchContext(harness.now, "report"),
		},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("malformed outcome = %v", err)
	}

	stored, err := harness.stored(t, "admission")
	if err != nil || stored.State != DispatchClaimed || stored.Version != 1 {
		t.Fatalf("a refused report moved the record: %#v, %v", stored, err)
	}
	// The token still works.
	if _, err := harness.service.Report(
		harness.context(t, "report"), AdmissionReport{
			Token: grant.Token, Outcome: json.RawMessage(`{"tokens":1}`),
			Context: dispatchContext(harness.now, "report"),
		}); err != nil {
		t.Fatalf("token was spent by the refused reports: %v", err)
	}
}

// TestAdmissionSurfaceRefusesOrdinaryDispatchActions pins the read side of the
// admission marker.
//
// A claimed dispatch action owned by the same principal passes every other
// check on the report path — the registry resolves it, the claim is live — and
// would then be completed under this surface's semantics rather than its own. A
// reported failure is a receipt here and the executor's error there, so a
// worker completing through this endpoint would be told its failed work had
// succeeded.
func TestAdmissionSurfaceRefusesOrdinaryDispatchActions(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	dispatcher := bindDecision(t, harness.authority, dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke))
	queued, err := harness.dispatch.Enqueue(dispatcher, EnqueueRequest{
		ID: []byte("dispatched"), IdempotencyKey: []byte("idempotency-dispatched"),
		AgentID: "agent", AgentGeneration: 1,
		Capability: "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object",
		Input:    json.RawMessage(`{"prompt_digest":"abc"}`),
		Context:  dispatchContext(harness.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := harness.dispatch.Claim(dispatcher, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version,
		ClaimID: []byte("worker"), Lease: time.Minute,
		Context: dispatchContext(harness.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Not-found, and identical to a token naming nothing at all: the caller
	// owns this record, and saying so would distinguish a dispatch action it
	// holds from an admission identity that does not exist.
	token := AdmissionToken{
		ActionID: claimed.ID, TokenID: claimed.ClaimID,
		Version: claimed.Version,
	}
	_, presentErr := harness.service.Report(
		harness.context(t, "report"), AdmissionReport{
			Token: token, Outcome: json.RawMessage(`{"tokens":1}`),
			Context: dispatchContext(harness.now, "report"),
		})
	absent := token
	absent.ActionID = []byte("never-existed")
	_, absentErr := harness.service.Report(
		harness.context(t, "report"), AdmissionReport{
			Token: absent, Outcome: json.RawMessage(`{"tokens":1}`),
			Context: dispatchContext(harness.now, "report"),
		})
	if !shoal.IsErrorCode(presentErr, shoal.ErrorNotFound) {
		t.Fatalf("reporting a dispatch action = %v", presentErr)
	}
	if presentErr.Error() != absentErr.Error() {
		t.Fatalf("refusal distinguished a dispatch action from nothing: %q vs %q",
			presentErr, absentErr)
	}
	if stored, err := harness.store.GetAction(
		context.Background(), []byte("dispatched"),
	); err != nil || stored.State != DispatchClaimed {
		t.Fatalf("refused report moved the dispatch record: %#v, %v", stored, err)
	}

	// Nor is it outstanding. Listing a record this surface refuses to close
	// would name work the caller cannot act on.
	page, err := harness.service.Outstanding(
		harness.context(t, "list"), OutstandingAdmissionsRequest{
			Limit: 16, Context: dispatchContext(harness.now, "list"),
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Admissions) != 0 {
		t.Fatalf("dispatch claim listed as an outstanding admission: %#v",
			page.Admissions)
	}
}

// TestDispatchReclaimAdvancesTheFence pins that the shared claim transition
// advances the fence rather than assigning it.
//
// An admission is born claimed at fence one, which an assignment would also
// produce — so the two cases only diverge on a re-claim, where a stale worker
// holding the previous fence must become detectable. Nothing pinned this before
// the two paths were merged into one function, and a shared assignment would
// have silently let a reclaimed action keep its old fence.
func TestDispatchReclaimAdvancesTheFence(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	dispatcher := bindDecision(t, harness.authority, dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke))
	queued, err := harness.dispatch.Enqueue(dispatcher, EnqueueRequest{
		ID: []byte("dispatched"), IdempotencyKey: []byte("idempotency-dispatched"),
		AgentID: "agent", AgentGeneration: 1,
		Capability: "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object",
		Input:    json.RawMessage(`{"prompt_digest":"abc"}`),
		Context:  dispatchContext(harness.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := harness.dispatch.Claim(dispatcher, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version,
		ClaimID: []byte("worker-one"), Lease: time.Minute,
		Context: dispatchContext(harness.now, "request"),
	})
	if err != nil || first.ClaimFence != 1 {
		t.Fatalf("first claim = %#v, %v", first, err)
	}

	// The lease lapses and a second worker takes the action.
	harness.now = harness.now.Add(2 * time.Minute)
	reclaimer := bindDecision(t, harness.authority, dispatchDecision(
		t, "owner", "actor", "retry",
		auth.OperationDispatch, auth.OperationInvoke))
	second, err := harness.dispatch.Claim(reclaimer, ClaimRequest{
		ID: queued.ID, ExpectedVersion: first.Version,
		ClaimID: []byte("worker-two"), Lease: time.Minute,
		Context: dispatchContext(harness.now, "retry"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ClaimFence != first.ClaimFence+1 {
		t.Fatalf("reclaim fence = %d, want %d",
			second.ClaimFence, first.ClaimFence+1)
	}
}

// TestDispatchCannotReclaimOrCloseAnAdmission pins that the dispatch surface
// will not touch an admission record.
//
// Merging the two claim paths gave dispatch's reclaim semantics reach over
// admissions, and the token a caller already holds carries everything
// CompleteClaim needs — so the bypass does not even require an expiry. Both
// routes are covered: a live admission closed through dispatch completion, and
// an expired one reclaimed and then closed.
func TestDispatchCannotReclaimOrCloseAnAdmission(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	grant, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete",
			Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatal(err)
	}
	worker := bindDecision(t, harness.authority, dispatchDecision(
		t, "owner", "actor", "worker",
		auth.OperationDispatch, auth.OperationInvoke))

	// The live token, used against dispatch completion. This needs no expiry:
	// the caller holds the action ID, the claim ID and the version already.
	if _, err := harness.dispatch.CompleteClaim(worker, CompletionRequest{
		ID: grant.Token.ActionID, ExpectedVersion: grant.Token.Version,
		ClaimID: grant.Token.TokenID,
		Result:  ExecutionResult{Output: json.RawMessage(`{"tokens":1}`)},
		Context: dispatchContext(harness.now, "worker"),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("dispatch completion of a live admission = %v", err)
	}

	// The same token against ExecuteClaim, which takes the record rather than
	// an ID — so the caller synthesises it from what its own grant told it.
	// This was the path the CompleteClaim guard missed.
	synthesised, err := harness.stored(t, "admission")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.dispatch.ExecuteClaim(
		harness.context(t, "request"), synthesised,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("dispatch execution of a live admission = %v", err)
	}
	// The executor is runnable here, so the refusal has to be the guard rather
	// than a missing Execute method — and nothing may have run.
	if harness.executor.calls != 0 {
		t.Fatalf("an admission was executed: %d calls", harness.executor.calls)
	}
	// The same record with the marker stripped is refused too: the guard reads
	// the stored record, not the one the caller passed, so omitting the
	// declaration does not dodge it.
	stripped := synthesised
	stripped.AdmittedEffects = nil
	stripped.AdmittedDisclosures = nil
	stripped.AdmittedObligation = nil
	if _, err := harness.dispatch.ExecuteClaim(
		harness.context(t, "request"), stripped,
	); err == nil {
		t.Fatal("a fabricated record without the marker was executed")
	}
	if harness.executor.calls != 0 {
		t.Fatalf("an admission was executed: %d calls", harness.executor.calls)
	}

	// Status is the fourth path: it must not answer for an admission either.
	if _, err := harness.dispatch.Status(worker, StatusRequest{
		ID: grant.Token.ActionID, Context: dispatchContext(harness.now, "worker"),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("dispatch status of an admission = %v", err)
	}

	// And the fifth: a team-overview reader must not be handed another
	// principal's admission, with what it declared and what it was obliged to
	// withhold.
	reader := bindDecision(t, harness.authority, dispatchDecision(
		t, "reader", "reader", "team", auth.OperationTeamOverviewRead))
	team, err := harness.dispatch.TeamActions(reader, TeamActionListRequest{
		Limit: 16, SourceIDs: [][]byte{[]byte("source")},
		PolicyIDs: [][]byte{[]byte("policy")},
		Context: RequestContext{
			RequestID: "team", CorrelationID: "correlation",
			ReasonCode: "team_overview", Deadline: harness.now.Add(time.Hour),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(team.Actions) != 0 {
		t.Fatalf("admission visible to a team-overview reader: %#v",
			team.Actions)
	}

	// The lease lapses. A dispatch worker must not see it, reclaim it, or
	// cancel it.
	harness.now = harness.now.Add(2 * time.Minute)
	page, err := harness.dispatch.Pull(worker, PullActionsRequest{
		Limit: 16, Context: dispatchContext(harness.now, "worker"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Actions) != 0 {
		t.Fatalf("expired admission offered as dispatch work: %#v", page.Actions)
	}
	if _, err := harness.dispatch.Claim(worker, ClaimRequest{
		ID: grant.Token.ActionID, ExpectedVersion: grant.Token.Version,
		ClaimID: []byte("worker-token"), Lease: time.Minute,
		Context: dispatchContext(harness.now, "worker"),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("dispatch reclaim of an expired admission = %v", err)
	}
	if _, err := harness.dispatch.Cancel(worker, CancelRequest{
		ID: grant.Token.ActionID, ExpectedVersion: grant.Token.Version,
		MutationKey: []byte("worker-cancel"),
		Context:     dispatchContext(harness.now, "worker"),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("dispatch cancel of an expired admission = %v", err)
	}

	// The record is untouched, and still says exactly what happened: permission
	// was granted and nobody came back.
	stored, err := harness.stored(t, "admission")
	if err != nil || stored.State != DispatchClaimed || stored.Version != 1 ||
		!bytes.Equal(stored.ClaimID, grant.Token.TokenID) {
		t.Fatalf("admission record = %#v, %v", stored, err)
	}
	outstanding, err := harness.service.Outstanding(
		harness.context(t, "list"), OutstandingAdmissionsRequest{
			Limit: 16, Context: dispatchContext(harness.now, "list"),
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(outstanding.Admissions) != 1 || !outstanding.Admissions[0].Expired {
		t.Fatalf("outstanding = %#v", outstanding.Admissions)
	}
}

// TestDispatchStillServesItsOwnActions pins the other side of that guard: the
// refusal is scoped to admissions, not to every claimed record. Without this a
// guard that refused everything would pass every assertion above.
func TestDispatchStillServesItsOwnActions(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	worker := bindDecision(t, harness.authority, dispatchDecision(
		t, "owner", "actor", "worker",
		auth.OperationDispatch, auth.OperationInvoke))
	queued, err := harness.dispatch.Enqueue(worker, EnqueueRequest{
		ID: []byte("dispatched"), IdempotencyKey: []byte("idempotency-dispatched"),
		AgentID: "agent", AgentGeneration: 1,
		Capability: "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object",
		Input:    json.RawMessage(`{"prompt_digest":"abc"}`),
		Context:  dispatchContext(harness.now, "worker"),
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := harness.dispatch.Pull(worker, PullActionsRequest{
		Limit: 16, Context: dispatchContext(harness.now, "worker"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Actions) != 1 ||
		!bytes.Equal(page.Actions[0].ID, queued.ID) {
		t.Fatalf("dispatch pull = %#v", page.Actions)
	}
	claimed, err := harness.dispatch.Claim(worker, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version,
		ClaimID: []byte("worker-token"), Lease: time.Minute,
		Context: dispatchContext(harness.now, "worker"),
	})
	if err != nil {
		t.Fatalf("dispatch claim of its own action = %v", err)
	}
	// Status too: the admission guard there must be scoped to admissions, or a
	// guard refusing everything would satisfy every refusal assertion.
	if status, err := harness.dispatch.Status(worker, StatusRequest{
		ID: queued.ID, Context: dispatchContext(harness.now, "worker"),
	}); err != nil || !bytes.Equal(status.ID, queued.ID) {
		t.Fatalf("dispatch status of its own action = %#v, %v", status, err)
	}
	if _, err := harness.dispatch.CompleteClaim(worker, CompletionRequest{
		ID: claimed.ID, ExpectedVersion: claimed.Version,
		ClaimID: claimed.ClaimID,
		Result:  ExecutionResult{Output: json.RawMessage(`{"tokens":1}`)},
		Context: dispatchContext(harness.now, "worker"),
	}); err != nil {
		t.Fatalf("dispatch completion of its own action = %v", err)
	}
}

// TestAdmissionIDIsBoundToItsPrincipal pins that a caller's chosen name is its
// own, so two principals naming the same admission do not meet.
func TestAdmissionIDIsBoundToItsPrincipal(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	mine := harness.context(t, "request")
	theirs := bindDecision(t, harness.authority, dispatchDecision(
		t, "other", "other", "request",
		auth.OperationInvoke, auth.OperationRetrieve))

	first, err := harness.service.Request(mine, harness.request(
		"request", "shared-name", "complete",
		Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatal(err)
	}
	// The same name, a different principal. Before the identity was derived,
	// this was a conflict against a record the second caller could not see.
	second, err := harness.service.Request(theirs, harness.request(
		"request", "shared-name", "complete",
		Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatalf("second principal naming the same admission = %v", err)
	}
	if second.Outcome != AdmissionAllowed {
		t.Fatalf("second principal outcome = %q", second.Outcome)
	}
	if bytes.Equal(first.Token.ActionID, second.Token.ActionID) {
		t.Fatal("two principals share one admission record")
	}
	// Neither can reach the other's, even holding the derived ID.
	if _, err := harness.service.Report(theirs, AdmissionReport{
		Token:   first.Token,
		Outcome: json.RawMessage(`{"tokens":1}`),
		Context: dispatchContext(harness.now, "request"),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("cross-principal report = %v", err)
	}
}

// principalDecision builds a decision varying only the identity components the
// derived admission ID is supposed to separate.
//
// The domain is a parameter, not a constant. It was a constant, which made the
// table below unable to vary the one component its own helper pinned — so
// deleting the domain from the derivation passed a table written specifically
// to catch exactly that. A table-driven test is only as good as the fixture it
// varies against, and a fixture that fixes a field silently removes it from
// every case.
func principalDecision(
	t *testing.T, domain, subject, actor, client string, onBehalfOf []shoal.ID,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(subject), Actor: shoal.ID(actor),
		ClientID: shoal.ID(client), OnBehalfOf: onBehalfOf,
		AuthorizationDomain: []byte(domain),
		AllowedOperations:   []auth.Operation{auth.OperationInvoke},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")},
		PolicyGeneration:    1,
		AuthenticationExpires: time.Date(
			2026, 9, 7, 0, 0, 0, 0, time.UTC),
		RequestID: "request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// TestAdmissionIDSeparatesEveryPrincipalComponent pins that the derived
// identity varies with every part of the principal, and that no part can be
// collided with its neighbour by moving bytes across the boundary.
//
// Each case varies exactly one component. Varying two at once — which an
// earlier version of this test did — passes against a derivation that ignores
// either one of them, because the other still separates the pair.
func TestAdmissionIDSeparatesEveryPrincipalComponent(t *testing.T) {
	base := principalDecision(t, "domain", "owner", "actor", "client", nil)
	baseID := admissionActionID(base, []byte("name"))

	for _, test := range []struct {
		name     string
		decision auth.Decision
		supplied string
	}{
		{"domain", principalDecision(
			t, "other", "owner", "actor", "client", nil), "name"},
		{"subject", principalDecision(
			t, "domain", "other", "actor", "client", nil), "name"},
		{"actor", principalDecision(
			t, "domain", "owner", "other", "client", nil), "name"},
		{"client", principalDecision(
			t, "domain", "owner", "actor", "other", nil), "name"},
		{"delegation", principalDecision(
			t, "domain", "owner", "actor", "client",
			[]shoal.ID{"delegate"}), "name"},
		{"supplied name", base, "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := admissionActionID(test.decision, []byte(test.supplied))
			if bytes.Equal(got, baseID) {
				t.Fatalf("%s does not change the derived identity", test.name)
			}
		})
	}

	// Framing, checked on each boundary between adjacent variable-length
	// components. Without a length prefix these pairs concatenate identically
	// and two principals share one record, which is the whole of what the
	// derivation exists to prevent.
	for _, test := range []struct {
		name          string
		leftDecision  auth.Decision
		leftName      string
		rightDecision auth.Decision
		rightName     string
	}{
		{
			name: "domain/subject",
			leftDecision: principalDecision(
				t, "ab", "c", "actor", "client", nil),
			leftName: "name",
			rightDecision: principalDecision(
				t, "a", "bc", "actor", "client", nil),
			rightName: "name",
		},
		{
			name: "subject/actor",
			leftDecision: principalDecision(
				t, "domain", "ab", "c", "client", nil),
			leftName: "name",
			rightDecision: principalDecision(
				t, "domain", "a", "bc", "client", nil),
			rightName: "name",
		},
		{
			name: "actor/client",
			leftDecision: principalDecision(
				t, "domain", "owner", "ab", "c", nil),
			leftName: "name",
			rightDecision: principalDecision(
				t, "domain", "owner", "a", "bc", nil),
			rightName: "name",
		},
		{
			name: "client/supplied",
			leftDecision: principalDecision(
				t, "domain", "owner", "actor", "ab", nil),
			leftName: "c",
			rightDecision: principalDecision(
				t, "domain", "owner", "actor", "a", nil),
			rightName: "bc",
		},
		{
			name: "delegation/supplied",
			leftDecision: principalDecision(
				t, "domain", "owner", "actor", "client", []shoal.ID{"ab"}),
			leftName: "c",
			rightDecision: principalDecision(
				t, "domain", "owner", "actor", "client", []shoal.ID{"a"}),
			rightName: "bc",
		},
		// The chain is written entry by entry, so the join between two of its
		// entries is a boundary as well, and "each adjacent pair" above skipped
		// it. Only the last entry's join to the supplied name was covered, and
		// that one stays green while the supplied name is framed — so dropping
		// the frame from chain entries collided two delegated principals with
		// the matrix still passing. Identity collision is the one thing this
		// derivation exists to prevent.
		//
		// The chain's other join, from the client ID into the first entry, is
		// deliberately absent: the client ID carries its own frame, so the two
		// sides differ there before the chain is reached whatever the chain
		// does, and the case cannot fail. actor/client covers the client ID's
		// framing. A case with no failure mode of its own would read as
		// coverage without being any.
		{
			name: "delegation/delegation",
			leftDecision: principalDecision(
				t, "domain", "owner", "actor", "client",
				[]shoal.ID{"ab", "c"}),
			leftName: "name",
			rightDecision: principalDecision(
				t, "domain", "owner", "actor", "client",
				[]shoal.ID{"a", "bc"}),
			rightName: "name",
		},
	} {
		t.Run("framing/"+test.name, func(t *testing.T) {
			left := admissionActionID(
				test.leftDecision, []byte(test.leftName))
			right := admissionActionID(
				test.rightDecision, []byte(test.rightName))
			if bytes.Equal(left, right) {
				t.Fatalf("%s boundary collides: %x", test.name, left)
			}
		})
	}

	// Stable for one principal and name, or no retry could find its own record.
	// The request identity deliberately does not participate.
	if !bytes.Equal(baseID, admissionActionID(base, []byte("name"))) {
		t.Fatal("the derived identity is not stable")
	}
}

// TestAdmissionRequiresAWellFormedID pins that the caller's name is validated
// before it is digested. Everything is digestible, so without this an empty or
// oversized name would derive a perfectly valid record identity and be accepted
// where every other surface refuses it.
func TestAdmissionRequiresAWellFormedID(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	for _, name := range []string{"", strings.Repeat("x", MaxActionIDBytes+1)} {
		request := harness.request(
			"request", "placeholder", "complete",
			Effects{EffectEgressesContent}, nil)
		request.ID = []byte(name)
		if _, err := harness.service.Request(
			harness.context(t, "request"), request,
		); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("admission ID of %d bytes = %v", len(name), err)
		}
	}
}

// enqueueDispatchAction queues an ordinary dispatch action under the harness
// principal, so a listing test has something it is entitled to see.
func (h *admissionHarness) enqueueDispatchAction(
	t *testing.T, id string,
) ActionRecord {
	t.Helper()
	worker := bindDecision(t, h.authority, dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke))
	queued, err := h.dispatch.Enqueue(worker, EnqueueRequest{
		ID: []byte(id), IdempotencyKey: []byte("idempotency-" + id),
		AgentID: "agent", AgentGeneration: 1,
		Capability: "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object",
		Input:    json.RawMessage(`{"prompt_digest":"abc"}`),
		Context:  dispatchContext(h.now, "request"),
	})
	if err != nil {
		t.Fatalf("enqueue %q = %v", id, err)
	}
	return queued
}

// legacyAdmission builds a record as the superseded identity scheme wrote one:
// an admission whose durable key is the caller's own name.
func legacyAdmission(
	harness *admissionHarness, name, subject, actor string,
) ActionRecord {
	expires := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	return ActionRecord{
		ID: []byte(name), IdempotencyKey: []byte("idempotency-" + name),
		Version: 1, State: DispatchClaimed, AgentID: "agent",
		AgentGeneration: 1, Capability: "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object", Input: json.RawMessage(`{"prompt_digest":"abc"}`),
		Subject: shoal.ID(subject), Actor: shoal.ID(actor),
		PolicyGeneration: 1, AuthorizationExpiresAt: expires,
		ExecutionPolicyGeneration: 1, ExecutionExpiresAt: expires,
		AuthorizedOperations: []auth.Operation{auth.OperationInvoke},
		RequestID:            "request", CorrelationID: "correlation",
		Reason:      interaction.Reason{Code: "operator_request"},
		ExecutorKey: []byte("executor-" + name),
		ClaimID:     []byte("token-" + name), ClaimFence: 1,
		ClaimLease:      time.Minute,
		ClaimLeaseUntil: harness.now.Add(time.Minute),
		Deadline:        harness.now.Add(time.Hour),
		CreatedAt:       harness.now, UpdatedAt: harness.now,
		AdmittedEffects: Effects{EffectEgressesContent},
		// No AdmittedIdentityScheme: zero is the superseded scheme, which is
		// what a record written before the field existed decodes to.
	}
}

// TestAdmissionRefusesWhileUnmigratedRecordsRemain pins the migration
// boundary, in both terminal shapes and the unresolved one.
//
// The derivation moved the durable key, so a request whose admission was
// written under the caller's own name misses. Granting on that miss issues a
// second live token for work already permitted; and for a terminal record it is
// worse than duplication — a durable *denial* would be re-adjudicated and could
// come back granted, inverting a refusal that is already recorded.
//
// Each shape is driven separately because they fail differently and a single
// probe catches only one.
func TestAdmissionRefusesWhileUnmigratedRecordsRemain(t *testing.T) {
	for _, test := range []struct {
		name  string
		state DispatchState
	}{
		{"unresolved", DispatchClaimed},
		{"reported", DispatchSucceeded},
		{"denied", DispatchCanceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newAdmissionHarness(t, nil)
			legacy := legacyAdmission(harness, "legacy-name", "owner", "actor")
			legacy.State = test.state
			switch test.state {
			case DispatchSucceeded:
				legacy.Output = json.RawMessage(`{"ok":true}`)
				legacy.EffectPossible = true
			case DispatchCanceled:
				legacy.CancelKey = []byte("cancel-legacy")
				legacy.CancelAuthorizationExpiresAt = legacy.ExecutionExpiresAt
				legacy.CancelAuthorizationFingerprint = auth.Fingerprint{1}
			}
			harness.store.records[string(legacy.ID)] = legacy

			// The same name, which is the double grant and, for a denial, the
			// inversion of a recorded refusal.
			grant, err := harness.service.Request(
				harness.context(t, "request"), harness.request(
					"request", "legacy-name", "complete",
					Effects{EffectEgressesContent}, nil))
			if !errors.Is(err, ErrAdmissionUnmigrated) {
				t.Fatalf("request over a %s legacy admission = %#v, %v",
					test.name, grant, err)
			}
			if len(grant.Token.ActionID) != 0 {
				t.Fatalf("a token was issued alongside the refusal: %#v",
					grant.Token)
			}
			if _, err := harness.stored(t, "legacy-name"); !errors.Is(
				err, ErrActionNotFound) {
				t.Fatalf("a second admission was written: %v", err)
			}
			// And any other name too: the verdict is about the store, not the
			// request, so nothing is adjudicated while a record remains.
			if _, err := harness.service.Request(
				harness.context(t, "request"), harness.request(
					"request", "unrelated", "complete",
					Effects{EffectEgressesContent}, nil),
			); !errors.Is(err, ErrAdmissionUnmigrated) {
				t.Fatalf("unrelated request while unmigrated = %v", err)
			}
		})
	}
}

// TestUnmigratedRecordsStillReportAndList pins that the refusal covers
// adjudication only.
//
// A grant already issued must stay reportable, or upgrading strands the audit
// record for an effect that may already have happened — and those records are
// exactly what an operator has to find and drain. Serving resumes once they are
// gone, which means removal rather than reporting: a reported legacy record is
// still a record written under the superseded scheme.
func TestUnmigratedRecordsStillReportAndList(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	legacy := legacyAdmission(harness, "legacy-name", "owner", "actor")
	harness.store.records[string(legacy.ID)] = legacy

	outstanding, err := harness.service.Outstanding(
		harness.context(t, "list"), OutstandingAdmissionsRequest{
			Limit: 16, Context: dispatchContext(harness.now, "list"),
		})
	if err != nil {
		t.Fatalf("outstanding while unmigrated = %v", err)
	}
	if len(outstanding.Admissions) != 1 ||
		!bytes.Equal(outstanding.Admissions[0].ActionID, legacy.ID) {
		t.Fatalf("legacy grant is not visible: %#v", outstanding.Admissions)
	}
	if _, err := harness.service.Report(
		harness.context(t, "report"), AdmissionReport{
			Token: AdmissionToken{
				ActionID: legacy.ID, TokenID: legacy.ClaimID, Version: 1,
			},
			Outcome: json.RawMessage(`{"tokens":1}`),
			Context: dispatchContext(harness.now, "report"),
		}); err != nil {
		t.Fatalf("legacy grant could not be reported: %v", err)
	}
	// Reported is not drained.
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "fresh", "complete",
			Effects{EffectEgressesContent}, nil),
	); !errors.Is(err, ErrAdmissionUnmigrated) {
		t.Fatalf("serving resumed before the record was removed = %v", err)
	}
	// Removed is.
	delete(harness.store.records, string(legacy.ID))
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "fresh", "complete",
			Effects{EffectEgressesContent}, nil)); err != nil {
		t.Fatalf("serving did not resume after removal = %v", err)
	}
}

// TestUnmigratedVerdictIsNotPerCaller pins that the refusal carries no
// information about who holds what.
//
// The per-record predicate it replaced could not manage that. The derivation
// treats the authorization domain as part of the principal, an ActionRecord
// does not carry its domain, and both ways to recover ownership are wrong:
// sameActionPrincipal omits the domain and hands the distinctive error to an
// identically-named identity elsewhere, while going through the agent
// descriptor is blind to any record whose generation has moved — which
// Heartbeat does on every lease renewal. A whole-store verdict has no ownership
// predicate to get wrong.
func TestUnmigratedVerdictIsNotPerCaller(t *testing.T) {
	answer := func(t *testing.T, owner string) string {
		t.Helper()
		harness := newAdmissionHarness(t, nil)
		legacy := legacyAdmission(harness, "contested", owner, owner)
		harness.store.records[string(legacy.ID)] = legacy
		_, err := harness.service.Request(
			harness.context(t, "request"), harness.request(
				"request", "contested", "complete",
				Effects{EffectEgressesContent}, nil))
		if err == nil {
			t.Fatal("a request was served while unmigrated")
		}
		return err.Error()
	}
	mine, theirs := answer(t, "owner"), answer(t, "victim")
	if mine != theirs {
		t.Fatalf("the verdict varies with who owns the record:\n mine:   %q\n"+
			" theirs: %q", mine, theirs)
	}
	// An ordinary dispatch action is not an unmigrated admission, so it must
	// not refuse at all — otherwise every deployment with queued work would be
	// told to migrate.
	harness := newAdmissionHarness(t, nil)
	harness.enqueueDispatchAction(t, "ordinary")
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "ordinary", "complete",
			Effects{EffectEgressesContent}, nil)); err != nil {
		t.Fatalf("an ordinary action blocked admissions: %v", err)
	}
}

// TestUnmigratedVerdictFailureStopsAdjudication pins what happens when the
// verdict cannot be reached. Serving while unable to prove that no unmigrated
// record exists is the double grant, so an unscannable store refuses.
func TestUnmigratedVerdictFailureStopsAdjudication(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	broken := errors.New("store unavailable")
	harness.store.failScan = broken
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "anything", "complete",
			Effects{EffectEgressesContent}, nil),
	); !errors.Is(err, broken) {
		t.Fatalf("request with an unscannable store = %v", err)
	}
	if _, err := harness.stored(t, "anything"); !errors.Is(
		err, ErrActionNotFound) {
		t.Fatalf("an admission was granted despite the failed verdict: %v", err)
	}
	// A failed verdict is not cached, or clearing the fault would need a
	// restart.
	// A second request while the fault persists must fail the same way. If the
	// failure were cached as a verdict, this would succeed — and a store that
	// recovered would never be re-examined either.
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "anything", "complete",
			Effects{EffectEgressesContent}, nil),
	); !errors.Is(err, broken) {
		t.Fatalf("second request with an unscannable store = %v", err)
	}
	harness.store.failScan = nil
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "anything", "complete",
			Effects{EffectEgressesContent}, nil)); err != nil {
		t.Fatalf("request after the store recovered = %v", err)
	}
	// A proven verdict is cached, so the scan does not repeat per request.
	harness.store.scans = 0
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "second", "complete",
			Effects{EffectEgressesContent}, nil)); err != nil {
		t.Fatal(err)
	}
	if harness.store.scans != 0 {
		t.Fatalf("a proven verdict was re-scanned: %d scans",
			harness.store.scans)
	}
}

// TestVerdictAcceptsAStoreFullOfProperlyNamedAdmissions pins the other half of
// the verdict: admissions inside the reserved region are not unmigrated.
//
// The clean verdict is cached, so in a fresh harness the scan runs before any
// admission exists and never sees one. A restart is what exercises it — the
// process comes back with grants already in the store and the first request
// scans over them. Without this, a verdict that ignored the region would refuse
// every deployment that had ever granted an admission, and the suite would not
// notice.
func TestVerdictAcceptsAStoreFullOfProperlyNamedAdmissions(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	for index := 0; index < 3; index++ {
		if _, err := harness.service.Request(
			harness.context(t, "request"), harness.request(
				"request", fmt.Sprintf("grant-%02d", index), "complete",
				Effects{EffectEgressesContent}, nil)); err != nil {
			t.Fatal(err)
		}
	}
	// A new service over the same store: the verdict starts unproven again.
	restarted, err := NewAdmissionService(AdmissionConfig{
		Dispatch: harness.dispatch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Request(
		harness.context(t, "request"), harness.request(
			"request", "after-restart", "complete",
			Effects{EffectEgressesContent}, nil)); err != nil {
		t.Fatalf("a store holding properly named admissions was refused: %v",
			err)
	}
}

// TestVerdictScansEveryPage pins that the verdict pages.
//
// A single-page scan proves nothing about a store larger than one page, and
// every other fixture here fits in one. An unmigrated record beyond the first
// page is the realistic shape — admissions accumulate behind ordinary work —
// and a verdict that stopped early would certify a store it never read.
func TestVerdictScansEveryPage(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	// Filler inserted directly: the verdict only reads, so the records need to
	// exist and sort below the legacy one, not to be independently valid.
	for index := 0; index < MaxDispatchListResults+10; index++ {
		id := fmt.Sprintf("aa-filler-%04d", index)
		harness.store.records[id] = ActionRecord{
			ID: []byte(id), State: DispatchQueued, Subject: "owner",
			Actor: "actor",
		}
	}
	legacy := legacyAdmission(harness, "zz-legacy", "owner", "actor")
	harness.store.records[string(legacy.ID)] = legacy

	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "anything", "complete",
			Effects{EffectEgressesContent}, nil),
	); !errors.Is(err, ErrAdmissionUnmigrated) {
		t.Fatalf("an unmigrated record beyond the first page was missed = %v",
			err)
	}
	if harness.store.scans < 2 {
		t.Fatalf("the verdict read %d pages, so it did not page",
			harness.store.scans)
	}
}

// TestTwoUnmigratedAdmissionsDoNotHideLaterRecords pins the narrower form of
// the outage.
//
// scanDispatchActions can filter a page down to nothing and still have more to
// read: two consecutive admissions outside the reserved region spend both of
// its attempts. TeamActions treated an empty page as an exhausted scan, so it
// reported end-of-scan with records still ahead — the same outage as the
// stranded cursor, triggered by two pre-prefix admissions rather than one
// admission of any kind. One is covered already and would pass.
func TestTwoUnmigratedAdmissionsDoNotHideLaterRecords(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	// Ordinary identities, ordered below "real-work" so the scan meets these
	// two first. Legacy admissions carry caller-supplied identities and so can
	// sort anywhere, which is exactly why the reserved span cannot be used to
	// recognise them.
	for _, name := range []string{"aaa-legacy-one", "aab-legacy-two"} {
		legacy := legacyAdmission(harness, name, "owner", "actor")
		harness.store.records[string(legacy.ID)] = legacy
	}
	queued := harness.enqueueDispatchAction(t, "real-work")

	raw, err := harness.store.ScanActions(context.Background(), nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Actions) != 2 ||
		!raw.Actions[0].isAdmission() || !raw.Actions[1].isAdmission() {
		t.Fatalf("the fixture does not put two admissions first: %#v",
			raw.Actions)
	}

	reader := bindDecision(t, harness.authority, dispatchDecision(
		t, "reader", "reader", "team", auth.OperationTeamOverviewRead))
	team, err := harness.dispatch.TeamActions(reader, TeamActionListRequest{
		Limit: 10, SourceIDs: [][]byte{[]byte("source")},
		PolicyIDs: [][]byte{[]byte("policy")},
		Context: RequestContext{
			RequestID: "team", CorrelationID: "correlation",
			ReasonCode: "team_overview", Deadline: harness.now.Add(time.Hour),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(team.Actions) != 1 ||
		!bytes.Equal(team.Actions[0].ID, queued.ID) {
		t.Fatalf("two unmigrated admissions hid the later record: %#v",
			team.Actions)
	}
}

// TestLegacyAdmissionInsideTheSpanIsStillLegacy is the adversarial case, and
// the reason the verdict reads a marker rather than an identity.
//
// Legacy admission identities were caller-supplied opaque bytes, so one can sit
// anywhere — including inside the reserved span, and including the exact
// prefix-plus-digest shape a derived identity has. A verdict that inferred the
// scheme from the span certified exactly that record as new, the retry derived
// a different key, and a second live grant was issued for work already
// permitted. A coincidental identity would not show this; the identity has to
// be the one the derivation would produce.
func TestLegacyAdmissionInsideTheSpanIsStillLegacy(t *testing.T) {
	for _, test := range []struct {
		name string
		id   func(h *admissionHarness, t *testing.T) []byte
	}{
		{
			name: "the identity the derivation would produce",
			id: func(h *admissionHarness, t *testing.T) []byte {
				return h.storedID(t, "legacy-name")
			},
		},
		{
			name: "some other identity inside the span",
			id: func(*admissionHarness, *testing.T) []byte {
				return append([]byte(admissionIDPrefix), []byte("squatted")...)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newAdmissionHarness(t, nil)
			legacy := legacyAdmission(harness, "placeholder", "owner", "actor")
			legacy.ID = test.id(harness, t)
			if !reservedAdmissionID(legacy.ID) {
				t.Fatalf("the fixture identity is not in the span: %q",
					legacy.ID)
			}
			if legacy.AdmittedIdentityScheme != 0 {
				t.Fatal("the fixture is not a superseded-scheme record")
			}
			harness.store.records[string(legacy.ID)] = legacy

			if _, err := harness.service.Request(
				harness.context(t, "request"), harness.request(
					"request", "legacy-name", "complete",
					Effects{EffectEgressesContent}, nil),
			); !errors.Is(err, ErrAdmissionUnmigrated) {
				t.Fatalf("a legacy record inside the span was certified: %v",
					err)
			}
			// And nothing was written, so no second grant exists.
			stored, err := harness.store.GetAction(
				context.Background(), harness.storedID(t, "legacy-name"))
			if err == nil && stored.AdmittedIdentityScheme ==
				AdmittedIdentitySchemeDerived {
				t.Fatal("a second live grant was issued")
			}
		})
	}
}

// TestEnqueueIdentityRejectsAMixedScheme pins the second layer under the
// verdict, directly, because nothing reaches it through Request.
//
// If a legacy record sits at the identity the derivation would produce, the
// verdict refuses the whole surface before replay is ever consulted — so the
// scheme comparison inside equivalentEnqueue is unreachable from the outside
// and no end-to-end mutation can observe it. It is kept because it is the thing
// that would refuse such a record as a conflict rather than replaying it as its
// own grant, were the verdict ever bypassed, and tested here so that keeping it
// is a decision rather than an assumption.
func TestEnqueueIdentityRejectsAMixedScheme(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	derived := legacyAdmission(harness, "name", "owner", "actor")
	derived.ID = harness.storedID(t, "name")
	derived.AdmittedIdentityScheme = AdmittedIdentitySchemeDerived

	legacy := cloneActionRecord(derived)
	legacy.AdmittedIdentityScheme = 0

	if !equivalentEnqueue(derived, derived) {
		t.Fatal("a record is not equivalent to itself")
	}
	if equivalentEnqueue(legacy, derived) {
		t.Fatal("a superseded-scheme record is equivalent to a derived one")
	}
	if equivalentEnqueue(derived, legacy) {
		t.Fatal("the comparison is not symmetric in the scheme")
	}
}

// TestOrdinaryActionInsideTheSpanIsDetected pins the other direction of the
// same root cause.
//
// Action identities are arbitrary non-empty bytes and always have been, so the
// span was a legal place to store an ordinary action before it was reserved.
// Nothing silently mishandles one now — the listings exclude admissions by
// marker rather than by range, and ExecuteClaim no longer refuses an identity
// for its shape — but enqueue refuses the span unconditionally, so such an
// action cannot be enqueue-replayed. The verdict is what makes that visible to
// an operator instead of leaving it latent.
func TestOrdinaryActionInsideTheSpanIsDetected(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	squatted := append([]byte(admissionIDPrefix), []byte("ordinary")...)
	harness.store.records[string(squatted)] = ActionRecord{
		ID: squatted, IdempotencyKey: []byte("key"), Version: 1,
		State: DispatchQueued, AgentID: "agent", AgentGeneration: 1,
		Capability: "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object", Input: json.RawMessage(`{"a":1}`),
		Subject: "owner", Actor: "actor",
		PolicyGeneration: 1, AuthorizationExpiresAt: harness.now.Add(time.Hour),
		AuthorizedOperations: []auth.Operation{auth.OperationDispatch},
		RequestID:            "request",
		Reason:               interaction.Reason{Code: "operator_request"},
		ExecutorKey:          []byte("executor"),
		Deadline:             harness.now.Add(time.Hour),
		CreatedAt:            harness.now, UpdatedAt: harness.now,
	}
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "anything", "complete",
			Effects{EffectEgressesContent}, nil),
	); !errors.Is(err, ErrAdmissionSpanOccupied) {
		t.Fatalf("an ordinary action inside the span went undetected: %v", err)
	}
	// The two rollout conditions answer differently, because the remedies
	// differ and an operator reading one should not go hunting for the other.
	if errors.Is(ErrAdmissionSpanOccupied, ErrAdmissionUnmigrated) {
		t.Fatal("the two rollout conditions are the same error")
	}
}

// TestOrdinaryActionInsideTheSpanStaysReachable pins what the span rules may no
// longer do to a record that predates them.
//
// It must still be listed and still be executable. Those were both broken: the
// listings skipped the span by key range, and ExecuteClaim refused any identity
// in it before reading anything. A record created legitimately became invisible
// and unexecutable, with nothing to say so.
func TestOrdinaryActionInsideTheSpanStaysReachable(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	worker := bindDecision(t, harness.authority, dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke))
	// Written through the store, because enqueue refuses the span by design:
	// the reservation is a forward rule about what a caller may name, and this
	// record predates it.
	squatted := append([]byte(admissionIDPrefix), []byte("ordinary")...)
	seed, err := harness.dispatch.Enqueue(worker, EnqueueRequest{
		ID: []byte("seed"), IdempotencyKey: []byte("idempotency-seed"),
		AgentID: "agent", AgentGeneration: 1,
		Capability: "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object",
		Input:    json.RawMessage(`{"prompt_digest":"abc"}`),
		Context:  dispatchContext(harness.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	relocated := cloneActionRecord(seed)
	relocated.ID = squatted
	relocated.ExecutorKey = executorKey(squatted, seed.IdempotencyKey)
	harness.store.records[string(squatted)] = relocated
	delete(harness.store.records, string(seed.ID))

	page, err := harness.dispatch.Pull(worker, PullActionsRequest{
		Limit: 16, Context: dispatchContext(harness.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var listed bool
	for _, record := range page.Actions {
		if bytes.Equal(record.ID, squatted) {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("an ordinary action in the span is invisible to pull: %#v",
			page.Actions)
	}
	claimed, err := harness.dispatch.Claim(worker, ClaimRequest{
		ID: squatted, ExpectedVersion: relocated.Version,
		ClaimID: []byte("worker-token"), Lease: time.Minute,
		Context: dispatchContext(harness.now, "request"),
	})
	if err != nil {
		t.Fatalf("an ordinary action in the span cannot be claimed: %v", err)
	}
	if _, err := harness.dispatch.ExecuteClaim(
		harness.context(t, "request"), claimed); err != nil {
		t.Fatalf("an ordinary action in the span cannot be executed: %v", err)
	}
	if harness.executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", harness.executor.calls)
	}
}

// TestAdmissionsSortAfterOrdinaryWork pins the placement that replaced the
// key-range jump.
//
// Filtering by marker is only cheap if a listing meets real work before it
// meets admissions. When admissions sorted first, a listing's scan budget went
// on records it could never return, which is what the jump existed to avoid —
// and the jump is what skipped everything else in the range. The ordering is
// now the mechanism, so it is asserted directly.
func TestAdmissionsSortAfterOrdinaryWork(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	for index := 0; index < 20; index++ {
		if _, err := harness.service.Request(
			harness.context(t, "request"), harness.request(
				"request", fmt.Sprintf("grant-%02d", index), "complete",
				Effects{EffectEgressesContent}, nil)); err != nil {
			t.Fatal(err)
		}
	}
	queued := harness.enqueueDispatchAction(t, "zzz-last-ordinary")

	// One page of one, from the start: the ordinary action comes back even
	// though it sorts after every other ordinary identity in this fixture,
	// because admissions are below none of them. A text identity only shows
	// that much — 0xff beats every letter, so the high end of the identity
	// space needs its own test, which is TestOrdinaryWorkAtTheTopOfTheKeySpace.
	harness.store.scans = 0
	page, err := harness.dispatch.scanDispatchActions(
		context.Background(), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Actions) != 1 ||
		!bytes.Equal(page.Actions[0].ID, queued.ID) {
		t.Fatalf("scan = %#v", page.Actions)
	}
	// And it cost one read, with no range skipping involved.
	if harness.store.scans != 1 {
		t.Fatalf("one page cost %d reads", harness.store.scans)
	}
}

// identityJustBelowTheSpan returns a nameable action identity that sorts above
// every other identity in these fixtures and immediately below the reserved
// span: the prefix with its final byte decremented.
//
// Not padded out to the greatest nameable identity, because padding cannot
// change which side of the span it falls on and the padded form overruns the
// idempotency key bound. What this has to be is high enough that no ordinary
// identity in the store sorts above it, so that if admissions can sort below
// anything, they sort below this.
func identityJustBelowTheSpan() []byte {
	id := []byte(admissionIDPrefix)
	id[len(id)-1]--
	return id
}

// TestOrdinaryWorkAtTheTopOfTheKeySpace is the case a high prefix passes and
// only a maximal one survives.
//
// The span was "\xffshoal.admission\x00", which stopped admissions sorting
// first but did not make them sort last: 's' leaves every byte above it free,
// so an ordinary \xff\xff sorted after every admission in the store. Pull does
// not refill a page its filter emptied, so a worker asking for four records got
// an empty page and a cursor for as long as the admissions lasted, and
// TeamActions spent its bounded discovery budget before reaching the action at
// all. Both are the listing outage that moving the span to 0xff was supposed to
// end, reached from the other end of the span.
//
// The fixture identity is computed from the span rather than written out,
// because which identity is the worst case depends on the span. \xff\xff is
// the reviewer's example and is the right probe for a four-byte span; under a
// one-byte span it is itself reserved, so a literal would fail this test by
// becoming un-enqueueable — reporting a change in the span's width as an
// ordering defect. Deriving it keeps the test about ordering alone.
func TestOrdinaryWorkAtTheTopOfTheKeySpace(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	// More admissions than the page any caller below asks for, so a span that
	// sorts them ahead of the action starves the request rather than merely
	// reordering it.
	for index := 0; index < 40; index++ {
		if _, err := harness.service.Request(
			harness.context(t, "request"), harness.request(
				"request", fmt.Sprintf("grant-%02d", index), "complete",
				Effects{EffectEgressesContent}, nil)); err != nil {
			t.Fatal(err)
		}
	}
	queued := harness.enqueueDispatchAction(t, string(identityJustBelowTheSpan()))

	// The ordering property itself is asserted by
	// TestEveryAdmissionSortsAboveEveryNameableIdentity. This test asserts only
	// what a caller observes, so that it fails as the listing outage it is
	// rather than as a statement about a constant.
	//
	// Pull: one page, asked for four, with forty admissions in the store.
	worker := bindDecision(t, harness.authority, dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke))
	pulled, err := harness.dispatch.Pull(worker, PullActionsRequest{
		Limit: 4, Context: dispatchContext(harness.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pulled.Actions) != 1 ||
		!bytes.Equal(pulled.Actions[0].ID, queued.ID) {
		t.Fatalf("pull starved by the admission tail = %#v", pulled.Actions)
	}

	// TeamActions: a bounded discovery budget, spent one record at a time.
	reader := bindDecision(t, harness.authority, dispatchDecision(
		t, "reader", "reader", "team", auth.OperationTeamOverviewRead))
	team, err := harness.dispatch.TeamActions(reader, TeamActionListRequest{
		Limit: 10, SourceIDs: [][]byte{[]byte("source")},
		PolicyIDs: [][]byte{[]byte("policy")},
		Context: RequestContext{
			RequestID: "team", CorrelationID: "correlation",
			ReasonCode: "team_overview", Deadline: harness.now.Add(time.Hour),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(team.Actions) != 1 ||
		!bytes.Equal(team.Actions[0].ID, queued.ID) {
		t.Fatalf("team listing starved by the admission tail = %#v",
			team.Actions)
	}
}

// TestEveryAdmissionSortsAboveEveryNameableIdentity pins maximality as a
// property of the constant rather than of the fixtures that depend on it.
//
// The listing tests each pick one high identity and show it survives. That
// catches a span above the identity picked and misses a span above only that
// one, which is how "\xffshoal.admission\x00" passed a suite containing a
// test named for this exact ordering. The guarantee is about every byte of the
// prefix, so it is asserted that way: a prefix is maximal iff decrementing any
// byte of it yields something that sorts below it, and a non-maximal prefix has
// at least one byte whose successor values are nameable and above.
func TestEveryAdmissionSortsAboveEveryNameableIdentity(t *testing.T) {
	prefix := []byte(admissionIDPrefix)
	for index := range prefix {
		// Pad with 0xff to the length bound: the greatest identity sharing the
		// first `index` bytes of the prefix and differing at `index`.
		candidate := append(
			append([]byte(nil), prefix[:index]...), prefix[index]+1)
		for len(candidate) < MaxActionIDBytes {
			candidate = append(candidate, 0xff)
		}
		// 0xff+1 wraps to 0, which sorts below rather than above, and that is
		// precisely the byte value that leaves nothing nameable above it.
		if prefix[index] != 0xff {
			t.Fatalf("prefix byte %d is %#x, so %x is nameable and "+
				"sorts above every admission",
				index, prefix[index], candidate)
		}
	}
}

// TestScanDoubleWalksIdentitiesInOrder pins the fixture property the listing
// tests rest on, deterministically.
//
// The double iterated its map, which is to say in a random order per run. That
// is why a cursor defect whose trigger was "admission identities sort first"
// survived the suite: the double could not express "first". Asserting the
// ordering through a listing test instead would only catch it by luck — with
// forty admissions and one action the unordered double still puts an admission
// first about ninety-seven times in a hundred, which is a flaky catch rather
// than a catch.
func TestScanDoubleWalksIdentitiesInOrder(t *testing.T) {
	store := newMemoryDispatchStore()
	for _, id := range []string{"c", "a", "\x00zero", "b"} {
		store.records[id] = ActionRecord{ID: []byte(id)}
	}
	page, err := store.ScanActions(context.Background(), nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"\x00zero", "a", "b", "c"}
	if len(page.Actions) != len(want) {
		t.Fatalf("scanned %d records, want %d", len(page.Actions), len(want))
	}
	for index, record := range page.Actions {
		if string(record.ID) != want[index] {
			t.Fatalf("scan order = %q at %d, want %q",
				record.ID, index, want[index])
		}
	}
}

// TestListingsStillWorkWithAdmissionsInTheStore is the test whose absence let a
// guard disable the surface it was guarding.
//
// Nothing listed actions with an admission present, so a per-record skip that
// stranded the scan cursor passed the whole suite. It did not merely degrade
// the listing: admission identities sort before essentially every ordinary one,
// the skip bypassed the cursor advance at the bottom of the loop, and the first
// admission was therefore rescanned to the scan bound and nothing after it was
// ever reached. Every deployment holding one grant lost TeamActions entirely.
//
// Many admissions, because one proves nothing: a budget spent one record at a
// time fails only once the admissions outnumber the page, and one admission per
// model call makes that the normal condition.
func TestListingsStillWorkWithAdmissionsInTheStore(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	for index := 0; index < 40; index++ {
		if _, err := harness.service.Request(
			harness.context(t, "request"), harness.request(
				"request", fmt.Sprintf("grant-%02d", index), "complete",
				Effects{EffectEgressesContent}, nil)); err != nil {
			t.Fatal(err)
		}
	}
	queued := harness.enqueueDispatchAction(t, "real-work")

	// The fixture's premise, asserted rather than assumed: the raw store hands
	// back the ordinary action before any admission, and hands an admission
	// back last. That ordering is what makes filtering by marker cheap — a
	// listing meets real work first — and it is the reason the key-range jump
	// could be deleted. Asserting it here keeps the ordering load-bearing
	// rather than incidental.
	raw, err := harness.store.ScanActions(context.Background(), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Actions) != 1 || raw.Actions[0].isAdmission() {
		t.Fatalf("the fixture does not put ordinary work first: %#v",
			raw.Actions)
	}
	tail, err := harness.store.ScanActions(
		context.Background(), nil, MaxDispatchListResults)
	if err != nil {
		t.Fatal(err)
	}
	if last := tail.Actions[len(tail.Actions)-1]; !last.isAdmission() {
		t.Fatalf("admissions do not sort last: %q", last.ID)
	}

	worker := bindDecision(t, harness.authority, dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke))
	pulled, err := harness.dispatch.Pull(worker, PullActionsRequest{
		Limit: 4, Context: dispatchContext(harness.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pulled.Actions) != 1 ||
		!bytes.Equal(pulled.Actions[0].ID, queued.ID) {
		t.Fatalf("pull with admissions present = %#v", pulled.Actions)
	}

	reader := bindDecision(t, harness.authority, dispatchDecision(
		t, "reader", "reader", "team", auth.OperationTeamOverviewRead))
	team, err := harness.dispatch.TeamActions(reader, TeamActionListRequest{
		Limit: 10, SourceIDs: [][]byte{[]byte("source")},
		PolicyIDs: [][]byte{[]byte("policy")},
		Context: RequestContext{
			RequestID: "team", CorrelationID: "correlation",
			ReasonCode: "team_overview", Deadline: harness.now.Add(time.Hour),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(team.Actions) != 1 ||
		!bytes.Equal(team.Actions[0].ID, queued.ID) {
		t.Fatalf("team listing with admissions present = %#v", team.Actions)
	}
	for _, record := range team.Actions {
		if record.isAdmission() {
			t.Fatal("an admission reached a team-overview reader")
		}
	}
}

// TestExecuteClaimAnswersAlikeForAReservedIdentity pins the enumeration that
// survived on this path.
//
// ExecuteClaim takes the record rather than an ID, and checks the *passed*
// record's principal — so an attacker supplies its own principal with the
// victim's identity. An absent identity came back as the store's own not-found
// sentinel and a stored admission as object-not-found: two answers, which is
// the enumeration this work exists to close.
func TestExecuteClaimAnswersAlikeForAReservedIdentity(t *testing.T) {
	message := func(t *testing.T, occupied bool) string {
		t.Helper()
		harness := newAdmissionHarness(t, nil)
		victim := bindDecision(t, harness.authority, dispatchDecision(
			t, "victim", "victim", "request",
			auth.OperationInvoke, auth.OperationRetrieve))
		target := admissionActionID(
			dispatchDecision(t, "victim", "victim", "request",
				auth.OperationInvoke), []byte("secret-name"))
		if occupied {
			if _, err := harness.service.Request(victim, harness.request(
				"request", "secret-name", "complete",
				Effects{EffectEgressesContent}, nil)); err != nil {
				t.Fatal(err)
			}
		}
		// The attacker's own principal, the victim's identity, and the shape a
		// fresh grant has.
		synthesised := ActionRecord{
			ID: target, IdempotencyKey: []byte("probe"), Version: 1,
			State: DispatchClaimed, AgentID: "agent", AgentGeneration: 1,
			Capability: "model", Action: "complete",
			SourceID: []byte("source"), PolicyID: []byte("policy"),
			ObjectID: "object", Input: json.RawMessage(`{"a":1}`),
			Subject: "owner", Actor: "actor",
			ClaimID: []byte("token"), ClaimFence: 1, ClaimLease: time.Minute,
			ClaimLeaseUntil: harness.now.Add(time.Minute),
			Deadline:        harness.now.Add(time.Hour),
			CreatedAt:       harness.now, UpdatedAt: harness.now,
		}
		_, err := harness.dispatch.ExecuteClaim(
			harness.context(t, "request"), synthesised)
		if err == nil {
			t.Fatal("a reserved identity was executed")
		}
		return err.Error()
	}
	occupied, absent := message(t, true), message(t, false)
	if occupied != absent {
		t.Fatalf("execute claim distinguishes a stored admission from an "+
			"absent one:\n occupied: %q\n absent:   %q", occupied, absent)
	}

	// The same pair for *unreserved* identities, which is the half the reserved
	// check cannot cover: an admission written before identities carried the
	// prefix against an ordinary identity that was never written. Only the
	// absence normalisation makes these two answer alike.
	unprefixed := func(t *testing.T, stored bool) string {
		t.Helper()
		harness := newAdmissionHarness(t, nil)
		legacy := ActionRecord{
			ID: []byte("legacy-admission"), IdempotencyKey: []byte("key"),
			Version: 1, State: DispatchClaimed, AgentID: "agent",
			AgentGeneration: 1, Capability: "model", Action: "complete",
			SourceID: []byte("source"), PolicyID: []byte("policy"),
			ObjectID: "object", Input: json.RawMessage(`{"a":1}`),
			Subject: "owner", Actor: "actor",
			ClaimID: []byte("token"), ClaimFence: 1, ClaimLease: time.Minute,
			ClaimLeaseUntil: harness.now.Add(time.Minute),
			Deadline:        harness.now.Add(time.Hour),
			CreatedAt:       harness.now, UpdatedAt: harness.now,
			AdmittedEffects: Effects{EffectEgressesContent},
		}
		if stored {
			harness.store.records[string(legacy.ID)] = legacy
		} else {
			legacy.ID = []byte("never-written")
		}
		_, err := harness.dispatch.ExecuteClaim(
			harness.context(t, "request"), legacy)
		if err == nil {
			t.Fatal("an unprefixed admission was executed")
		}
		return err.Error()
	}
	storedLegacy, absentPlain := unprefixed(t, true), unprefixed(t, false)
	if storedLegacy != absentPlain {
		t.Fatalf("execute claim distinguishes a pre-prefix admission from an "+
			"absent identity:\n stored: %q\n absent: %q",
			storedLegacy, absentPlain)
	}
}

// TestDispatchRefusesAdmissionsWrittenBeforeThePrefix pins that the durable
// marker, not the identity, is what finally names an admission.
//
// An admission written before identities carried the prefix has an unprefixed
// identity that no name-based check can recognise. Any environment that ran the
// admission surface before the region existed holds them, so the prefix check
// cannot stand alone.
func TestDispatchRefusesAdmissionsWrittenBeforeThePrefix(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	legacy := ActionRecord{
		ID: []byte("legacy-admission"), IdempotencyKey: []byte("key"),
		Version: 1, State: DispatchClaimed, AgentID: "agent",
		AgentGeneration: 1, Capability: "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object", Input: json.RawMessage(`{"a":1}`),
		Subject: "owner", Actor: "actor",
		ClaimID: []byte("token"), ClaimFence: 1, ClaimLease: time.Minute,
		ClaimLeaseUntil: harness.now.Add(time.Minute),
		Deadline:        harness.now.Add(time.Hour),
		CreatedAt:       harness.now, UpdatedAt: harness.now,
		// The marker, with no prefix on the identity.
		AdmittedEffects: Effects{EffectEgressesContent},
	}
	if reservedAdmissionID(legacy.ID) {
		t.Fatal("the fixture is not an unprefixed identity")
	}
	harness.store.records[string(legacy.ID)] = legacy

	worker := bindDecision(t, harness.authority, dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke))
	if _, err := harness.dispatch.ExecuteClaim(
		harness.context(t, "request"), legacy,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("execute claim of a pre-prefix admission = %v", err)
	}
	if harness.executor.calls != 0 {
		t.Fatalf("a pre-prefix admission was executed: %d calls",
			harness.executor.calls)
	}
	// The same record with the marker stripped from the *argument*. The stored
	// record still carries it, and the guard reads the stored one — so a caller
	// that fabricates a record without the declaration does not dodge it. For a
	// prefixed identity the reserved check would catch this anyway; for an
	// unprefixed one the marker read from the store is the only thing left.
	stripped := legacy
	stripped.AdmittedEffects = nil
	stripped.AdmittedDisclosures = nil
	stripped.AdmittedObligation = nil
	if _, err := harness.dispatch.ExecuteClaim(
		harness.context(t, "request"), stripped,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("execute claim of a stripped pre-prefix admission = %v", err)
	}
	if harness.executor.calls != 0 {
		t.Fatalf("a stripped pre-prefix admission was executed: %d calls",
			harness.executor.calls)
	}
	if _, err := harness.dispatch.CompleteClaim(worker, CompletionRequest{
		ID: legacy.ID, ExpectedVersion: 1, ClaimID: legacy.ClaimID,
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: dispatchContext(harness.now, "request"),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("completion of a pre-prefix admission = %v", err)
	}
	// And it stays out of the listings, which filter on the marker too.
	page, err := harness.dispatch.Pull(worker, PullActionsRequest{
		Limit: 16, Context: dispatchContext(harness.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range page.Actions {
		if bytes.Equal(record.ID, legacy.ID) {
			t.Fatal("a pre-prefix admission was offered as dispatch work")
		}
	}
}

// TestAdmissionRegionIsAPrefixNotASubstring pins that the reservation is
// anchored at the start of the identity.
//
// A substring match would reserve every ID that happens to contain the marker
// anywhere, refusing legitimate dispatch IDs a caller is entitled to use — and
// a reservation that eats valid names is a denial of service rather than a
// boundary.
func TestAdmissionRegionIsAPrefixNotASubstring(t *testing.T) {
	if !reservedAdmissionID(append(
		[]byte(admissionIDPrefix), []byte("tail")...)) {
		t.Fatal("an identity in the region is not recognised")
	}
	embedded := append([]byte("x"), admissionIDPrefix...)
	if reservedAdmissionID(embedded) {
		t.Fatalf("an identity merely containing the marker is reserved: %q",
			embedded)
	}
	harness := newAdmissionHarness(t, nil)
	worker := bindDecision(t, harness.authority, dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke))
	if _, err := harness.dispatch.Enqueue(worker, EnqueueRequest{
		ID: embedded, IdempotencyKey: []byte("idempotency-embedded"),
		AgentID: "agent", AgentGeneration: 1,
		Capability: "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object",
		Input:    json.RawMessage(`{"prompt_digest":"abc"}`),
		Context:  dispatchContext(harness.now, "request"),
	}); err != nil {
		t.Fatalf("a legitimate ID containing the marker was refused: %v", err)
	}
}

// TestDispatchCannotProbeAnAdmissionIdentity is the probe the earlier fix
// missed, and it goes through Enqueue rather than the admission surface.
//
// The derivation is unkeyed over a principal tuple that is knowable, so an
// attacker can compute a victim's admission ID. It then does not need the
// admission endpoint at all: dispatch enqueue takes an arbitrary ID, raw-reads
// it with no principal check, and answered conflict for an occupied one and
// success for an unheld one. That is the oracle, and squatting an unheld ID
// would additionally deny the victim its own admission by name.
//
// The answer is now the same whatever is stored there, which is why the probe
// compares encoded answers rather than asserting "it errors".
func TestDispatchCannotProbeAnAdmissionIdentity(t *testing.T) {
	enqueueAt := func(t *testing.T, occupied bool) map[string]any {
		t.Helper()
		harness := newAdmissionHarness(t, nil)
		victim := bindDecision(t, harness.authority, dispatchDecision(
			t, "victim", "victim", "request",
			auth.OperationInvoke, auth.OperationRetrieve))
		victimID := admissionActionID(
			dispatchDecision(t, "victim", "victim", "request",
				auth.OperationInvoke),
			[]byte("secret-name"))
		if occupied {
			if _, err := harness.service.Request(victim, harness.request(
				"request", "secret-name", "complete",
				Effects{EffectEgressesContent}, nil)); err != nil {
				t.Fatal(err)
			}
			if _, err := harness.store.GetAction(
				context.Background(), victimID); err != nil {
				t.Fatalf("victim admission is not where the attacker looks: %v", err)
			}
		}
		attacker := bindDecision(t, harness.authority, dispatchDecision(
			t, "owner", "actor", "request",
			auth.OperationDispatch, auth.OperationInvoke))
		_, err := harness.dispatch.Enqueue(attacker, EnqueueRequest{
			ID: victimID, IdempotencyKey: []byte("probe"),
			AgentID: "agent", AgentGeneration: 1,
			Capability: "model", Action: "complete",
			SourceID: []byte("source"), PolicyID: []byte("policy"),
			ObjectID: "object",
			Input:    json.RawMessage(`{"prompt_digest":"abc"}`),
			Context:  dispatchContext(harness.now, "request"),
		})
		if err == nil {
			t.Fatal("dispatch enqueue squatted an admission identity")
		}
		return map[string]any{
			"error":            err.Error(),
			"invalid_argument": shoal.IsErrorCode(err, shoal.ErrorInvalidArgument),
			"conflict":         shoal.IsErrorCode(err, shoal.ErrorConflict),
			"not_found":        shoal.IsErrorCode(err, shoal.ErrorNotFound),
		}
	}
	disclosureconformance.Run(t, disclosureconformance.Probe{
		Name:     "dispatch/enqueue-at-an-admission-identity",
		Withheld: enqueueAt(t, true),
		Control:  enqueueAt(t, false),
	})
}

// TestDispatchSurfacesAnswerAlikeForAnAdmissionIdentity pins the uniformity the
// guards buy: every dispatch entry point that takes an action ID gives one
// answer for an admission-region identity, whether it is absent, another
// principal's, or the caller's own. A caller that can separate those three has
// the oracle back in a different shape.
func TestDispatchSurfacesAnswerAlikeForAnAdmissionIdentity(t *testing.T) {
	type answer struct {
		claim    string
		cancel   string
		complete string
		status   string
	}
	probe := func(t *testing.T, kind string) answer {
		t.Helper()
		harness := newAdmissionHarness(t, nil)
		var target []byte
		switch kind {
		case "absent":
			target = admissionActionID(
				dispatchDecision(t, "owner", "actor", "request",
					auth.OperationInvoke), []byte("never-requested"))
		case "foreign":
			other := bindDecision(t, harness.authority, dispatchDecision(
				t, "victim", "victim", "request",
				auth.OperationInvoke, auth.OperationRetrieve))
			grant, err := harness.service.Request(other, harness.request(
				"request", "theirs", "complete",
				Effects{EffectEgressesContent}, nil))
			if err != nil {
				t.Fatal(err)
			}
			target = grant.Token.ActionID
		case "own":
			grant, err := harness.service.Request(
				harness.context(t, "request"), harness.request(
					"request", "mine", "complete",
					Effects{EffectEgressesContent}, nil))
			if err != nil {
				t.Fatal(err)
			}
			target = grant.Token.ActionID
		}
		caller := bindDecision(t, harness.authority, dispatchDecision(
			t, "owner", "actor", "request",
			auth.OperationDispatch, auth.OperationInvoke))
		message := func(err error) string {
			if err == nil {
				return "<accepted>"
			}
			return err.Error()
		}
		_, claimErr := harness.dispatch.Claim(caller, ClaimRequest{
			ID: target, ExpectedVersion: 1, ClaimID: []byte("probe"),
			Lease: time.Minute, Context: dispatchContext(harness.now, "request"),
		})
		_, cancelErr := harness.dispatch.Cancel(caller, CancelRequest{
			ID: target, ExpectedVersion: 1, MutationKey: []byte("probe"),
			Context: dispatchContext(harness.now, "request"),
		})
		_, completeErr := harness.dispatch.CompleteClaim(
			caller, CompletionRequest{
				ID: target, ExpectedVersion: 1, ClaimID: []byte("probe"),
				Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
				Context: dispatchContext(harness.now, "request"),
			})
		_, statusErr := harness.dispatch.Status(caller, StatusRequest{
			ID: target, Context: dispatchContext(harness.now, "request"),
		})
		return answer{
			claim: message(claimErr), cancel: message(cancelErr),
			complete: message(completeErr), status: message(statusErr),
		}
	}
	absent := probe(t, "absent")
	for _, kind := range []string{"foreign", "own"} {
		got := probe(t, kind)
		if got != absent {
			t.Fatalf("%s admission answers differently from an absent one:\n"+
				" %s: %#v\n absent: %#v", kind, kind, got, absent)
		}
	}
}

// TestAdmissionAnswersAreIndistinguishableAcrossPrincipals is the enumeration
// pin, and it compares encoded answers rather than statuses.
//
// The bug it covers returned a 200 grant for a name nobody held and a 409
// conflict for a name another principal held, so probing names enumerated other
// principals' admissions. A test asserting "both succeed" would pass against a
// version that merely returned the same status while differing in the token, or
// the outcome, or a field added later — so the whole encoded answer is
// compared, with the token normalised only for the request identity that
// legitimately varies between two probes.
func TestAdmissionAnswersAreIndistinguishableAcrossPrincipals(t *testing.T) {
	probe := func(t *testing.T, occupied bool) AdmissionGrant {
		t.Helper()
		harness := newAdmissionHarness(t, nil)
		if occupied {
			theirs := bindDecision(t, harness.authority, dispatchDecision(
				t, "other", "other", "request",
				auth.OperationInvoke, auth.OperationRetrieve))
			if _, err := harness.service.Request(theirs, harness.request(
				"request", "probe-target", "complete",
				Effects{EffectEgressesContent}, nil)); err != nil {
				t.Fatal(err)
			}
		}
		grant, err := harness.service.Request(
			harness.context(t, "request"), harness.request(
				"request", "probe-target", "complete",
				Effects{EffectEgressesContent}, nil))
		if err != nil {
			t.Fatalf("probe (occupied=%v) = %v", occupied, err)
		}
		return grant
	}
	disclosureconformance.Run(t, disclosureconformance.Probe{
		Name:     "admission/request-name-held-by-another-principal",
		Withheld: admissionProbeResponse(probe(t, true)),
		Control:  admissionProbeResponse(probe(t, false)),
	})
}

// admissionProbeResponse renders a grant the way a caller receives it, so the
// comparison covers every field rather than the ones a test remembered to name.
func admissionProbeResponse(grant AdmissionGrant) map[string]any {
	withhold := make([]string, 0, len(grant.Obligations.Withhold))
	for _, reference := range grant.Obligations.Withhold {
		withhold = append(withhold, string(reference))
	}
	return map[string]any{
		"outcome":    string(grant.Outcome),
		"action_id":  grant.Token.ActionID,
		"token_id":   grant.Token.TokenID,
		"version":    grant.Token.Version,
		"expires_at": grant.Token.ExpiresAt,
		"withhold":   withhold,
	}
}

// TestAdmissionReportRefusalsAreIndistinguishable pins the same property on the
// report path, which was already correct and is easy to regress: an absent
// admission and another principal's admission must produce the same bytes, not
// merely the same status.
func TestAdmissionReportRefusalsAreIndistinguishable(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	theirs := bindDecision(t, harness.authority, dispatchDecision(
		t, "other", "other", "request",
		auth.OperationInvoke, auth.OperationRetrieve))
	foreign, err := harness.service.Request(theirs, harness.request(
		"request", "theirs", "complete",
		Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatal(err)
	}
	report := func(actionID []byte) map[string]any {
		_, err := harness.service.Report(
			harness.context(t, "report"), AdmissionReport{
				Token: AdmissionToken{
					ActionID: actionID, TokenID: []byte("token-theirs"),
					Version: 1,
				},
				Outcome: json.RawMessage(`{"tokens":1}`),
				Context: dispatchContext(harness.now, "report"),
			})
		return map[string]any{
			"error":     err.Error(),
			"not_found": shoal.IsErrorCode(err, shoal.ErrorNotFound),
			"conflict":  shoal.IsErrorCode(err, shoal.ErrorConflict),
		}
	}
	disclosureconformance.Run(t, disclosureconformance.Probe{
		Name:     "admission/report-token-of-another-principal",
		Withheld: report(foreign.Token.ActionID),
		Control:  report(harness.storedID(t, "never-existed")),
	})
}

// TestAdmissionDisclosureDigestSeparatesDistinctSets pins the framing. Without
// a length prefix per element, two different declarations concatenate to the
// same bytes and a retry could swap one for the other under a live token.
func TestAdmissionDisclosureDigestSeparatesDistinctSets(t *testing.T) {
	left := disclosureDigest([]shoal.ID{"ab", "c"})
	right := disclosureDigest([]shoal.ID{"a", "bc"})
	if bytes.Equal(left, right) {
		t.Fatalf("distinct declarations digest identically: %x", left)
	}
	if disclosureDigest(nil) != nil {
		t.Fatal("an empty declaration must digest to nothing")
	}
	if !bytes.Equal(left, disclosureDigest([]shoal.ID{"ab", "c"})) {
		t.Fatal("the same declaration must digest identically")
	}
}

// TestAdmittedDeclarationShapeIsValidated pins what a durable record may say it
// admitted. Membership is deliberately not checked — an unrecognised class is
// refused at resolution, and refusing to decode would lose the audit trail for
// exactly the admissions most worth reading — but shape is, because shape is
// what makes two declarations comparable.
func TestAdmittedDeclarationShapeIsValidated(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	base := ActionRecord{
		ID: []byte("action"), IdempotencyKey: []byte("key"), Version: 1,
		State: DispatchQueued, AgentID: "agent", AgentGeneration: 1,
		Capability: "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object", Input: json.RawMessage(`{}`),
		Subject: "owner", Actor: "actor", PolicyGeneration: 1,
		AuthorizationExpiresAt: now.Add(time.Hour), RequestID: "request",
		Reason:               mustReason(t),
		AuthorizedOperations: []auth.Operation{auth.OperationInvoke},
		ExecutorKey:          []byte("executor"),
		Deadline:             now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("baseline record = %v", err)
	}

	unordered := base
	unordered.AdmittedEffects = Effects{
		EffectReadsCorpus, EffectEgressesContent}
	if err := unordered.Validate(); err == nil {
		t.Fatal("accepted a non-canonical declared effect set")
	}
	duplicated := base
	duplicated.AdmittedEffects = Effects{
		EffectReadsCorpus, EffectReadsCorpus}
	if err := duplicated.Validate(); err == nil {
		t.Fatal("accepted a duplicated declared effect")
	}
	unknown := base
	unknown.AdmittedEffects = Effects{"invented"}
	if err := unknown.Validate(); err != nil {
		t.Fatalf("refused to decode an unrecognised class: %v", err)
	}

	stunted := base
	stunted.AdmittedEffects = Effects{EffectEgressesContent}
	stunted.AdmittedDisclosures = []byte("not-a-digest")
	if err := stunted.Validate(); err == nil {
		t.Fatal("accepted a disclosure field that is not a digest")
	}
	orphaned := base
	orphaned.AdmittedDisclosures = disclosureDigest([]shoal.ID{"doc-a"})
	if err := orphaned.Validate(); err == nil {
		t.Fatal("accepted declared references with no declared effect")
	}
	whole := base
	whole.AdmittedEffects = Effects{EffectEgressesContent}
	whole.AdmittedDisclosures = disclosureDigest([]shoal.ID{"doc-a"})
	if err := whole.Validate(); err != nil {
		t.Fatalf("complete declaration = %v", err)
	}

	// An obligation names positions in a declared list. Without the list there
	// is nothing for it to index, and a record carrying one has been assembled
	// by something that did not go through the grant path.
	unanchored := base
	unanchored.AdmittedEffects = Effects{EffectEgressesContent}
	unanchored.AdmittedObligation = []byte{0b0000_0001}
	if err := unanchored.Validate(); err == nil {
		t.Fatal("accepted an obligation with no declared references")
	}
	oversized := whole
	oversized.AdmittedObligation = make(
		[]byte, MaxAdmittedObligationBytes+1)
	if err := oversized.Validate(); err == nil {
		t.Fatal("accepted an obligation wider than the declaration bound")
	}
	obliged := whole
	obliged.AdmittedObligation = []byte{0b0000_0001}
	if err := obliged.Validate(); err != nil {
		t.Fatalf("complete obligation = %v", err)
	}

	// A scheme marker claims how an admission's key was produced, so it is
	// incoherent on a record that is not an admission.
	schemeOnly := base
	schemeOnly.AdmittedIdentityScheme = AdmittedIdentitySchemeDerived
	if err := schemeOnly.Validate(); err == nil {
		t.Fatal("accepted an identity scheme with no admitted effect")
	}
	derived := whole
	derived.AdmittedIdentityScheme = AdmittedIdentitySchemeDerived
	if err := derived.Validate(); err != nil {
		t.Fatalf("complete derived admission = %v", err)
	}
	// A scheme this build does not know still decodes. The verdict refuses
	// anything that is not the derived marker, so an unrecognised value is
	// refused at use rather than made undecodable — the same treatment an
	// unrecognised effect class gets.
	future := whole
	future.AdmittedIdentityScheme = 99
	if err := future.Validate(); err != nil {
		t.Fatalf("a future identity scheme must still decode: %v", err)
	}
}

// TestAdmissionRecordDoesNotAliasItsDeclaration pins that a record handed out
// shares no backing array with the one the store holds.
//
// Copying the struct carries the slice headers on its own, so the declaration
// survives a clone that forgets to reallocate — what it does not survive is a
// caller writing through the header it was given. A record that aliases store
// state lets whatever holds it edit what was admitted after the fact.
func TestAdmissionRecordDoesNotAliasItsDeclaration(t *testing.T) {
	// A restrictor that withholds, so the record carries an obligation as well
	// as a declaration; all three fields have to be independently owned.
	restrictor := &stubRestrictor{allowed: []shoal.ID{"doc-a"}}
	harness := newAdmissionHarness(t, restrictor)
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "complete",
			Effects{EffectEgressesContent},
			[]shoal.ID{"doc-a", "doc-b"})); err != nil {
		t.Fatal(err)
	}
	handed, err := harness.stored(t, "admission")
	if err != nil {
		t.Fatal(err)
	}
	if len(handed.AdmittedEffects) != 1 ||
		len(handed.AdmittedDisclosures) == 0 ||
		len(handed.AdmittedObligation) == 0 {
		t.Fatalf("stored declaration = %#v", handed)
	}
	handed.AdmittedEffects[0] = EffectMutatesExternal
	handed.AdmittedDisclosures[0] ^= 0xff
	handed.AdmittedObligation[0] ^= 0xff

	reread, err := harness.stored(t, "admission")
	if err != nil {
		t.Fatal(err)
	}
	if reread.AdmittedEffects[0] != EffectEgressesContent {
		t.Fatalf("declared effects were edited through the handed record: %#v",
			reread.AdmittedEffects)
	}
	if bytes.Equal(reread.AdmittedDisclosures, handed.AdmittedDisclosures) {
		t.Fatal("declared references were edited through the handed record")
	}
	// The obligation most of all: editing it through a handed-out record would
	// change what a replay tells the caller to withhold.
	if bytes.Equal(reread.AdmittedObligation, handed.AdmittedObligation) {
		t.Fatal("the obligation was edited through the handed record")
	}
}

func mustReason(t *testing.T) interaction.Reason {
	t.Helper()
	reason, err := interaction.NewReason("operator_request", "")
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

// TestAdmissionClampsItsLeaseToTheActionDeadline pins that a token never
// outlives the action it was granted against. A token live past the deadline is
// one the report path will refuse, so issuing it tells a caller it has longer
// than it does.
func TestAdmissionClampsItsLeaseToTheActionDeadline(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	request := harness.request(
		"request", "admission", "complete", Effects{EffectEgressesContent}, nil)
	request.Lease = time.Minute
	request.Context.Deadline = harness.now.Add(30 * time.Second)
	grant, err := harness.service.Request(
		harness.context(t, "request"), request)
	if err != nil {
		t.Fatal(err)
	}
	if !grant.Token.ExpiresAt.Equal(harness.now.Add(30 * time.Second)) {
		t.Fatalf("token expiry = %s, want the action deadline",
			grant.Token.ExpiresAt)
	}
}

// TestAdmissionChargesAPossibleEffectExactlyAsAClaimDoes pins that a grant and
// a plain dispatch claim of the same action agree about what may already have
// happened.
//
// Egress counts. An earlier version of this test asserted the opposite,
// carrying forward a rationale that had already been corrected on the dispatch
// side — so the test defended the drift instead of catching it. A caller
// granted permission to transmit may have transmitted before it went silent,
// and content that left the host cannot be recalled, so a record asserting no
// effect was possible asserts the one thing nobody knows.
func TestAdmissionChargesAPossibleEffectExactlyAsAClaimDoes(t *testing.T) {
	for _, test := range []struct {
		name   string
		id     string
		action string
		effect Effect
	}{
		{"external", "mutating", "publish", EffectMutatesExternal},
		{"egress", "egressing", "complete", EffectEgressesContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newAdmissionHarness(t, nil)
			if _, err := harness.service.Request(
				harness.context(t, "request"), harness.request(
					"request", test.id, test.action,
					Effects{test.effect}, nil)); err != nil {
				t.Fatal(err)
			}
			granted, err := harness.stored(t, test.id)
			if err != nil {
				t.Fatal(err)
			}
			if !granted.EffectPossible {
				t.Fatalf("%s admission left EffectPossible false", test.name)
			}
		})
	}

	// An action that neither transmits nor mutates leaves its whole outcome in
	// Shoal's own record, so nothing has to be assumed about it. Without this
	// the assertion above would pass against a grant that set the flag
	// unconditionally.
	harness := newAdmissionHarness(t, nil)
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "reading", "summarize",
			Effects{EffectReadsCorpus}, nil)); err != nil {
		t.Fatal(err)
	}
	reading, err := harness.stored(t, "reading")
	if err != nil || reading.EffectPossible {
		t.Fatalf("corpus-read admission = %#v, %v", reading, err)
	}
}

// TestAdmissionAndDispatchClaimAgreeOnTheRecord pins the structural fix behind
// that agreement: the two paths write the same claim state because they are the
// same function, not because two conditions were kept in step.
func TestAdmissionAndDispatchClaimAgreeOnTheRecord(t *testing.T) {
	harness := newAdmissionHarness(t, nil)
	if _, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admitted", "complete",
			Effects{EffectEgressesContent}, nil)); err != nil {
		t.Fatal(err)
	}
	admitted, err := harness.stored(t, "admitted")
	if err != nil {
		t.Fatal(err)
	}

	dispatcher := bindDecision(t, harness.authority, dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke))
	queued, err := harness.dispatch.Enqueue(dispatcher, EnqueueRequest{
		ID: []byte("dispatched"), IdempotencyKey: []byte("idempotency-dispatched"),
		AgentID: "agent", AgentGeneration: 1,
		Capability: "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object",
		Input:    json.RawMessage(`{"prompt_digest":"abc"}`),
		Context:  dispatchContext(harness.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := harness.dispatch.Claim(dispatcher, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version,
		ClaimID: []byte("token-dispatched"), Lease: time.Minute,
		Context: dispatchContext(harness.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if admitted.EffectPossible != claimed.EffectPossible {
		t.Fatalf("EffectPossible: admission %v, dispatch claim %v",
			admitted.EffectPossible, claimed.EffectPossible)
	}
	// The execution fingerprint is deliberately not compared. It digests the
	// decision, and the two paths cannot run under the same one: Enqueue
	// requires dispatch authority and an admission requires only invoke. Every
	// other part of the claim state is a function of the action and the lease,
	// so it must match exactly.
	if admitted.State != claimed.State ||
		admitted.ClaimFence != claimed.ClaimFence ||
		admitted.ClaimLease != claimed.ClaimLease ||
		!admitted.ClaimLeaseUntil.Equal(claimed.ClaimLeaseUntil) ||
		admitted.ExecutionPolicyGeneration != claimed.ExecutionPolicyGeneration ||
		!admitted.ExecutionExpiresAt.Equal(claimed.ExecutionExpiresAt) {
		t.Fatalf("claim state diverged:\n admission %#v\n dispatch  %#v",
			admitted, claimed)
	}
}

// TestAdmissionDenialNamesNothing pins the disclosure rule on every refusal
// this surface can produce.
func TestAdmissionDenialNamesNothing(t *testing.T) {
	forbidden := []string{
		"policy", "source", "compartment", "domain", "doc-", "owner", "agent",
	}
	harness := newAdmissionHarness(t, nil)
	grant, err := harness.service.Request(
		harness.context(t, "request"), harness.request(
			"request", "admission", "summarize",
			Effects{EffectEgressesContent}, nil))
	if err != nil {
		t.Fatalf("denial = %v", err)
	}
	if grant.Outcome != AdmissionDenied {
		t.Fatalf("outcome = %q", grant.Outcome)
	}
	for _, err := range []error{ErrAdmissionSpent, ErrAdmissionConflict} {
		for _, term := range forbidden {
			if strings.Contains(err.Error(), term) {
				t.Fatalf("error %q names %q", err, term)
			}
		}
	}
}

// TestAFailureReportsHowMuchEscaped is #427.
//
// Report is either {Outcome} or {Failed, ErrorCode}, and that exclusivity is
// right for an executor: work that failed did not happen, so there is nothing
// to quantify. The proxy is the one boundary where a failure follows a partial
// irreversible effect — a streamed completion that breaks halfway has already
// put bytes in front of the caller and cannot recall them — and the byte count
// was dropped. So three different events arrived identically as
// response_truncated: nothing left, fifty bytes left, two megabytes left. The
// first is a non-event, the third a near-complete disclosure, and #389
// consumes these reports to shape the next decision.
func TestAFailureReportsHowMuchEscaped(t *testing.T) {
	escaped := EffectedVolume{Bytes: 2 << 20, Chunks: 64}

	t.Run("committed on an egressing action", func(t *testing.T) {
		harness := newAdmissionHarness(t, nil)
		grant, err := harness.service.Request(
			harness.context(t, "request"), harness.request(
				"request", "admission", "complete",
				Effects{EffectEgressesContent}, nil))
		if err != nil {
			t.Fatal(err)
		}
		record, err := harness.service.Report(
			harness.context(t, "report"), AdmissionReport{
				Token: grant.Token, Failed: true,
				ErrorCode: "response_truncated", Effected: escaped,
				Context: dispatchContext(harness.now, "report"),
			})
		if err != nil {
			t.Fatalf("a failure carrying a volume was refused: %v", err)
		}
		if record.Effected != escaped {
			t.Fatalf("returned volume = %#v, want %#v", record.Effected, escaped)
		}
		stored, err := harness.stored(t, "admission")
		if err != nil {
			t.Fatal(err)
		}
		if stored.Effected != escaped {
			t.Fatalf("stored volume = %#v, want %#v: the durable record is "+
				"what an operator reconciling the egress reads",
				stored.Effected, escaped)
		}
	})

	// The property the exclusivity rule exists to protect, and the reason a
	// quantity may ride on a failure at all. An outcome was refused there
	// because the completion path *discarded* it, so two failures with the
	// same error code compared equal and a caller could replace what it
	// reported and be told the second was recorded. Effected is committed and
	// compared, so it has none of that.
	t.Run("a different volume is a different report", func(t *testing.T) {
		harness := newAdmissionHarness(t, nil)
		grant, err := harness.service.Request(
			harness.context(t, "request"), harness.request(
				"request", "admission", "complete",
				Effects{EffectEgressesContent}, nil))
		if err != nil {
			t.Fatal(err)
		}
		report := AdmissionReport{
			Token: grant.Token, Failed: true,
			ErrorCode: "response_truncated", Effected: escaped,
			Context: dispatchContext(harness.now, "report"),
		}
		if _, err := harness.service.Report(
			harness.context(t, "report"), report); err != nil {
			t.Fatal(err)
		}
		// The identical report replays as a receipt.
		if _, err := harness.service.Report(
			harness.context(t, "report"), report); err != nil {
			t.Fatalf("an identical replay was refused: %v", err)
		}
		// A different volume is not this report, so the spent token says so
		// rather than quietly accepting a rewritten number.
		lying := report
		lying.Effected = EffectedVolume{Bytes: 1}
		if _, err := harness.service.Report(
			harness.context(t, "report"), lying,
		); !errors.Is(err, ErrAdmissionSpent) {
			t.Fatalf("a failure reporting a different volume = %v, want "+
				"ErrAdmissionSpent: otherwise a caller replaces how much "+
				"escaped and is told the second number was recorded", err)
		}
		stored, err := harness.stored(t, "admission")
		if err != nil {
			t.Fatal(err)
		}
		if stored.Effected != escaped {
			t.Fatalf("the record's volume changed to %#v", stored.Effected)
		}
	})

	for _, probe := range []struct {
		name   string
		action string
		mutate func(*AdmissionReport)
	}{
		{
			// Not a free-form channel. An action the host did not declare may
			// send content outside Shoal has no egress to quantify, and
			// recording a number there would make this a place to write
			// arbitrary values on a durable record.
			name: "an action that does not declare egress", action: "summarize",
			mutate: func(r *AdmissionReport) { r.Effected = escaped },
		},
		{
			// Meaningless on a success: this field answers how much escaped
			// before a failure, not how much a completed call sent.
			name: "a successful report", action: "complete",
			mutate: func(r *AdmissionReport) {
				r.Failed = false
				r.ErrorCode = ""
				r.Outcome = json.RawMessage(`{"prompt_digest":"abc"}`)
				r.Effected = escaped
			},
		},
		{
			name: "a volume over its byte bound", action: "complete",
			mutate: func(r *AdmissionReport) {
				r.Effected = EffectedVolume{Bytes: MaxEffectedBytes + 1}
			},
		},
		{
			name: "a volume over its chunk bound", action: "complete",
			mutate: func(r *AdmissionReport) {
				r.Effected = EffectedVolume{
					Bytes: 1, Chunks: MaxEffectedChunks + 1}
			},
		},
		{
			name: "a negative volume", action: "complete",
			mutate: func(r *AdmissionReport) {
				r.Effected = EffectedVolume{Bytes: -1}
			},
		},
		{
			// A chunk that left carried something, so chunks without bytes
			// describes nothing that can have happened.
			name: "chunks with no bytes", action: "complete",
			mutate: func(r *AdmissionReport) {
				r.Effected = EffectedVolume{Chunks: 3}
			},
		},
	} {
		t.Run("refused: "+probe.name, func(t *testing.T) {
			harness := newAdmissionHarness(t, nil)
			effects := Effects{EffectEgressesContent}
			if probe.action == "summarize" {
				effects = Effects{EffectReadsCorpus}
			}
			grant, err := harness.service.Request(
				harness.context(t, "request"), harness.request(
					"request", "admission", probe.action, effects, nil))
			if err != nil {
				t.Fatal(err)
			}
			report := AdmissionReport{
				Token: grant.Token, Failed: true,
				ErrorCode: "response_truncated",
				Context:   dispatchContext(harness.now, "report"),
			}
			probe.mutate(&report)
			if _, err := harness.service.Report(
				harness.context(t, "report"), report,
			); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("report = %v, want invalid argument", err)
			}
			// Refused before anything committed: the token is still live, so
			// a caller that sends a usable report next is not told its work
			// was already recorded.
			stored, err := harness.stored(t, "admission")
			if err != nil {
				t.Fatal(err)
			}
			if stored.State != DispatchClaimed {
				t.Fatalf("the refused report moved the record to %q",
					stored.State)
			}
			if !stored.Effected.Zero() {
				t.Fatalf("the refused report recorded %#v", stored.Effected)
			}
		})
	}
}

// TestAnAdmissionReportSurvivesALapsedLease is the path a gateway actually
// hits, and the reason the invoke-route half of #577 matters in practice.
//
// An admission is a dispatch action born claimed, and Report completes it
// through completeClaim under executorPhaseComplete. The lapse toleration was
// gated on the execute route, and an admission caller is not on it — so a
// gateway that performed an irreversible egress and then reported it, after
// the operator stopped renewing the descriptor's lease, lost the record of
// the effect.
//
// Revocation is still refused, which is the line this keeps: "stopped
// renewing" and "withdrew the agent" are different statements.
func TestAnAdmissionReportSurvivesALapsedLease(t *testing.T) {
	for _, probe := range []struct {
		name    string
		revoke  bool
		refused bool
	}{
		{name: "a lapsed lease is tolerated"},
		{name: "a revoked descriptor is refused", revoke: true, refused: true},
	} {
		t.Run(probe.name, func(t *testing.T) {
			harness := newAdmissionHarness(t, nil)
			grant, err := harness.service.Request(
				harness.context(t, "request"), harness.request(
					"request", "admission", "complete",
					Effects{EffectEgressesContent}, nil))
			if err != nil {
				t.Fatal(err)
			}
			stored := harness.registryStore.records["agent"]
			descriptor := cloneDescriptor(stored.Descriptor)
			descriptor.LeaseExpiresAt = descriptor.UpdatedAt.Add(-time.Second)
			if probe.revoke {
				descriptor.RevokedAt = descriptor.UpdatedAt
			}
			stored.Descriptor = descriptor
			harness.registryStore.records["agent"] = stored

			_, err = harness.service.Report(
				harness.context(t, "report"), AdmissionReport{
					Token: grant.Token, Failed: true,
					ErrorCode: "target_rejected_502",
					Context:   dispatchContext(harness.now, "report"),
				})
			if probe.refused {
				if err == nil {
					t.Fatal("an admission report resolved against a revoked " +
						"descriptor; tolerating a lapse must not tolerate a " +
						"withdrawal")
				}
				return
			}
			if err != nil {
				t.Fatalf("a gateway's report was refused because the "+
					"descriptor's lease had lapsed, so the egress happened "+
					"and the record does not say so: %v", err)
			}
			// And it committed, rather than being accepted and dropped.
			committed, storeErr := harness.stored(t, "admission")
			if storeErr != nil || committed.State != DispatchFailed ||
				committed.ErrorCode != "target_rejected_502" {
				t.Fatalf("the report was accepted but not recorded: %#v, %v",
					committed, storeErr)
			}
		})
	}
}
