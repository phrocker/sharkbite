// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"time"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Collector administration routes (#446 slice 5). They provision, revoke and
// read collector registrations and require the collector_admin operation,
// which no shipped authenticator mints; a host that mounts them mints its own
// admin decisions. See docs/collectors.md.
const (
	AdminRoute              = Route + "/admin"
	AdminProvisionRoute     = AdminRoute + "/provision"
	AdminRevokeRoute        = AdminRoute + "/revoke"
	AdminRegistrationsRoute = AdminRoute + "/registrations"

	// MaxAdminReceiptBytes bounds every admin write receipt, with the
	// collector ID at shoal.MaxIDBytes.
	MaxAdminReceiptBytes = 2048
	// MaxRegistrationChanges bounds the change summaries in one
	// RegistrationRecord: two per generation over ViewGenerations.
	MaxRegistrationChanges = 16
	// ViewGenerations is how many recent generations a RegistrationRecord
	// covers.
	ViewGenerations = 8
)

// Change log actions and sources, as RegistrationRecord reports them.
const (
	ChangeProvision      = "provision"
	ChangeRevoke         = "revoke"
	SourceOperatorAPI    = "operator_api"
	SourceCollectorAdmin = "collector_admin"
)

// Provisioning is an admin's grant to one principal, in contract form. It
// mirrors the server's Go provisioning API, plus the generation the
// registration must hold afterwards: 1 to create, the current generation to
// reactivate a revoked registration or to confirm an identical active one.
type Provisioning struct {
	CollectorID        shoal.ID
	Subject            shoal.ID
	ClientID           shoal.ID
	Domain             []byte
	AuthorityPolicyIDs []shoal.ID
	Control            collector.Control
	Mode               collector.Mode
	Generation         int64
}

// Validate checks p without contacting the server.
func (p Provisioning) Validate() error {
	if _, e := p.registration().Canonical(); e != nil || p.Generation <= 0 {
		return invalidWire()
	}
	return nil
}

func (p Provisioning) registration() collector.Registration {
	return collector.Registration{CollectorID: p.CollectorID, Subject: p.Subject, ClientID: p.ClientID, Domain: p.Domain, AuthorityPolicyIDs: p.AuthorityPolicyIDs, Control: p.Control, Mode: p.Mode, Generation: 1, State: collector.Provisioned}
}

// AdminProvider is the server-side seam of the admin routes. Implementations
// resolve the admin from the trusted request context and own every
// authorization decision; nothing in these arguments carries authority.
type AdminProvider interface {
	ProvisionCollector(context.Context, Provisioning) (AdminReceipt, error)
	RevokeCollector(context.Context, shoal.ID, int64) (AdminReceipt, error)
	ReadRegistration(context.Context, shoal.ID) (RegistrationRecord, error)
}

type ProvisionRequest struct {
	CollectorID        string   `json:"collector_id"`
	Subject            string   `json:"subject"`
	ClientID           string   `json:"client_id,omitempty"`
	Domain             string   `json:"domain"`
	AuthorityPolicyIDs []string `json:"authority_policy_ids"`
	Control            string   `json:"control"`
	Mode               string   `json:"mode"`
	Generation         int64    `json:"generation"`
}

type RevokeRequest struct {
	CollectorID string `json:"collector_id"`
	Generation  int64  `json:"generation"`
}

// AdminReceipt is the receipt of an admin write. Like the collector write
// receipts it carries identifiers and counters only; the registration is read
// back through the registrations route. Changed reports whether this call
// made the change: a retry of a change that already applied reports false.
type AdminReceipt struct {
	Schema      int    `json:"schema"`
	CollectorID string `json:"collector_id"`
	Generation  int64  `json:"generation"`
	State       string `json:"state"`
	Changed     bool   `json:"changed"`
}

// AdminAttribution identifies the admin decision behind a change.
type AdminAttribution struct {
	Subject                  string `json:"subject"`
	Actor                    string `json:"actor"`
	ClientID                 string `json:"client_id,omitempty"`
	RequestID                string `json:"request_id"`
	CorrelationID            string `json:"correlation_id,omitempty"`
	AuthorizationFingerprint string `json:"authorization_fingerprint"`
}

// Anomaly reasons, as RegistrationRecord reports them (docs/collectors.md,
// "Audit").
const (
	AnomalyUndecodable    = "undecodable"
	AnomalyUnknownVersion = "unknown_version"
	AnomalyMisplaced      = "misplaced"
	AnomalyUnexpected     = "unexpected"
	AnomalyDuplicate      = "duplicate"
	AnomalyInconsistent   = "inconsistent"
)

// ChangeSummary is one change log entry. Its slot is (Generation, Action).
// By is present exactly when Source is SourceCollectorAdmin; the trusted Go
// API has no caller identity.
type ChangeSummary struct {
	Action     string            `json:"action"`
	Source     string            `json:"source"`
	Generation int64             `json:"generation"`
	State      string            `json:"state"`
	By         *AdminAttribution `json:"by,omitempty"`
	At         time.Time         `json:"at"`
}

// LogGap is a slot the registration implies that holds no entry.
type LogGap struct {
	Generation int64  `json:"generation"`
	Action     string `json:"action"`
}

// LogAnomaly is a stored cell that is not exactly one valid entry for its
// slot. Generation and Action are absent when its qualifier names no slot.
type LogAnomaly struct {
	Generation int64  `json:"generation,omitempty"`
	Action     string `json:"action,omitempty"`
	Reason     string `json:"reason"`
}

// RegistrationRecord is a registration as an admin reads it, with the change
// log of its generations from FromGeneration on: the entries, newest first,
// and the gaps and anomalies found there. It carries no quarantine status:
// observation status is computed from the current registration at each
// observation read.
type RegistrationRecord struct {
	Schema             int             `json:"schema"`
	CollectorID        string          `json:"collector_id"`
	Subject            string          `json:"subject"`
	ClientID           string          `json:"client_id,omitempty"`
	Domain             string          `json:"domain"`
	AuthorityPolicyIDs []string        `json:"authority_policy_ids"`
	Control            string          `json:"control"`
	Mode               string          `json:"mode"`
	Generation         int64           `json:"generation"`
	State              string          `json:"state"`
	EnrollmentID       string          `json:"enrollment_id,omitempty"`
	FromGeneration     int64           `json:"from_generation"`
	Changes            []ChangeSummary `json:"changes"`
	Gaps               []LogGap        `json:"gaps"`
	Anomalies          []LogAnomaly    `json:"anomalies"`
	// MoreAnomalies counts anomalies beyond MaxRegistrationAnomalies that
	// are not listed. Inconsistent and duplicate anomalies are listed first.
	MoreAnomalies int `json:"more_anomalies,omitempty"`
}

// MaxRegistrationAnomalies bounds the anomalies one RegistrationRecord lists.
const MaxRegistrationAnomalies = 64

// EncodeProvision converts a contract provisioning to its wire form.
func EncodeProvision(p Provisioning) ProvisionRequest {
	out := ProvisionRequest{CollectorID: EncodeID(p.CollectorID), Subject: EncodeID(p.Subject), Domain: EncodeKey(p.Domain), AuthorityPolicyIDs: []string{}, Control: string(p.Control), Mode: string(p.Mode), Generation: p.Generation}
	if p.ClientID != "" {
		out.ClientID = EncodeID(p.ClientID)
	}
	for _, id := range p.AuthorityPolicyIDs {
		out.AuthorityPolicyIDs = append(out.AuthorityPolicyIDs, EncodeID(id))
	}
	return out
}

// DecodeProvision converts and validates a wire provisioning. Authority IDs
// are returned in canonical order.
func DecodeProvision(w ProvisionRequest) (Provisioning, error) {
	var p Provisioning
	var e error
	if p.CollectorID, e = DecodeID(w.CollectorID); e != nil {
		return Provisioning{}, invalidWire()
	}
	if p.Subject, e = DecodeID(w.Subject); e != nil {
		return Provisioning{}, invalidWire()
	}
	if w.ClientID != "" {
		if p.ClientID, e = DecodeID(w.ClientID); e != nil {
			return Provisioning{}, invalidWire()
		}
	}
	if p.Domain, e = DecodeKey(w.Domain); e != nil {
		return Provisioning{}, invalidWire()
	}
	if len(w.AuthorityPolicyIDs) > collector.MaxAuthorityPolicyIDs {
		return Provisioning{}, invalidWire()
	}
	for _, encoded := range w.AuthorityPolicyIDs {
		id, e := DecodeID(encoded)
		if e != nil {
			return Provisioning{}, invalidWire()
		}
		p.AuthorityPolicyIDs = append(p.AuthorityPolicyIDs, id)
	}
	p.Control, p.Mode, p.Generation = collector.Control(w.Control), collector.Mode(w.Mode), w.Generation
	canonical, e := p.registration().Canonical()
	if e != nil || p.Generation <= 0 {
		return Provisioning{}, invalidWire()
	}
	p.AuthorityPolicyIDs = canonical.AuthorityPolicyIDs
	return p, nil
}

// DecodeRevoke converts and validates a wire revocation.
func DecodeRevoke(w RevokeRequest) (shoal.ID, int64, error) {
	id, e := DecodeID(w.CollectorID)
	if e != nil || w.Generation <= 0 {
		return "", 0, invalidWire()
	}
	return id, w.Generation, nil
}

func validState(s string) bool {
	switch collector.State(s) {
	case collector.Provisioned, collector.Enrolled, collector.Revoked:
		return true
	}
	return false
}

func (r AdminReceipt) Validate() error {
	if r.Schema != Schema || !validEncodedID(r.CollectorID) || r.Generation <= 0 || !validState(r.State) {
		return badResponse()
	}
	return nil
}

func (a AdminAttribution) validate() error {
	if !validEncodedID(a.Subject) || !validEncodedID(a.Actor) || !validEncodedID(a.RequestID) || a.AuthorizationFingerprint == "" || len(a.AuthorizationFingerprint) > 128 {
		return badResponse()
	}
	for _, optional := range []string{a.ClientID, a.CorrelationID} {
		if optional != "" && !validEncodedID(optional) {
			return badResponse()
		}
	}
	return nil
}

func (c ChangeSummary) validate() error {
	if c.Generation <= 0 || !validState(c.State) || !validTime(c.At) {
		return badResponse()
	}
	if (c.Action != ChangeProvision && c.Action != ChangeRevoke) || (c.Action == ChangeRevoke) != (c.State == string(collector.Revoked)) {
		return badResponse()
	}
	switch c.Source {
	case SourceOperatorAPI:
		if c.By != nil {
			return badResponse()
		}
	case SourceCollectorAdmin:
		if c.By == nil || c.By.validate() != nil {
			return badResponse()
		}
	default:
		return badResponse()
	}
	return nil
}

// slotOrder orders slots as they happen: by generation, a revoke into g
// before a provisioning at g.
func slotOrder(generation int64, action string) [2]int64 {
	o := int64(1)
	if action == ChangeRevoke {
		o = 0
	}
	return [2]int64{generation, o}
}

func before(a, b [2]int64) bool { return a[0] < b[0] || (a[0] == b[0] && a[1] < b[1]) }

// Validate checks wire integrity: the registration decodes canonically, the
// change summaries are well formed, in slots the registration implies within
// [FromGeneration, Generation], newest first, and gaps and anomalies are
// well formed.
func (r RegistrationRecord) Validate() error {
	if r.Schema != Schema || !validState(r.State) || len(r.Changes) > MaxRegistrationChanges || len(r.Anomalies) > MaxRegistrationAnomalies || r.FromGeneration <= 0 || r.FromGeneration > r.Generation || r.Generation-r.FromGeneration >= ViewGenerations {
		return badResponse()
	}
	if r.Changes == nil || r.Gaps == nil || r.Anomalies == nil || r.MoreAnomalies < 0 || (r.MoreAnomalies > 0 && len(r.Anomalies) != MaxRegistrationAnomalies) {
		return badResponse()
	}
	p, e := DecodeProvision(ProvisionRequest{CollectorID: r.CollectorID, Subject: r.Subject, ClientID: r.ClientID, Domain: r.Domain, AuthorityPolicyIDs: r.AuthorityPolicyIDs, Control: r.Control, Mode: r.Mode, Generation: r.Generation})
	if e != nil {
		return badResponse()
	}
	for i, id := range p.AuthorityPolicyIDs {
		if EncodeID(id) != r.AuthorityPolicyIDs[i] {
			return badResponse()
		}
	}
	if (r.EnrollmentID != "") != (r.State == string(collector.Enrolled)) || (r.EnrollmentID != "" && !validEncodedID(r.EnrollmentID)) {
		return badResponse()
	}
	inRange := func(generation int64, action string) bool {
		if generation < r.FromGeneration || generation > r.Generation || (action != ChangeProvision && action != ChangeRevoke) {
			return false
		}
		if action == ChangeRevoke {
			return generation >= 2
		}
		return generation < r.Generation || r.State != string(collector.Revoked)
	}
	for i, c := range r.Changes {
		if c.validate() != nil || !inRange(c.Generation, c.Action) || (i > 0 && !before(slotOrder(c.Generation, c.Action), slotOrder(r.Changes[i-1].Generation, r.Changes[i-1].Action))) {
			return badResponse()
		}
	}
	for _, g := range r.Gaps {
		if !inRange(g.Generation, g.Action) {
			return badResponse()
		}
		for _, c := range r.Changes {
			if c.Generation == g.Generation && c.Action == g.Action {
				return badResponse()
			}
		}
	}
	for _, a := range r.Anomalies {
		switch a.Reason {
		case AnomalyUndecodable, AnomalyUnknownVersion, AnomalyMisplaced, AnomalyUnexpected, AnomalyDuplicate, AnomalyInconsistent:
		default:
			return badResponse()
		}
		if a.Generation < 0 || (a.Action != "" && a.Action != ChangeProvision && a.Action != ChangeRevoke) {
			return badResponse()
		}
	}
	return nil
}
