// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// A failed record's ErrorCode was written by the service or by the executor,
// and since #529 ErrorCodeOrigin says which; a record written before it does
// not. Who a reason sentence says assigned the code follows the origin alone,
// never the code (#508):
//
//   - service: Shoal's own determination, stated as such ("Shoal recorded the
//     failure as …, which means …");
//   - executor: the executor's account, a report whose meaning is conditional
//     ("reported as …, which, if accurate, means …");
//   - omitted, or a value this build does not know: attributed to neither, and
//     conditional.
//
// Since #538 a failure's next step also depends on EffectPossible, which on a
// terminal record says what the action DECLARED, not what happened:
//
//   - set: it starts with reconciling with the target, whoever assigned the
//     code — request_not_sent included;
//   - clear, with a gateway code: the code implies a target was involved, so
//     the declaration is contradicted and it still starts with reconciling;
//   - clear, with anything else — fleet's own codes at any origin included,
//     since fleet assigns them after the worker acted: reconcile if the
//     action reached any external system.
//
// No failure ever drops reconciliation.
func TestErrorCodesAreReportsNotFindings(t *testing.T) {
	r := New(nil)
	families := sourceCodeFamilies(t)
	codes := append(sourceErrorCodes(t), "made up by an executor")
	for _, origin := range errorCodeOriginCases(t) {
		for _, code := range codes {
			for _, effect := range []bool{true, false} {
				what := fmt.Sprintf("%s origin=%s effect=%v", code, origin, effect)
				record := failedWith(code, origin)
				record.EffectPossible = effect
				sentences, err := r.Action(record, Options{})
				if err != nil {
					t.Fatal(err)
				}
				nexts := 0
				for _, s := range sentences {
					text := strings.ToLower(s.Text)
					if !strings.HasPrefix(s.Key, "dispatch.error.") {
						continue
					}
					switch s.Role {
					case RoleReason:
						reasonIsFaithful(t, what, code, wantOrigin[origin], s)
					case RoleNext:
						nexts++
						switch {
						case effect || families[code] == "gateway":
							if !strings.HasPrefix(text, "reconcile with the target before") {
								t.Errorf("%s: next step does not start with reconciliation: %s", what, s.Text)
							}
						default:
							if !strings.HasPrefix(text, "reconcile with the target if the action reached any external system") {
								t.Errorf("%s: a failure on a clear flag does not keep "+
									"reconciliation: %s", what, s.Text)
							}
						}
						if strings.Contains(text, "no reconciliation") {
							t.Errorf("%s: a failure drops reconciliation: %s", what, s.Text)
						}
						for _, unsafe := range []string{"without repeating", "safe to", "can be requested again"} {
							if strings.Contains(text, unsafe) {
								t.Errorf("%s: next step advises a retry: %s", what, s.Text)
							}
						}
						nextIsFaithful(t, what, wantOrigin[origin], text)
					}
				}
				if nexts != 1 {
					t.Errorf("%s: %d next steps", what, nexts)
				}
			}
		}
	}
}

// reasonIsFaithful checks a failure's reason sentence says who assigned the
// code exactly as the origin establishes, and no more.
func reasonIsFaithful(t *testing.T, what, code string, want Selector, s Sentence) {
	t.Helper()
	text := strings.ToLower(s.Text)
	_, _, known := errorCodeKey(code)
	if len(s.Refs) == 0 {
		t.Errorf("%s: reason does not name the reporter", what)
	}
	attribution := AttributedToExecutorOrService
	switch want {
	case OriginService:
		attribution = AttributedToService
		if !strings.HasPrefix(text, "shoal recorded the failure") {
			t.Errorf("%s: Shoal's own code is not stated as Shoal's: %s", what, s.Text)
		}
		if strings.Contains(text, "if accurate") || strings.Contains(text, "reported") {
			t.Errorf("%s: Shoal's own code is hedged as a report: %s", what, s.Text)
		}
	case OriginExecutor:
		attribution = AttributedToExecutor
		if !strings.Contains(text, "reported") || strings.Contains(text, "shoal recorded") {
			t.Errorf("%s: the executor's code is not phrased as its report: %s", what, s.Text)
		}
		if known && !strings.Contains(text, "if accurate") {
			t.Errorf("%s: the executor's meaning is stated as fact: %s", what, s.Text)
		}
	default:
		if !strings.Contains(text, "either the executor reported or shoal assigned") ||
			!strings.Contains(text, "does not say which") {
			t.Errorf("%s: a code of unknown origin is not attributed to neither: %s", what, s.Text)
		}
		if strings.Contains(text, "shoal recorded") || strings.Contains(text, "was reported as") {
			t.Errorf("%s: a code of unknown origin is attributed to one side: %s", what, s.Text)
		}
		if known && !strings.Contains(text, "if accurate") {
			t.Errorf("%s: a code of unknown origin is stated as fact: %s", what, s.Text)
		}
	}
	for _, q := range s.Quotes {
		if q.Attribution != attribution {
			t.Errorf("%s: code quoted as %s's, want %s's", what, q.Attribution, attribution)
		}
	}
	if known == (len(s.Quotes) > 0) {
		t.Errorf("%s: a known code is quoted, or an unknown one is not: %s", what, s.Text)
	}
}

// nextIsFaithful checks a failure's next step does not contradict who the
// origin says assigned the code: only the executor's code is "the report",
// a code of unknown origin is only "the code", and Shoal's own code is
// neither second-guessed nor said to be beyond Shoal.
func nextIsFaithful(t *testing.T, what string, want Selector, text string) {
	t.Helper()
	var wrong []string
	switch want {
	case OriginService:
		wrong = []string{"the report says", "the code says", "shoal cannot interpret"}
	case OriginExecutor:
		wrong = []string{"the code says", "this renderer"}
	default:
		wrong = []string{"the report says", "shoal cannot interpret"}
	}
	for _, phrase := range wrong {
		if strings.Contains(text, phrase) {
			t.Errorf("%s: next step says %q under a %s origin: %s", what, phrase, want, text)
		}
	}
}

// failureSentences renders a failed record and the transition that failed it.
func failureSentences(t *testing.T, r *Renderer, record fleet.ActionRecord) []Sentence {
	t.Helper()
	sentences, err := r.Action(record, Options{})
	if err != nil {
		t.Fatal(err)
	}
	history, err := r.ActionHistory([]fleet.ActionTransition{
		{ID: []byte("t1"), Kind: "action.failed", Record: record},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return append(sentences, history...)
}

// TestAnUnknownOriginIsNeverTheExecutors: a record written before #529 has no
// origin, and its code may have been the service's (invalid_executor_output,
// say) or the executor's. Reading it as the executor's would put Shoal's
// adjudication in the worker's mouth, or a worker's claim in nobody's, so it
// is attributed to neither — and so is any origin this build does not know,
// which is how a record from a newer build arrives.
func TestAnUnknownOriginIsNeverTheExecutors(t *testing.T) {
	r := New(nil)
	unknown := []fleet.ErrorCodeOrigin{
		fleet.ErrorCodeOriginUnknown, futureOrigin,
		"Executor", " executor", "executor\x00", "service ",
	}
	for _, origin := range unknown {
		if got := errorOrigin(origin); got != OriginEither {
			t.Errorf("origin %q is narrated as %q, not as either", origin, got)
		}
	}
	for _, code := range append(sourceErrorCodes(t), "made up by an executor") {
		omitted := failureSentences(t, r, failedWith(code, fleet.ErrorCodeOriginUnknown))
		executor := failureSentences(t, r, failedWith(code, fleet.ErrorCodeOriginExecutor))
		if len(omitted) != len(executor) {
			t.Fatalf("%s: %d sentences, %d for the executor's", code, len(omitted), len(executor))
		}
		for i, s := range omitted {
			text := strings.ToLower(s.Text)
			for _, phrase := range []string{"reported as failed by", "reported failure with", "was reported as", "shoal recorded"} {
				if strings.Contains(text, phrase) {
					t.Errorf("%s: an omitted origin is attributed (%q): %s", code, phrase, s.Text)
				}
			}
			for _, q := range s.Quotes {
				if q.Attribution == AttributedToExecutor || q.Attribution == AttributedToService {
					t.Errorf("%s: an omitted origin's code is quoted as %s's", code, q.Attribution)
				}
			}
			// Where the executor's account reads differently, the omitted
			// one must not read like it: the outcome, the reason and the
			// failing transition.
			switch s.Role {
			case RoleOutcome, RoleReason:
				if s.Text == executor[i].Text {
					t.Errorf("%s: an omitted origin reads as the executor's: %s", code, s.Text)
				}
			case RoleHistory:
				if s.Key == "dispatch.transition.fail" && s.Text == executor[i].Text {
					t.Errorf("%s: an omitted origin reads as the executor's: %s", code, s.Text)
				}
			}
		}
		for _, origin := range unknown[1:] {
			other := failureSentences(t, r, failedWith(code, origin))
			if dump(other) != dump(omitted) {
				t.Errorf("%s: origin %q renders differently from an omitted one:\n%s\n---\n%s",
					code, origin, dump(other), dump(omitted))
			}
		}
	}
}

// TestShoalIsCreditedOnlyWithTheCode: an origin of service says Shoal
// assigned the error code, and nothing more. In particular it does not say
// the executor reported a failure — it may have reported success, with
// output Shoal then refused (invalid_executor_output) — and it does not say
// Shoal decided the work failed: executor_error is Shoal's code for a
// failure the executor reported without one.
//
// The checks are positive, not a list of banned phrases: each service reason
// begins with one fixed form, the service transition is one exact sentence,
// no other service-origin sentence names Shoal, and every "Shoal" any failure
// sentence says, under any origin, is followed by "recorded" or is one of the
// few non-crediting forms shoalOnlyRecords lists.
func TestShoalIsCreditedOnlyWithTheCode(t *testing.T) {
	r := New(nil)
	at := "2026-10-08 12:03:00 UTC"
	codes := append(sourceErrorCodes(t), "made up by an executor")
	for _, code := range codes {
		shown := code
		if _, _, known := errorCodeKey(code); !known {
			shown = "“" + code + "”"
		}
		for _, s := range failureSentences(t, r, failedWith(code, fleet.ErrorCodeOriginService)) {
			switch {
			case s.Role == RoleReason:
				prefix := "Shoal recorded the failure as " + shown + ", "
				if !strings.HasPrefix(s.Text, prefix) {
					t.Errorf("%s: service reason does not begin %q: %s", code, prefix, s.Text)
				}
			case s.Key == "dispatch.transition.fail":
				want := "At " + at + ", Shoal recorded the failure as " + shown +
					" after worker-1 reported its outcome."
				if s.Text != want {
					t.Errorf("%s: service transition\n got %s\nwant %s", code, s.Text, want)
				}
			case strings.Contains(s.Text, "Shoal"):
				t.Errorf("%s: %s names Shoal: %s", code, s.Key, s.Text)
			}
		}
	}
	for _, origin := range errorCodeOriginCases(t) {
		for _, code := range codes {
			for _, s := range failureSentences(t, r, failedWith(code, origin)) {
				shoalOnlyRecords(t, code+" origin="+string(origin), wantOrigin[origin], s.Text)
			}
		}
	}
}

// shoalOnlyRecords requires every "Shoal" in text to be followed by
// "recorded", or to be one of the forms that credit Shoal with nothing: the
// disjunction an unknown origin uses ("… or Shoal assigned; the record does
// not say which") and the negations in the executor's unrecognized-code
// sentences ("Shoal does not define", "Shoal cannot interpret").
func shoalOnlyRecords(t *testing.T, what string, origin Selector, text string) {
	t.Helper()
	allowed := map[Selector][]string{
		OriginEither:   {"or Shoal assigned"},
		OriginExecutor: {"Shoal does not define", "Shoal cannot interpret"},
	}[origin]
	for i := 0; ; {
		j := strings.Index(text[i:], "Shoal")
		if j < 0 {
			return
		}
		i += j
		ok := strings.HasPrefix(text[i+len("Shoal"):], " recorded ")
		for _, form := range allowed {
			start := i - strings.Index(form, "Shoal")
			ok = ok || (start >= 0 && strings.HasPrefix(text[start:], form))
		}
		if !ok {
			word, _, _ := strings.Cut(strings.TrimSpace(text[i+len("Shoal"):]), " ")
			t.Errorf("%s: Shoal is paired with %q: %s", what, word, text)
		}
		i += len("Shoal")
	}
}

// Admissions are reported by the identity that requested them; histories and
// outcomes name it, never an unrecorded claimant.
func TestAdmissionReporterIsTheRequester(t *testing.T) {
	r := New(nil)
	granted := admission(fleet.DispatchClaimed)
	for _, c := range []struct {
		end     fleet.DispatchState
		origin  fleet.ErrorCodeOrigin
		outcome string
	}{
		{fleet.DispatchSucceeded, "", "reported as succeeded by alice"},
		{fleet.DispatchFailed, fleet.ErrorCodeOriginExecutor, "reported as failed by alice"},
		{fleet.DispatchFailed, fleet.ErrorCodeOriginService, "after alice reported its outcome"},
		{fleet.DispatchFailed, fleet.ErrorCodeOriginUnknown, "after alice reported its outcome"},
	} {
		end := c.end
		finished := admission(end)
		finished.ErrorCodeOrigin = c.origin
		finished.Version = granted.Version + 1
		kind := "action.completed"
		if end == fleet.DispatchFailed {
			kind = "action.failed"
		}
		history, err := r.ActionHistory([]fleet.ActionTransition{
			{ID: []byte("t1"), Kind: "action.claimed", Record: granted},
			{ID: []byte("t2"), Kind: kind, Record: finished},
		}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		last := history[len(history)-1]
		if !strings.Contains(last.Text, "alice reported") || !hasRef(last, "principal", "alice") ||
			strings.Contains(last.Text, "unrecorded") {
			t.Errorf("%s: admission report not attributed to the requester: %s %v", end, last.Text, last.Refs)
		}
		sentences, err := r.Action(finished, Options{QuoteOutput: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sentences {
			if strings.Contains(s.Text, "unrecorded principal") {
				t.Errorf("%s: %s", end, s.Text)
			}
			for _, q := range s.Quotes {
				if s.Key == "dispatch.detail.output" && q.By != "alice" {
					t.Errorf("%s: output attributed to %q", end, q.By)
				}
			}
		}
		if !strings.Contains(sentences[0].Text, c.outcome) {
			t.Errorf("%s: outcome %q", end, sentences[0].Text)
		}
	}
}

// A whole-request decision reason may be the service's or a predictor's own.
// Since #556 a predictor cannot return one of the service's reasons
// (decision.ReservedServiceReason), but a result written before #556 could,
// and a result records nothing that shows which build wrote it. So every
// whole-request reason, a service reason included, is attributed to both and
// its meaning is conditional; none is ever stated as the service's finding.
func TestDecisionReasonsAreReportsNotFindings(t *testing.T) {
	r := New(nil)
	req := request(t, taskConfig(), []shoal.ID{"subject1"}, nil)
	const predictorWords = "predictor words"
	if decision.ReservedServiceReason(predictorWords) {
		t.Fatal("the predictor fixture is a reserved service reason")
	}
	// Every reserved reason on every status, so one reserved for the other
	// status (evidence_ineligible on a failure) is covered too, and a reason
	// that is not reserved, which only a predictor can have set.
	var reasons []string
	for _, rs := range decision.ServiceReasons() {
		reasons = append(reasons, rs...)
	}
	reasons = append(reasons, predictorWords)
	for _, status := range []decision.ResultStatus{decision.Abstained, decision.Failed} {
		for _, reason := range reasons {
			sentences, err := r.Prediction(prediction(t, req, status, reason, nil), Options{})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, s := range sentences {
				text := strings.ToLower(s.Text)
				switch {
				case strings.HasPrefix(s.Key, "decision.reason."):
					found = true
					decisionReasonIsFaithful(t, status, reason, s)
				case strings.HasPrefix(s.Key, "decision.next.") && knownDecisionReason(status, reason):
					if !strings.HasPrefix(text, "if ") {
						t.Errorf("%s/%s: next step is not conditional: %s", status, reason, s.Text)
					}
				}
			}
			if !found {
				t.Errorf("%s/%s: no reason sentence", status, reason)
			}
		}
	}
}

// decisionReasonIsFaithful checks a whole-request reason sentence attributes
// the reason to the predictor or the service, never to the service alone, and
// states a reason's meaning only conditionally.
func decisionReasonIsFaithful(t *testing.T, status decision.ResultStatus, reason string, s Sentence) {
	t.Helper()
	text := strings.ToLower(s.Text)
	if !strings.Contains(text, "reported, by the predictor or the service") {
		t.Errorf("%s/%s: reason not attributed to both: %s", status, reason, s.Text)
	}
	// The service's finding stated as such, as an error code with a service
	// origin is. No decision result supports it: a reserved reason may be a
	// pre-#556 predictor's, and any other reason is a predictor's or a newer
	// build's.
	for _, finding := range []string{
		"shoal recorded", "shoal determined", "shoal found", "shoal assigned",
		"the service recorded", "the service found", "the service determined",
		"the service assigned", "the service established", "service’s own",
		"service's own", "no predictor was run", "so no",
	} {
		if strings.Contains(text, finding) {
			t.Errorf("%s/%s: stated as the service's finding (%q): %s",
				status, reason, finding, s.Text)
		}
	}
	if knownDecisionReason(status, reason) && !strings.Contains(text, "if accurate") {
		t.Errorf("%s/%s: meaning stated as fact: %s", status, reason, s.Text)
	}
	quoted := false
	for _, q := range s.Quotes {
		quoted = true
		if q.Attribution != AttributedToPredictorOrService {
			t.Errorf("%s/%s: quote attributed to %s", status, reason, q.Attribution)
		}
	}
	if !quoted && !strings.Contains(text, reason) {
		t.Errorf("%s/%s: the reason is neither quoted nor named: %s", status, reason, s.Text)
	}
}

func TestSuccessIsAReport(t *testing.T) {
	r := New(nil)
	for _, record := range []fleet.ActionRecord{action(fleet.DispatchSucceeded), admission(fleet.DispatchSucceeded)} {
		sentences, err := r.Action(record, Options{})
		if err != nil {
			t.Fatal(err)
		}
		outcome := sentences[0]
		want := "worker-1"
		if isAdmission(record) {
			want = "alice"
		}
		if !strings.Contains(outcome.Text, "was reported as succeeded by "+want) ||
			!hasRef(outcome, "principal", want) {
			t.Errorf("success not attributed to %s: %s %v", want, outcome.Text, outcome.Refs)
		}
	}
}

func hasRef(s Sentence, kind, id string) bool {
	for _, ref := range s.Refs {
		if ref.Kind == kind && ref.ID == id {
			return true
		}
	}
	return false
}

func refByKey(t *testing.T, sentences []Sentence, key, kind, id string) {
	t.Helper()
	for _, s := range sentences {
		if s.Key == key {
			if !hasRef(s, kind, id) {
				t.Errorf("%s does not ref %s=%s: %v", key, kind, id, s.Refs)
			}
			return
		}
	}
	t.Errorf("no %s sentence", key)
}

// Sentences that name a principal or a claim carry it as a reference.
func TestPrincipalsAreGrounded(t *testing.T) {
	r := New(nil)
	claimed, err := r.Action(action(fleet.DispatchClaimed), Options{Now: t0.Add(2 * 60e9), QuoteInput: true})
	if err != nil {
		t.Fatal(err)
	}
	refByKey(t, claimed, "dispatch.history.requested", "principal", "alice")
	refByKey(t, claimed, "dispatch.history.requested", "request", "req-1")
	refByKey(t, claimed, "dispatch.history.claimant", "principal", "worker-1")
	refByKey(t, claimed, "dispatch.history.claimant", "claim", "claim-1")
	refByKey(t, claimed, "dispatch.blocked.lease_live", "claim", "claim-1")
	refByKey(t, claimed, "dispatch.detail.input", "principal", "alice")

	approved := action(fleet.DispatchSucceeded)
	approved.ApprovalRequestDigest = make([]byte, 32)
	approved.ApproverActor, approved.ApproverSubject = "bob", "bob"
	sentences, err := r.Action(approved, Options{})
	if err != nil {
		t.Fatal(err)
	}
	refByKey(t, sentences, "dispatch.history.approved", "principal", "bob")

	history, err := r.ActionHistory([]fleet.ActionTransition{
		{ID: []byte("t1"), Kind: "action.enqueued", Record: action(fleet.DispatchQueued)},
		{ID: []byte("t2"), Kind: "action.claimed", Record: action(fleet.DispatchClaimed)},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	refByKey(t, history, "dispatch.transition.enqueue", "principal", "alice")
	refByKey(t, history, "dispatch.transition.claim", "principal", "worker-1")
	refByKey(t, history, "dispatch.transition.claim", "claim", "claim-1")

	for _, verdict := range []fleet.ApprovalVerdict{fleet.ApprovalVerdictApprove, fleet.ApprovalVerdictRefuse} {
		state := fleet.ApprovalApproved
		if verdict == fleet.ApprovalVerdictRefuse {
			state = fleet.ApprovalRefused
		}
		sentences, err := r.Approval(fleet.ApprovalStatus{
			Approval: approvalRecord(state, verdict), State: state,
		}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		refByKey(t, sentences, "approval.transition.requested", "principal", "alice")
		refByKey(t, sentences, "approval.transition."+string(state), "principal", "bob")
		refByKey(t, sentences, "approval.transition."+string(state), "request", "req-2")
	}

	req := request(t, taskConfig(), []shoal.ID{"subject1"}, nil)
	sentences, err = r.Prediction(prediction(t, req, decision.Failed, "deadline_exceeded", nil), Options{})
	if err != nil {
		t.Fatal(err)
	}
	refByKey(t, sentences, "decision.history.requested", "principal", "carol")
}
