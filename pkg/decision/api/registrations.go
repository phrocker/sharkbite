// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const MaxRegistrationSources = 64
const MaxRegistrationSourceBytes = 8 << 20
const MaxRegistrationRequestBytes = 12 << 20
const MaxRegistrationResponseBytes = 1 << 20

type RegistrationSourceInput struct {
	ObservationID shoal.ID
	Bytes         []byte
}
type RegistrationSelection struct {
	ProfileID, ProfileRevisionID shoal.ID
	Sources                      []RegistrationSourceInput
}

// RegistrationScope preserves original authenticated bytes, not current grants.
type RegistrationScope struct {
	Domain                       []byte
	SubjectID, ActorID, ClientID shoal.ID
	OnBehalfOf                   []shoal.ID
}
type RegistrationSourcePin struct {
	Mode              string    `json:"mode"`
	CollectorID       shoal.ID  `json:"collector_id"`
	ObservationID     shoal.ID  `json:"observation_id"`
	ArtifactID        shoal.ID  `json:"artifact_id"`
	EnrollmentID      shoal.ID  `json:"enrollment_id"`
	AuthorityPolicyID shoal.ID  `json:"authority_policy_id"`
	Generation        int64     `json:"generation"`
	ArtifactSHA256    string    `json:"artifact_sha256"`
	SourceSHA256      string    `json:"source_sha256"`
	ReceivedAt        time.Time `json:"received_at"`
}

// RegistrationSnapshot exposes immutable provenance commitments. The omitted
// canonical artifact bytes cannot be reconstructed or authenticated from this
// projection; hashes are not grants, signatures, or a training approval.
type RegistrationSnapshot struct {
	SelectionSHA256          string                  `json:"selection_sha256"`
	ProfileID                shoal.ID                `json:"profile_id"`
	ProfileRevisionID        shoal.ID                `json:"profile_revision_id"`
	BuilderID                shoal.ID                `json:"builder_id"`
	RequestID                shoal.ID                `json:"request_id"`
	TaskID                   shoal.ID                `json:"task_id"`
	PictureID                shoal.ID                `json:"picture_id"`
	PredictorID              shoal.ID                `json:"predictor_id"`
	Sources                  []RegistrationSourcePin `json:"sources"`
	AcceptedAt               time.Time               `json:"accepted_at"`
	AuthenticationExpiresAt  time.Time               `json:"authentication_expires_at"`
	AuthorizationFingerprint string                  `json:"authorization_fingerprint"`
	RecordSHA256             string                  `json:"record_sha256"`
	RecordBytes              int                     `json:"record_bytes"`
}
type RegistrationReceipt struct {
	ID                            shoal.ID
	Scope                         RegistrationScope
	State                         string
	Version                       int64
	Snapshot                      RegistrationSnapshot
	FrozenSHA256                  string
	CreatedAt, UpdatedAt, ReadyAt time.Time
}
type RegistrationProvider interface {
	Register(context.Context, RegistrationSelection, []byte) (RegistrationReceipt, error)
	Read(context.Context, shoal.ID) (RegistrationReceipt, error)
}
type registrationSourceWire struct {
	ObservationID string `json:"observation_id"`
	Bytes         string `json:"bytes"`
}
type registrationSelectionWire struct {
	ProfileID         string                   `json:"profile_id"`
	ProfileRevisionID string                   `json:"profile_revision_id"`
	Sources           []registrationSourceWire `json:"sources"`
}
type registrationRequestWire struct {
	Schema    int                       `json:"schema"`
	Selection registrationSelectionWire `json:"selection"`
}
type registrationScopeWire struct {
	Domain     string   `json:"domain"`
	SubjectID  string   `json:"subject_id"`
	ActorID    string   `json:"actor_id"`
	ClientID   string   `json:"client_id"`
	OnBehalfOf []string `json:"on_behalf_of"`
}
type registrationReceiptWire struct {
	ID           string                `json:"id"`
	Scope        registrationScopeWire `json:"scope"`
	State        string                `json:"state"`
	Version      int64                 `json:"version"`
	Snapshot     RegistrationSnapshot  `json:"snapshot"`
	FrozenSHA256 string                `json:"frozen_sha256"`
	CreatedAt    time.Time             `json:"created_at"`
	UpdatedAt    time.Time             `json:"updated_at"`
	ReadyAt      time.Time             `json:"ready_at"`
}
type registrationResponseWire struct {
	Schema       int                     `json:"schema"`
	Registration registrationReceiptWire `json:"registration"`
}

func invalidRegistrationProtocol() error { return errors.New("invalid registration protocol value") }
func registrationObservationID(id shoal.ID) bool {
	return strings.HasPrefix(string(id), "observation:") && outcomeDigest(strings.TrimPrefix(string(id), "observation:"))
}
func registrationHash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func NormalizeRegistrationSelection(s RegistrationSelection) (RegistrationSelection, error) {
	if !outcomeID(s.ProfileID) || strings.TrimSpace(string(s.ProfileID)) == "" || !outcomeID(s.ProfileRevisionID) || strings.TrimSpace(string(s.ProfileRevisionID)) == "" || len(s.Sources) == 0 || len(s.Sources) > MaxRegistrationSources {
		return RegistrationSelection{}, invalidRegistrationProtocol()
	}
	out := RegistrationSelection{ProfileID: s.ProfileID, ProfileRevisionID: s.ProfileRevisionID, Sources: make([]RegistrationSourceInput, len(s.Sources))}
	used := 0
	for i, x := range s.Sources {
		if !registrationObservationID(x.ObservationID) || len(x.Bytes) > MaxRegistrationSourceBytes-used {
			return RegistrationSelection{}, invalidRegistrationProtocol()
		}
		used += len(x.Bytes)
		out.Sources[i] = RegistrationSourceInput{x.ObservationID, bytes.Clone(x.Bytes)}
	}
	sort.Slice(out.Sources, func(i, j int) bool { return out.Sources[i].ObservationID < out.Sources[j].ObservationID })
	for i := 1; i < len(out.Sources); i++ {
		if out.Sources[i].ObservationID == out.Sources[i-1].ObservationID {
			return RegistrationSelection{}, invalidRegistrationProtocol()
		}
	}
	return out, nil
}
func EncodeRegistrationRequest(s RegistrationSelection) ([]byte, error) {
	s, e := NormalizeRegistrationSelection(s)
	if e != nil {
		return nil, e
	}
	w := registrationRequestWire{Schema: 1, Selection: registrationSelectionWire{ProfileID: EncodeID(s.ProfileID), ProfileRevisionID: EncodeID(s.ProfileRevisionID), Sources: []registrationSourceWire{}}}
	for _, x := range s.Sources {
		w.Selection.Sources = append(w.Selection.Sources, registrationSourceWire{EncodeID(x.ObservationID), base64.StdEncoding.EncodeToString(x.Bytes)})
	}
	b, e := json.Marshal(w)
	if e != nil || len(b) > MaxRegistrationRequestBytes {
		return nil, invalidRegistrationProtocol()
	}
	return b, nil
}
func DecodeRegistrationRequest(b []byte) (RegistrationSelection, error) {
	var w registrationRequestWire
	if len(b) > MaxRegistrationRequestBytes || decodeStrict(b, &w) != nil || w.Schema != 1 || len(w.Selection.Sources) == 0 || len(w.Selection.Sources) > MaxRegistrationSources {
		return RegistrationSelection{}, invalidRegistrationProtocol()
	}
	var s RegistrationSelection
	var e error
	s.ProfileID, e = DecodeID(w.Selection.ProfileID)
	if e != nil {
		return s, invalidRegistrationProtocol()
	}
	s.ProfileRevisionID, e = DecodeID(w.Selection.ProfileRevisionID)
	if e != nil {
		return RegistrationSelection{}, invalidRegistrationProtocol()
	}
	used := 0
	for _, x := range w.Selection.Sources {
		id, e := DecodeID(x.ObservationID)
		if e != nil || len(x.Bytes) > base64.StdEncoding.EncodedLen(MaxRegistrationSourceBytes-used) {
			return RegistrationSelection{}, invalidRegistrationProtocol()
		}
		raw, e := base64.StdEncoding.Strict().DecodeString(x.Bytes)
		if e != nil || len(raw) > MaxRegistrationSourceBytes-used || base64.StdEncoding.EncodeToString(raw) != x.Bytes {
			return RegistrationSelection{}, invalidRegistrationProtocol()
		}
		used += len(raw)
		s.Sources = append(s.Sources, RegistrationSourceInput{id, raw})
	}
	return NormalizeRegistrationSelection(s)
}
func registrationTime(t time.Time) bool {
	t = t.Round(0).UTC()
	return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999
}
func snapshotIDs(s *RegistrationSnapshot) []*shoal.ID {
	ids := []*shoal.ID{&s.ProfileID, &s.ProfileRevisionID, &s.BuilderID, &s.RequestID, &s.TaskID, &s.PictureID, &s.PredictorID}
	for i := range s.Sources {
		p := &s.Sources[i]
		ids = append(ids, &p.CollectorID, &p.ObservationID, &p.ArtifactID, &p.EnrollmentID, &p.AuthorityPolicyID)
	}
	return ids
}
func ValidateRegistrationReceipt(r RegistrationReceipt) error {
	if !strings.HasPrefix(string(r.ID), "decision-registration:") || !outcomeDigest(strings.TrimPrefix(string(r.ID), "decision-registration:")) || r.State != "ready" || r.Version != 2 || !outcomeDigest(r.FrozenSHA256) || len(r.Scope.Domain) == 0 || len(r.Scope.Domain) > 128 || len(r.Scope.SubjectID) == 0 || len(r.Scope.SubjectID) > shoal.MaxIDBytes || len(r.Scope.ActorID) > shoal.MaxIDBytes || len(r.Scope.ClientID) > shoal.MaxIDBytes || len(r.Scope.OnBehalfOf) > 64 {
		return invalidRegistrationProtocol()
	}
	for _, id := range r.Scope.OnBehalfOf {
		if len(id) == 0 || len(id) > shoal.MaxIDBytes {
			return invalidRegistrationProtocol()
		}
	}
	s := r.Snapshot
	if len(s.Sources) == 0 || len(s.Sources) > MaxRegistrationSources || r.ID != s.RequestID || !outcomeDigest(s.SelectionSHA256) || !outcomeDigest(s.RecordSHA256) || s.RecordBytes < 1 || s.RecordBytes > 48<<20 || !strings.HasPrefix(s.AuthorizationFingerprint, "auth-sha256:") || !outcomeDigest(strings.TrimPrefix(s.AuthorizationFingerprint, "auth-sha256:")) {
		return invalidRegistrationProtocol()
	}
	for _, id := range snapshotIDs(&s) {
		if !outcomeID(*id) || strings.TrimSpace(string(*id)) == "" {
			return invalidRegistrationProtocol()
		}
	}
	for _, t := range []time.Time{s.AcceptedAt, s.AuthenticationExpiresAt, r.CreatedAt, r.UpdatedAt, r.ReadyAt} {
		if !registrationTime(t) {
			return invalidRegistrationProtocol()
		}
	}
	if !s.AcceptedAt.Before(s.AuthenticationExpiresAt) || r.CreatedAt.Before(s.AcceptedAt) || r.ReadyAt.Before(r.CreatedAt) || !r.UpdatedAt.Equal(r.ReadyAt) {
		return invalidRegistrationProtocol()
	}
	seen := map[shoal.ID]bool{}
	for _, p := range s.Sources {
		if !registrationObservationID(p.ObservationID) || seen[p.ObservationID] || p.Generation < 1 || (p.Mode != "imported" && p.Mode != "server_observed") || !outcomeDigest(p.ArtifactSHA256) || !outcomeDigest(p.SourceSHA256) || !registrationTime(p.ReceivedAt) || p.ReceivedAt.After(s.AcceptedAt) {
			return invalidRegistrationProtocol()
		}
		seen[p.ObservationID] = true
	}
	return nil
}

// MatchRegistrationSelection binds every original source's bytes (including
// length via SHA-256) and selected profile. It does not duplicate or purport to
// recompute private registration-store hash encodings.
func MatchRegistrationSelection(s RegistrationSelection, r RegistrationReceipt) bool {
	s, e := NormalizeRegistrationSelection(s)
	if e != nil || ValidateRegistrationReceipt(r) != nil || s.ProfileID != r.Snapshot.ProfileID || s.ProfileRevisionID != r.Snapshot.ProfileRevisionID || len(s.Sources) != len(r.Snapshot.Sources) {
		return false
	}
	pins := map[shoal.ID]string{}
	for _, p := range r.Snapshot.Sources {
		pins[p.ObservationID] = p.ArtifactSHA256
	}
	for _, x := range s.Sources {
		if pins[x.ObservationID] != registrationHash(x.Bytes) {
			return false
		}
	}
	return true
}
func EncodeRegistrationReceipt(r RegistrationReceipt) ([]byte, error) {
	if ValidateRegistrationReceipt(r) != nil {
		return nil, invalidRegistrationProtocol()
	}
	s := r.Snapshot
	s.Sources = append([]RegistrationSourcePin(nil), s.Sources...)
	sort.Slice(s.Sources, func(i, j int) bool { return s.Sources[i].ObservationID < s.Sources[j].ObservationID })
	for _, id := range snapshotIDs(&s) {
		*id = shoal.ID(EncodeID(*id))
	}
	s.AcceptedAt = s.AcceptedAt.UTC()
	s.AuthenticationExpiresAt = s.AuthenticationExpiresAt.UTC()
	for i := range s.Sources {
		s.Sources[i].ReceivedAt = s.Sources[i].ReceivedAt.UTC()
	}
	scope := registrationScopeWire{Domain: base64.RawURLEncoding.EncodeToString(r.Scope.Domain), SubjectID: EncodeID(r.Scope.SubjectID), ActorID: EncodeID(r.Scope.ActorID), ClientID: EncodeID(r.Scope.ClientID), OnBehalfOf: []string{}}
	for _, id := range r.Scope.OnBehalfOf {
		scope.OnBehalfOf = append(scope.OnBehalfOf, EncodeID(id))
	}
	w := registrationResponseWire{1, registrationReceiptWire{EncodeID(r.ID), scope, r.State, r.Version, s, r.FrozenSHA256, r.CreatedAt.UTC(), r.UpdatedAt.UTC(), r.ReadyAt.UTC()}}
	b, e := json.Marshal(w)
	if e != nil || len(b) > MaxRegistrationResponseBytes {
		return nil, invalidRegistrationProtocol()
	}
	return b, nil
}
func decodeRegistrationOpaque(s string, optional bool) (shoal.ID, error) {
	if optional && s == "" {
		return "", nil
	}
	if len(s) > base64.RawURLEncoding.EncodedLen(shoal.MaxIDBytes) {
		return "", invalidRegistrationProtocol()
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(s)
	if e != nil || len(b) == 0 || len(b) > shoal.MaxIDBytes || base64.RawURLEncoding.EncodeToString(b) != s {
		return "", invalidRegistrationProtocol()
	}
	return shoal.ID(b), nil
}
func DecodeRegistrationReceipt(b []byte) (RegistrationReceipt, error) {
	var w registrationResponseWire
	var zero RegistrationReceipt
	if len(b) > MaxRegistrationResponseBytes || decodeStrict(b, &w) != nil || w.Schema != 1 || len(w.Registration.Snapshot.Sources) > MaxRegistrationSources || len(w.Registration.Scope.OnBehalfOf) > 64 {
		return zero, invalidRegistrationProtocol()
	}
	x := w.Registration
	r := RegistrationReceipt{State: x.State, Version: x.Version, Snapshot: x.Snapshot, FrozenSHA256: x.FrozenSHA256, CreatedAt: x.CreatedAt.UTC(), UpdatedAt: x.UpdatedAt.UTC(), ReadyAt: x.ReadyAt.UTC()}
	var e error
	r.ID, e = DecodeID(x.ID)
	if e != nil {
		return zero, invalidRegistrationProtocol()
	}
	for _, id := range snapshotIDs(&r.Snapshot) {
		*id, e = DecodeID(string(*id))
		if e != nil {
			return zero, invalidRegistrationProtocol()
		}
	}
	domain, e := decodeRegistrationOpaque(x.Scope.Domain, false)
	if e != nil {
		return zero, e
	}
	r.Scope.Domain = []byte(domain)
	r.Scope.SubjectID, e = decodeRegistrationOpaque(x.Scope.SubjectID, false)
	if e != nil {
		return zero, e
	}
	r.Scope.ActorID, e = decodeRegistrationOpaque(x.Scope.ActorID, true)
	if e != nil {
		return zero, e
	}
	r.Scope.ClientID, e = decodeRegistrationOpaque(x.Scope.ClientID, true)
	if e != nil {
		return zero, e
	}
	for _, raw := range x.Scope.OnBehalfOf {
		id, e := decodeRegistrationOpaque(raw, false)
		if e != nil {
			return zero, e
		}
		r.Scope.OnBehalfOf = append(r.Scope.OnBehalfOf, id)
	}
	r.Snapshot.AcceptedAt = r.Snapshot.AcceptedAt.UTC()
	r.Snapshot.AuthenticationExpiresAt = r.Snapshot.AuthenticationExpiresAt.UTC()
	for i := range r.Snapshot.Sources {
		r.Snapshot.Sources[i].ReceivedAt = r.Snapshot.Sources[i].ReceivedAt.UTC()
	}
	if ValidateRegistrationReceipt(r) != nil {
		return zero, invalidRegistrationProtocol()
	}
	return r, nil
}
