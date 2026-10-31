// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// InboxRoute lists what is awaiting a person on this host: held fleet
// requests the caller may decide, and learning candidates sealed in shadow
// that no release has taken, each with the measured record of the classes
// it was trained on. The inbox reads through the providers that already
// serve those objects and adds no authority of its own: a caller who is not
// an eligible approver sees no approvals, and the records beside a candidate
// are the ones the class route would serve the same caller.
const InboxRoute = "/api/v1/inbox"

// Bounds on what one inbox read composes. A page of pending approvals is
// MaxApprovalListResults; the inbox walks pages up to MaxInboxApprovals and
// says when it stopped.
const (
	MaxInboxApprovals = 256
	// MaxInboxApprovalPages bounds the store rows one read scans: the
	// pending route filters each page to what the caller may decide, so a
	// page can be empty with more behind it, and the walk continues until
	// the cursor ends or this many pages have been read.
	MaxInboxApprovalPages = 16
	MaxInboxCandidates    = 32
	MaxInboxCohorts       = 8
	// DefaultInboxRecordWindow is how far before now the measured record
	// beside a candidate reaches, when the host declares nothing else.
	DefaultInboxRecordWindow = 365 * 24 * time.Hour
)

// InboxSurface is what the inbox composes. Approvals, Learning, and Classes
// are each optional; a section whose provider is absent says so. The
// resolver must be the one bound to the host's authenticated transport.
type InboxSurface struct {
	Resolver  auth.Resolver
	Approvals FleetApprovalProvider
	Learning  decisionapi.LearningProvider
	Classes   decisionapi.ClassRecordProvider
	// RecordWindow is how far before now the declared window of the
	// measured record beside a candidate begins; the window ends at the
	// cohort's own cutoff, and the record states the window it was served
	// for. Zero means the default.
	RecordWindow time.Duration
	Now          func() time.Time
}

func (s InboxSurface) validate() error {
	if isAbsentInterface(s.Resolver) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "inbox requires the trusted resolver")
	}
	if isAbsentInterface(s.Approvals) && isAbsentInterface(s.Learning) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "inbox requires an approval or a learning provider")
	}
	if s.RecordWindow < 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "inbox record window must not be negative")
	}
	return nil
}

// NewInboxHandler returns the inbox route without adding another
// authentication boundary; mount it through Handler.MountAuthenticated at
// InboxRoute, or use MountInbox.
func NewInboxHandler(surface InboxSurface) (http.Handler, error) {
	if err := surface.validate(); err != nil {
		return nil, err
	}
	if surface.RecordWindow == 0 {
		surface.RecordWindow = DefaultInboxRecordWindow
	}
	if surface.Now == nil {
		surface.Now = time.Now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+InboxRoute, (&inboxHTTP{surface}).serve)
	return mux, nil
}

// MountInbox adds the inbox route behind the host's authenticated transport
// and advertises the capability.
func (h *Handler) MountInbox(surface InboxSurface) error {
	if h == nil || h.mux == nil || isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "inbox requires authenticated transport")
	}
	handler, err := NewInboxHandler(surface)
	if err != nil {
		return err
	}
	if err := h.MountAuthenticated(InboxRoute, handler); err != nil {
		return err
	}
	h.enableDecisionCapability(CapabilityInbox)
	return nil
}

type inboxHTTP struct{ surface InboxSurface }

type inboxRequestWire struct {
	Context *fleetRequestContextWire `json:"context,omitempty"`
}

// inboxItemWire is one thing awaiting a person. Waiting says what it waits
// for; this slice has only decisions. Exactly one of Approval and Candidate
// is set, by Kind.
type inboxItemWire struct {
	Kind      string              `json:"kind"`
	ID        string              `json:"id"`
	Waiting   string              `json:"waiting"`
	Approval  *fleetApprovalWire  `json:"approval,omitempty"`
	Candidate *inboxCandidateWire `json:"candidate,omitempty"`
	Classes   []inboxClassWire    `json:"classes,omitempty"`
	// ClassesError says why no class record could be attempted: the
	// candidate's training job could not be read. It is never set beside
	// Classes.
	ClassesError string `json:"classes_error,omitempty"`
}

type inboxGroupWire struct {
	Capability string          `json:"capability"`
	Action     string          `json:"action"`
	Items      []inboxItemWire `json:"items"`
}

type inboxApprovalsWire struct {
	Mounted   bool             `json:"mounted"`
	Available bool             `json:"available"`
	Note      string           `json:"note,omitempty"`
	Groups    []inboxGroupWire `json:"groups"`
	Truncated bool             `json:"truncated"`
}

type inboxCandidateWire struct {
	Disposition            string          `json:"disposition"`
	TaskIDs                json.RawMessage `json:"task_ids,omitempty"`
	DatasetID              string          `json:"dataset_id,omitempty"`
	TrainingManifestPin    string          `json:"training_manifest_sha256,omitempty"`
	Evaluations            json.RawMessage `json:"evaluations"`
	Lineage                json.RawMessage `json:"lineage,omitempty"`
	PromotableEvaluationID string          `json:"promotable_evaluation_id,omitempty"`
}

// inboxClassWire is the measured record of one cohort a candidate consumed,
// or why it could not be served to this caller. Records is the class route's
// response verbatim.
type inboxClassWire struct {
	CohortID string          `json:"cohort_id"`
	From     time.Time       `json:"from"`
	Records  json.RawMessage `json:"records,omitempty"`
	Error    string          `json:"error,omitempty"`
}

type inboxCandidatesWire struct {
	Mounted   bool            `json:"mounted"`
	Available bool            `json:"available"`
	Note      string          `json:"note,omitempty"`
	Items     []inboxItemWire `json:"items"`
	Truncated bool            `json:"truncated"`
}

type inboxWire struct {
	GeneratedAt time.Time           `json:"generated_at"`
	Approvals   inboxApprovalsWire  `json:"approvals"`
	Candidates  inboxCandidatesWire `json:"candidates"`
}

func (h *inboxHTTP) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var wire inboxRequestWire
	if err := decodeRequest(w, r, &wire); err != nil {
		writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
		return
	}
	ctx := r.Context()
	decision, err := h.surface.Resolver.Resolve(ctx)
	if err != nil || !h.surface.Now().Before(decision.AuthenticationExpires()) {
		writeError(w, authenticationDenied())
		return
	}
	before, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		writeError(w, authenticationDenied())
		return
	}
	out := inboxWire{GeneratedAt: h.surface.Now().UTC(), Approvals: inboxApprovalsWire{Groups: []inboxGroupWire{}}, Candidates: inboxCandidatesWire{Items: []inboxItemWire{}}}
	if !isAbsentInterface(h.surface.Approvals) {
		out.Approvals.Mounted = true
		if wire.Context == nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, "inbox requires a request context to list approvals"))
			return
		}
		requestContext, err := wire.Context.decode()
		if err != nil {
			writeError(w, err)
			return
		}
		if err := h.approvals(ctx, requestContext, &out.Approvals); err != nil {
			writeError(w, err)
			return
		}
	} else {
		out.Approvals.Note = "no approval service is mounted on this host"
	}
	if !isAbsentInterface(h.surface.Learning) {
		out.Candidates.Mounted = true
		if err := h.candidates(ctx, &out.Candidates); err != nil {
			writeError(w, err)
			return
		}
	} else {
		out.Candidates.Note = "no learning service is mounted on this host"
	}
	// Nothing composed above is disclosed under an authority that moved
	// while it was being read.
	after, err := h.surface.Resolver.Resolve(ctx)
	if err != nil {
		writeError(w, authenticationDenied())
		return
	}
	if fp, err := auth.AuthorizationFingerprint(after); err != nil || fp != before {
		writeError(w, authenticationDenied())
		return
	}
	writeResponse(w, http.StatusOK, out)
}

// approvals walks every page of pending approvals the caller may decide and
// groups them by the action class they belong to: the capability and action
// the request names. A caller who is not an eligible approver gets an empty
// section that says so, not an error; any other failure is the failure.
func (h *inboxHTTP) approvals(ctx context.Context, requestContext fleet.RequestContext, out *inboxApprovalsWire) error {
	var after []byte
	var records []fleet.ApprovalRecord
	for pages := 0; ; pages++ {
		if pages == MaxInboxApprovalPages {
			out.Truncated = true
			break
		}
		page, err := h.surface.Approvals.Pending(ctx, fleet.PendingApprovalsRequest{After: after, Limit: fleet.MaxApprovalListResults, Context: requestContext})
		if err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
				out.Available, out.Note = false, "the caller may not decide held requests on this host"
				return nil
			}
			// The approval service's own errors carry deliberate codes and
			// messages; anything else is a store failure whose text is
			// not the caller's to read.
			mapped := fleetApprovalError(err)
			var coded *shoal.Error
			if !errors.As(mapped, &coded) {
				return shoal.NewError(shoal.ErrorUnavailable, "held requests unavailable")
			}
			return mapped
		}
		records = append(records, page.Approvals...)
		if len(records) >= MaxInboxApprovals {
			out.Truncated = len(records) > MaxInboxApprovals || len(page.Next) > 0
			records = records[:MaxInboxApprovals]
			break
		}
		// An empty page is not the end: the service filtered it. Only an
		// exhausted cursor is.
		if len(page.Next) == 0 {
			break
		}
		after = page.Next
	}
	out.Available = true
	groups := map[[2]string]*inboxGroupWire{}
	for _, record := range records {
		key := [2]string{record.Request.Capability, record.Request.Action}
		group := groups[key]
		if group == nil {
			group = &inboxGroupWire{Capability: key[0], Action: key[1], Items: []inboxItemWire{}}
			groups[key] = group
		}
		encoded := encodeApproval(record, false)
		group.Items = append(group.Items, inboxItemWire{Kind: "approval", ID: encoded.ID, Waiting: "decision", Approval: &encoded})
	}
	for _, group := range groups {
		sort.SliceStable(group.Items, func(i, j int) bool {
			a, b := group.Items[i].Approval, group.Items[j].Approval
			if !a.RequestedAt.Equal(b.RequestedAt) {
				return a.RequestedAt.After(b.RequestedAt)
			}
			return a.ID < b.ID
		})
		out.Groups = append(out.Groups, *group)
	}
	sort.Slice(out.Groups, func(i, j int) bool {
		if out.Groups[i].Capability != out.Groups[j].Capability {
			return out.Groups[i].Capability < out.Groups[j].Capability
		}
		return out.Groups[i].Action < out.Groups[j].Action
	})
	return nil
}

func inboxString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func inboxRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// candidates lists sealed shadow candidates no release has taken, newest
// evaluation first, with the measured record of each cohort the candidate
// was trained on. A learning provider that cannot be read is the failure;
// a class record that cannot be served to this caller is said per cohort.
func (h *inboxHTTP) candidates(ctx context.Context, out *inboxCandidatesWire) error {
	raw, err := h.surface.Learning.Candidates(ctx)
	if err != nil {
		return learningInboxError(err)
	}
	var listing struct {
		Candidates []struct {
			Candidate   map[string]any   `json:"candidate"`
			Evaluations []map[string]any `json:"evaluations"`
			Lineage     map[string]any   `json:"lineage"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return shoal.NewError(shoal.ErrorUnavailable, "learning candidates are not readable")
	}
	released := map[string]bool{}
	if raw, err := h.surface.Learning.Releases(ctx); err == nil {
		var releases struct {
			Active *struct {
				Release map[string]any `json:"release"`
			} `json:"active"`
			History []struct {
				Release map[string]any `json:"release"`
			} `json:"history"`
		}
		if err := json.Unmarshal(raw, &releases); err != nil {
			// Releases that cannot be read would let a taken candidate
			// reappear as awaiting promotion; that is the failure.
			return shoal.NewError(shoal.ErrorUnavailable, "learning releases are not readable")
		}
		if releases.Active != nil {
			released[inboxString(releases.Active.Release, "candidate_id")] = true
		}
		for _, r := range releases.History {
			released[inboxString(r.Release, "candidate_id")] = true
		}
	} else {
		return learningInboxError(err)
	}
	out.Available = true
	for _, c := range listing.Candidates {
		id := inboxString(c.Candidate, "id")
		if id == "" || released[id] {
			continue
		}
		var promotable, disposition string
		for _, e := range c.Evaluations {
			if inboxString(e, "disposition") == "shadow_candidate" {
				promotable, disposition = inboxString(e, "id"), "shadow_candidate"
			} else if disposition == "" {
				disposition = inboxString(e, "disposition")
			}
		}
		if promotable == "" {
			// Nothing a person can decide: the candidate was held or has
			// no sealed evaluation yet.
			continue
		}
		if len(out.Items) >= MaxInboxCandidates {
			out.Truncated = true
			break
		}
		item := inboxItemWire{Kind: "candidate", ID: id, Waiting: "decision", Candidate: &inboxCandidateWire{Disposition: disposition, TaskIDs: inboxRaw(c.Candidate["task_ids"]), DatasetID: inboxString(c.Candidate, "dataset_id"), TrainingManifestPin: inboxString(c.Candidate, "training_manifest_sha256"), Evaluations: inboxRaw(c.Evaluations), Lineage: inboxRaw(c.Lineage), PromotableEvaluationID: promotable}}
		if item.Candidate.Evaluations == nil {
			item.Candidate.Evaluations = json.RawMessage("[]")
		}
		item.Classes, item.ClassesError = h.classes(ctx, item.Candidate.TrainingManifestPin)
		out.Items = append(out.Items, item)
	}
	return nil
}

// classes resolves the cohorts a candidate's job consumed and serves the
// measured record of each over the declared window: from the record window
// before now to the cohort's own cutoff. Without a class provider nothing is
// listed; a job that cannot be read is said; a cohort whose record this
// caller cannot read is listed with the reason, never with a thinned record;
// a record the class route would refuse for a substituted cohort or window
// is refused here too.
func (h *inboxHTTP) classes(ctx context.Context, pin string) ([]inboxClassWire, string) {
	if isAbsentInterface(h.surface.Classes) || pin == "" {
		return nil, ""
	}
	raw, err := h.surface.Learning.Job(ctx, pin)
	if err != nil {
		return nil, "training job unavailable"
	}
	var job struct {
		Job struct {
			Exports []struct {
				CohortID string `json:"cohort_id"`
			} `json:"exports"`
		} `json:"job"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil, "training job unavailable"
	}
	from := h.surface.Now().UTC().Add(-h.surface.RecordWindow)
	var out []inboxClassWire
	seen := map[string]bool{}
	for _, export := range job.Job.Exports {
		if export.CohortID == "" || seen[export.CohortID] {
			continue
		}
		seen[export.CohortID] = true
		if len(out) >= MaxInboxCohorts {
			break
		}
		entry := inboxClassWire{CohortID: export.CohortID, From: from}
		records, err := h.surface.Classes.ClassRecords(ctx, decisionapi.ClassRecordRequest{CohortID: shoal.ID(export.CohortID), From: from})
		if err == nil {
			var encoded []byte
			substituted := records.CohortID != shoal.ID(export.CohortID) || !records.From.Equal(from) || !records.To.Equal(records.Cutoff)
			if encoded, err = decisionapi.EncodeClassRecords(records); err == nil && !substituted {
				entry.Records = encoded
			} else {
				entry.Error = "class record unavailable"
			}
		} else {
			switch {
			case shoal.IsErrorCode(err, shoal.ErrorNotFound):
				entry.Error = "class record not found"
			case shoal.IsErrorCode(err, shoal.ErrorInvalidArgument):
				entry.Error = "the declared window does not reach this cohort"
			default:
				entry.Error = "class record unavailable"
			}
		}
		out = append(out, entry)
	}
	return out, ""
}

func learningInboxError(err error) error {
	switch {
	case shoal.IsErrorCode(err, shoal.ErrorUnauthorized):
		return shoal.NewError(shoal.ErrorUnauthorized, "authentication required")
	case shoal.IsErrorCode(err, shoal.ErrorNotFound):
		return shoal.NewError(shoal.ErrorNotFound, "learning records not found")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return shoal.NewError(shoal.ErrorUnavailable, "learning records unavailable")
	}
	return shoal.NewError(shoal.ErrorUnavailable, "learning records unavailable")
}
