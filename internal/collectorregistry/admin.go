// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package collectorregistry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Collector administration (#446 slice 5).
//
// AdminProvision, AdminRevoke and AdminRegistration are the HTTP-facing
// counterparts of Provision, Revoke and Registration. They require
// auth.OperationCollectorAdmin in the caller's own authorization domain and
// act only on registrations in that domain. A delegated decision never acts
// as an admin, and an admin can never provision its own subject or actor as a
// collector.
//
// The change log. Every provisioning and revocation that changes a
// registration, through either API, writes one immutable entry in the same
// one-row compare-and-set mutation as the registration: neither is durable
// without the other. There is no head and no global sequence. An entry's
// place is its slot, (generation, action):
//
//   - a provisioning that creates the registration writes (1, provision);
//   - a revocation moves generation g to g+1 and writes (g+1, revoke);
//   - a provisioning that reactivates a revoked registration at g writes
//     (g, provision).
//
// So a registration at generation G expects exactly (1, provision), then
// (g, revoke) and (g, provision) for every g in 2..G, except (G, provision)
// while it is revoked. The qualifier is the slot plus the write attempt's
// random nonce, and the write is conditioned only on that exact cell being
// absent. No other cell, damaged or stray, can block it; a write depends on
// nothing but its own registration row.
//
// Readers scan the collector's entries and check them against the slots the
// registration implies. A slot without an entry is a gap; a cell that is not
// exactly one valid entry for its slot is an anomaly. Both are reported, and
// neither ever blocks a write. Entries live in their own column family of the
// registration's row, so no cell the existing flows write changes
// (testdata/persisted_rows.golden.json).

// ChangeAction is what a change log entry did.
type ChangeAction string

const (
	ChangeProvision ChangeAction = "provision"
	ChangeRevoke    ChangeAction = "revoke"
)

// ChangeSource is the API a change came through.
type ChangeSource string

const (
	// SourceOperatorAPI is the trusted Go API (Provision, Revoke), which has
	// no caller identity.
	SourceOperatorAPI ChangeSource = "operator_api"
	// SourceCollectorAdmin is the HTTP admin routes, attributed to the admin.
	SourceCollectorAdmin ChangeSource = "collector_admin"
)

// AdminAttribution identifies the admin decision behind a change.
type AdminAttribution struct {
	Subject                  shoal.ID
	Actor                    shoal.ID
	ClientID                 shoal.ID
	Domain                   []byte
	RequestID                shoal.ID
	CorrelationID            shoal.ID
	AuthorizationFingerprint string
}

// ChangeVersion is the entry format this build writes. Decoders accept
// every known version and refuse others explicitly (decodeChange).
const ChangeVersion = 1

// Change is one immutable change log entry: the registration as written, who
// wrote it and when. Its slot is (Registration.Generation, Action).
type Change struct {
	Version      int
	CollectorID  shoal.ID
	Action       ChangeAction
	Source       ChangeSource
	Registration collector.Registration
	// Admin is set exactly when Source is SourceCollectorAdmin.
	Admin *AdminAttribution
	At    time.Time
	// Attempt is random per write attempt and is part of the entry's
	// qualifier, so every attempt writes a cell of its own; after a lost
	// acknowledgement, reading back that exact cell tells a writer whether
	// its mutation applied.
	Attempt string
}

// Generation is the generation of the entry's slot.
func (c Change) Generation() int64 { return c.Registration.Generation }

// LogGap is a slot the registration implies that holds no entry.
type LogGap struct {
	Generation int64
	Action     ChangeAction
}

// AnomalyReason says why a cell in the change log is not an entry.
type AnomalyReason string

const (
	// AnomalyUndecodable: the cell is not a decodable entry.
	AnomalyUndecodable AnomalyReason = "undecodable"
	// AnomalyUnknownVersion: the entry's format version is not one this
	// build knows.
	AnomalyUnknownVersion AnomalyReason = "unknown_version"
	// AnomalyMisplaced: the entry's content does not match its qualifier.
	AnomalyMisplaced AnomalyReason = "misplaced"
	// AnomalyUnexpected: the entry is in a slot the registration does not
	// imply (a generation above it, or a provisioning while revoked).
	AnomalyUnexpected AnomalyReason = "unexpected"
	// AnomalyDuplicate: more than one entry occupies one slot. The registry
	// writes at most one per slot (the registration compare-and-set admits
	// one change per slot), so neither is presented.
	AnomalyDuplicate AnomalyReason = "duplicate"
	// AnomalyInconsistent: the newest slot's entry describes a registration
	// other than the current one.
	AnomalyInconsistent AnomalyReason = "inconsistent"
)

// LogAnomaly is a cell that is not exactly one valid entry for its slot.
// Generation and Action are those of the slot when the qualifier names one.
type LogAnomaly struct {
	Generation int64
	Action     ChangeAction
	Reason     AnomalyReason
}

// ChangeLog is a range of a collector's change log as checked against its
// registration: the entries found, oldest first, the gaps and the anomalies.
type ChangeLog struct {
	Registration collector.Registration
	Entries      []Change
	Gaps         []LogGap
	Anomalies    []LogAnomaly
}

// AdminResult is the outcome of an admin write. Changed reports whether this
// call made the change; a retry of a change that already applied reports
// false with the same registration.
type AdminResult struct {
	Registration collector.Registration
	Changed      bool
}

// AdminView is a registration as an admin reads it, with the change log for
// its most recent generations (from FromGeneration), newest entry first.
type AdminView struct {
	Registration   collector.Registration
	EnrollmentID   shoal.ID
	FromGeneration int64
	Changes        []Change
	Gaps           []LogGap
	Anomalies      []LogAnomaly
}

const (
	// ViewGenerations is how many recent generations AdminRegistration
	// checks: at most two entries each, so at most 16 entries.
	ViewGenerations = 8
	// MaxChangesGenerations bounds one Changes call.
	MaxChangesGenerations = 128
	// maxLogCells bounds the cells one read examines.
	maxLogCells = 4 * MaxChangesGenerations
)

type attribution struct {
	source ChangeSource
	admin  *AdminAttribution
	at     time.Time
}

func (r *Registry) operator() (attribution, error) {
	now, e := r.now()
	if e != nil {
		return attribution{}, e
	}
	return attribution{source: SourceOperatorAPI, at: now}, nil
}

func (a attribution) record(action ChangeAction) func(registrationRow) (Change, error) {
	return func(next registrationRow) (Change, error) {
		c := Change{Version: ChangeVersion, CollectorID: next.Registration.CollectorID, Action: action, Source: a.source, Registration: next.Registration, At: a.at}
		if a.admin != nil {
			admin := *a.admin
			admin.Domain = bytes.Clone(admin.Domain)
			c.Admin = &admin
		}
		return c, nil
	}
}

const (
	changeFamily = "a"
	entryPrefix  = "entry:"
	scanPage     = 256
)

// slotCode orders the two slots of a generation as they happen: a revoke
// moves into g before a reactivating provisioning at g.
func slotCode(a ChangeAction) (byte, bool) {
	switch a {
	case ChangeRevoke:
		return 'a', true
	case ChangeProvision:
		return 'b', true
	}
	return 0, false
}

func (r *Registry) changeCoordinate(id shoal.ID, qualifier string) allocator.Coordinate {
	return allocator.Coordinate{Row: r.registrationCoordinate(id).Row, Family: []byte(changeFamily), Qualifier: []byte(qualifier), Visibility: bytes.Clone(r.config.Visibility)}
}

func entryQualifier(generation int64, action ChangeAction, attempt string) string {
	code, _ := slotCode(action)
	return fmt.Sprintf("%s%020d:%c:%s", entryPrefix, generation, code, attempt)
}

func generationPrefix(generation int64) []byte {
	return []byte(fmt.Sprintf("%s%020d:", entryPrefix, generation))
}

// parseQualifier recovers a slot and attempt from a qualifier the registry
// could have written, ok false otherwise.
func parseQualifier(q []byte) (generation int64, action ChangeAction, attempt string, ok bool) {
	rest, found := strings.CutPrefix(string(q), entryPrefix)
	parts := strings.Split(rest, ":")
	if !found || len(parts) != 3 || len(parts[0]) != 20 || len(parts[1]) != 1 || len(parts[2]) != 32 {
		return 0, "", "", false
	}
	n, e := strconv.ParseInt(parts[0], 10, 64)
	if e != nil || n <= 0 || fmt.Sprintf("%020d", n) != parts[0] {
		return 0, "", "", false
	}
	switch parts[1] {
	case "a":
		action = ChangeRevoke
	case "b":
		action = ChangeProvision
	default:
		return 0, "", "", false
	}
	if _, e := hex.DecodeString(parts[2]); e != nil || strings.ToLower(parts[2]) != parts[2] {
		return 0, "", "", false
	}
	return n, action, parts[2], true
}

type auditWrite struct {
	qualifier  string
	value      []byte
	conditions []allocator.Condition
	updates    []allocator.Update
}

// changeV1, registrationV1 and attributionV1 are the frozen storage form of
// version 1 entries. Version 1 decodes into these, not into the live types,
// so a field added to Change, collector.Registration or AdminAttribution
// later cannot change how a stored version 1 entry decodes or re-encodes;
// such a change takes a new version. Their field names and order are the
// format (testdata/change_v1.golden.json pins it).
type changeV1 struct {
	Version      int
	CollectorID  shoal.ID
	Action       ChangeAction
	Source       ChangeSource
	Registration registrationV1
	Admin        *attributionV1
	At           time.Time
	Attempt      string
}

type registrationV1 struct {
	CollectorID        shoal.ID
	Subject            shoal.ID
	ClientID           shoal.ID
	Domain             []byte
	AuthorityPolicyIDs []shoal.ID
	Control            collector.Control
	Mode               collector.Mode
	Generation         int64
	State              collector.State
}

type attributionV1 struct {
	Subject                  shoal.ID
	Actor                    shoal.ID
	ClientID                 shoal.ID
	Domain                   []byte
	RequestID                shoal.ID
	CorrelationID            shoal.ID
	AuthorizationFingerprint string
}

func toV1(c Change) changeV1 {
	r := c.Registration
	out := changeV1{Version: c.Version, CollectorID: c.CollectorID, Action: c.Action, Source: c.Source, At: c.At, Attempt: c.Attempt,
		Registration: registrationV1{CollectorID: r.CollectorID, Subject: r.Subject, ClientID: r.ClientID, Domain: r.Domain, AuthorityPolicyIDs: r.AuthorityPolicyIDs, Control: r.Control, Mode: r.Mode, Generation: r.Generation, State: r.State}}
	if a := c.Admin; a != nil {
		out.Admin = &attributionV1{Subject: a.Subject, Actor: a.Actor, ClientID: a.ClientID, Domain: a.Domain, RequestID: a.RequestID, CorrelationID: a.CorrelationID, AuthorizationFingerprint: a.AuthorizationFingerprint}
	}
	return out
}

func fromV1(v changeV1) Change {
	r := v.Registration
	out := Change{Version: v.Version, CollectorID: v.CollectorID, Action: v.Action, Source: v.Source, At: v.At, Attempt: v.Attempt,
		Registration: collector.Registration{CollectorID: r.CollectorID, Subject: r.Subject, ClientID: r.ClientID, Domain: r.Domain, AuthorityPolicyIDs: r.AuthorityPolicyIDs, Control: r.Control, Mode: r.Mode, Generation: r.Generation, State: r.State}}
	if a := v.Admin; a != nil {
		out.Admin = &AdminAttribution{Subject: a.Subject, Actor: a.Actor, ClientID: a.ClientID, Domain: a.Domain, RequestID: a.RequestID, CorrelationID: a.CorrelationID, AuthorizationFingerprint: a.AuthorizationFingerprint}
	}
	return out
}

// encodeChange stores c in this build's version.
func encodeChange(c Change) ([]byte, error) { return encode(toV1(c)) }

// errUnknownVersion marks an entry whose format version this build does not
// know. Readers report it as an anomaly; it is never guessed at.
var errUnknownVersion = errors.New("collector change entry version unknown")

// decodeChange decodes one stored entry. The envelope and the version are
// read first; each known version then decodes strictly into its own struct,
// so a later build can add a version without making these entries
// unreadable, and this build refuses a version it does not know explicitly.
func decodeChange(raw []byte) (Change, error) {
	var payload json.RawMessage
	if decode(raw, &payload) != nil {
		return Change{}, ErrUnavailable
	}
	var probe struct{ Version int }
	if json.Unmarshal(payload, &probe) != nil {
		return Change{}, ErrUnavailable
	}
	switch probe.Version {
	case 1:
		var v changeV1
		if decode(raw, &v) != nil || v.Version != 1 {
			return Change{}, ErrUnavailable
		}
		return fromV1(v), nil
	default:
		return Change{}, errUnknownVersion
	}
}

// prepareChange builds the entry for next's change: at its slot plus a fresh
// nonce, conditioned only on that exact cell being absent. It reads nothing,
// so the state of the rest of the log can never block a change.
func (r *Registry) prepareChange(id shoal.ID, next registrationRow, record func(registrationRow) (Change, error)) (auditWrite, error) {
	change, e := record(next)
	if e != nil {
		return auditWrite{}, e
	}
	var attempt [16]byte
	if _, e := rand.Read(attempt[:]); e != nil {
		return auditWrite{}, ErrUnavailable
	}
	change.Attempt = hex.EncodeToString(attempt[:])
	if validChange(change, id) != nil {
		return auditWrite{}, invalid()
	}
	value, e := encodeChange(change)
	if e != nil {
		return auditWrite{}, invalid()
	}
	// Never write what the readers would refuse: the entry must decode back
	// to exactly what was meant, under the rules readers apply.
	if back, e := decodeChange(value); e != nil || validChange(back, id) != nil || !reflect.DeepEqual(back, change) {
		return auditWrite{}, invalid()
	}
	qualifier := entryQualifier(change.Generation(), change.Action, change.Attempt)
	entry := r.changeCoordinate(id, qualifier)
	return auditWrite{
		qualifier:  qualifier,
		value:      value,
		conditions: []allocator.Condition{{Coordinate: entry, Absent: true}},
		updates:    []allocator.Update{{Coordinate: entry, Timestamp: 1, Value: value}},
	}, nil
}

// changeApplied reports whether the entry w would write is the one stored.
func (r *Registry) changeApplied(ctx context.Context, id shoal.ID, w auditWrite) (bool, error) {
	cell, e := r.read(ctx, r.changeCoordinate(id, w.qualifier))
	if shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	return bytes.Equal(cell.Value, w.value), nil
}

func validChange(c Change, id shoal.ID) error {
	// The time and attribution bounds are exactly the wire's
	// (pkg/collector/api), so an entry this accepts is one the HTTP read
	// can present.
	if c.Version != ChangeVersion || c.CollectorID != id || c.Registration.CollectorID != id || c.At.IsZero() || c.At.Year() < 1 || c.At.Year() > 9999 || len(c.Attempt) != 32 {
		return invalid()
	}
	if _, e := c.Registration.Canonical(); e != nil {
		return invalid()
	}
	switch {
	case c.Action == ChangeRevoke && (c.Registration.State != collector.Revoked || c.Registration.Generation < 2):
		return invalid()
	case c.Action == ChangeProvision && c.Registration.State != collector.Provisioned:
		return invalid()
	case c.Action != ChangeRevoke && c.Action != ChangeProvision:
		return invalid()
	}
	switch c.Source {
	case SourceOperatorAPI:
		if c.Admin != nil {
			return invalid()
		}
	case SourceCollectorAdmin:
		if c.Admin == nil || validAttribution(*c.Admin) != nil || !bytes.Equal(c.Admin.Domain, c.Registration.Domain) {
			return invalid()
		}
	default:
		return invalid()
	}
	return nil
}

// validAttribution applies one rule to every recorded admin identity:
// required where the decision requires it, within shoal.MaxIDBytes, valid
// UTF-8 and printable. encoding/json would silently rewrite invalid UTF-8 to
// U+FFFD, producing an entry that no longer decodes to itself, and the wire
// validator refuses such IDs. The correlation ID uses the interaction rule
// verbatim (interaction.ValidateCorrelationID). Subjects, actors, clients and
// request IDs may contain an ASCII space, as OIDC-mapped subjects do, so they
// use the same rule without the no-space clause.
func validAttribution(a AdminAttribution) error {
	for _, field := range []struct {
		id       shoal.ID
		required bool
	}{{a.Subject, true}, {a.Actor, true}, {a.ClientID, false}, {a.RequestID, true}} {
		if !printableID(field.id, field.required) {
			return invalid()
		}
	}
	if interaction.ValidateCorrelationID(a.CorrelationID) != nil || a.AuthorizationFingerprint == "" || len(a.AuthorizationFingerprint) > maxFingerprintBytes || !printableID(shoal.ID(a.AuthorizationFingerprint), true) || len(a.Domain) == 0 || len(a.Domain) > shoal.MaxIDBytes {
		return invalid()
	}
	return nil
}

// maxFingerprintBytes is the wire's bound on an authorization fingerprint.
const maxFingerprintBytes = 128

func printableID(id shoal.ID, required bool) bool {
	if id == "" {
		return !required
	}
	if len(id) > shoal.MaxIDBytes || !utf8.ValidString(string(id)) {
		return false
	}
	for _, r := range string(id) {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

type rowScanner interface {
	ScanRowPrefix(context.Context, []byte, []byte, []byte, []byte, int) ([]allocator.Cell, error)
}

type slot struct {
	generation int64
	action     ChangeAction
}

// expectedSlots lists, oldest first, the slots reg implies within
// [from, to].
func expectedSlots(reg collector.Registration, from, to int64) []slot {
	var out []slot
	for g := max(from, 1); g <= min(to, reg.Generation); g++ {
		if g >= 2 {
			out = append(out, slot{g, ChangeRevoke})
		}
		if g < reg.Generation || reg.State != collector.Revoked {
			out = append(out, slot{g, ChangeProvision})
		}
	}
	return out
}

// scanLog reads every cell of id's change log for generations [from, to],
// paging until a page comes back empty (a store may apply the limit before
// filtering) or a qualifier past the range appears.
func (r *Registry) scanLog(ctx context.Context, id shoal.ID, from, to int64) ([]allocator.Cell, error) {
	scanner, ok := r.config.Backend.(rowScanner)
	if !ok {
		// Listing needs row scans; writes do not.
		return nil, ErrUnavailable
	}
	row := r.registrationCoordinate(id).Row
	start, end := generationPrefix(from), generationPrefix(to+1)
	var out []allocator.Cell
	for {
		cells, e := scanner.ScanRowPrefix(ctx, row, []byte(changeFamily), start, bytes.Clone(r.config.Visibility), scanPage)
		if e != nil {
			return nil, ErrUnavailable
		}
		if len(cells) == 0 {
			return out, nil
		}
		for _, cell := range cells {
			if bytes.Compare(cell.Coordinate.Qualifier, end) >= 0 || !bytes.HasPrefix(cell.Coordinate.Qualifier, []byte(entryPrefix)) {
				return out, nil
			}
			out = append(out, cell)
			if len(out) > maxLogCells {
				return nil, ErrUnavailable
			}
		}
		next := append(bytes.Clone(cells[len(cells)-1].Coordinate.Qualifier), 0)
		if bytes.Compare(next, start) <= 0 {
			return nil, ErrUnavailable
		}
		start = next
	}
}

// checkLog sorts cells into entries, gaps and anomalies against the slots
// reg implies within [from, to].
func checkLog(id shoal.ID, reg collector.Registration, from, to int64, cells []allocator.Cell) ChangeLog {
	log := ChangeLog{Registration: reg}
	found := map[slot][]Change{}
	for _, cell := range cells {
		g, action, attempt, ok := parseQualifier(cell.Coordinate.Qualifier)
		if !ok {
			log.Anomalies = append(log.Anomalies, LogAnomaly{Reason: AnomalyMisplaced})
			continue
		}
		c, e := decodeChange(cell.Value)
		switch {
		case errors.Is(e, errUnknownVersion):
			log.Anomalies = append(log.Anomalies, LogAnomaly{g, action, AnomalyUnknownVersion})
			continue
		case e != nil || validChange(c, id) != nil:
			log.Anomalies = append(log.Anomalies, LogAnomaly{g, action, AnomalyUndecodable})
			continue
		case c.Generation() != g || c.Action != action || c.Attempt != attempt || !bytes.Equal(c.Registration.Domain, reg.Domain) || c.Registration.Subject != reg.Subject || c.Registration.ClientID != reg.ClientID:
			log.Anomalies = append(log.Anomalies, LogAnomaly{g, action, AnomalyMisplaced})
			continue
		}
		found[slot{g, action}] = append(found[slot{g, action}], c)
	}
	expected := expectedSlots(reg, from, to)
	want := map[slot]bool{}
	for _, s := range expected {
		want[s] = true
	}
	for s := range found {
		if !want[s] {
			log.Anomalies = append(log.Anomalies, LogAnomaly{s.generation, s.action, AnomalyUnexpected})
		}
	}
	for i, s := range expected {
		entries := found[s]
		switch {
		case len(entries) == 0:
			log.Gaps = append(log.Gaps, LogGap{s.generation, s.action})
		case len(entries) > 1:
			log.Anomalies = append(log.Anomalies, LogAnomaly{s.generation, s.action, AnomalyDuplicate})
			log.Gaps = append(log.Gaps, LogGap{s.generation, s.action})
		default:
			c := entries[0]
			// The newest slot's entry must describe the current
			// registration; an older one, the registration as it was.
			if i == len(expected)-1 && s.generation == reg.Generation && !sameLoggedState(c.Registration, reg) {
				log.Anomalies = append(log.Anomalies, LogAnomaly{s.generation, s.action, AnomalyInconsistent})
			}
			log.Entries = append(log.Entries, c)
		}
	}
	// The anomalies about entries that are presented or withheld
	// (inconsistent, duplicate) come first, so a bounded listing never drops
	// them; then by generation, newest first, and reason.
	rank := func(a LogAnomaly) int {
		switch a.Reason {
		case AnomalyInconsistent:
			return 0
		case AnomalyDuplicate:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(log.Anomalies, func(a, b LogAnomaly) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		if a.Generation != b.Generation {
			if a.Generation > b.Generation {
				return -1
			}
			return 1
		}
		if c := strings.Compare(string(a.Action), string(b.Action)); c != 0 {
			return c
		}
		return strings.Compare(string(a.Reason), string(b.Reason))
	})
	return log
}

// sameLoggedState compares what the log records of a registration: its
// generation, whether it is revoked, and its terms. Enrollment changes the
// registration without a logged change, so it is not compared.
func sameLoggedState(a, b collector.Registration) bool {
	return a.Generation == b.Generation && (a.State == collector.Revoked) == (b.State == collector.Revoked) && slices.Equal(a.AuthorityPolicyIDs, b.AuthorityPolicyIDs) && a.Control == b.Control && a.Mode == b.Mode
}

// readLog reads id's registration and its change log for generations
// [from, to], between two registration reads that agree on the logged
// state, so the entries and the registration describe the same moment.
// domain, when set, is checked before anything about the log is read.
func (r *Registry) readLog(ctx context.Context, id shoal.ID, domain []byte, from, to func(collector.Registration) int64) (registrationRow, ChangeLog, error) {
	for range maxCASAttempts {
		before, _, e := r.readRegistration(ctx, id)
		if e != nil {
			return registrationRow{}, ChangeLog{}, e
		}
		if domain != nil && !bytes.Equal(before.Registration.Domain, domain) {
			return registrationRow{}, ChangeLog{}, auth.ObjectNotFound()
		}
		lo, hi := from(before.Registration), to(before.Registration)
		// A range that reaches the current generation also reads every
		// entry above it, which the registration does not imply.
		scanTo := hi
		if hi >= before.Registration.Generation {
			scanTo = math.MaxInt64 - 1
		}
		cells, e := r.scanLog(ctx, id, lo, scanTo)
		if e != nil {
			return registrationRow{}, ChangeLog{}, e
		}
		after, _, e := r.readRegistration(ctx, id)
		if e != nil {
			return registrationRow{}, ChangeLog{}, e
		}
		if !sameLoggedState(before.Registration, after.Registration) {
			continue
		}
		return after, checkLog(id, after.Registration, lo, hi, cells), nil
	}
	// The registration kept changing under the read: concurrency, not
	// damage, so a retryable conflict.
	return registrationRow{}, ChangeLog{}, ErrConflict
}

// Changes is a trusted operator read of id's change log with no caller
// check, for generations [from, from+generations-1] (clamped to the current
// generation), entries oldest first, with the gaps and anomalies in that
// range.
func (r *Registry) Changes(ctx context.Context, id shoal.ID, from int64, generations int) (ChangeLog, error) {
	if shoal.ValidateRequiredID("collector ID", id) != nil || from <= 0 || generations <= 0 || generations > MaxChangesGenerations || from > math.MaxInt64-int64(generations) {
		return ChangeLog{}, invalid()
	}
	_, log, e := r.readLog(ctx, id, nil, func(collector.Registration) int64 { return from }, func(reg collector.Registration) int64 {
		return min(reg.Generation, from+int64(generations)-1)
	})
	return log, e
}

// admin resolves a collector-admin decision. Delegated decisions, service
// decisions and executor-bound decisions never administer collectors.
func (r *Registry) admin(ctx context.Context) (auth.Decision, auth.Fingerprint, attribution, error) {
	d, fp, now, e := r.caller(ctx, auth.OperationCollectorAdmin)
	if e != nil {
		return auth.Decision{}, auth.Fingerprint{}, attribution{}, e
	}
	if len(d.OnBehalfOf()) != 0 || d.TrustedService() || d.ExecutorBinding() != "" {
		return auth.Decision{}, auth.Fingerprint{}, attribution{}, unauthorized()
	}
	admin := &AdminAttribution{
		Subject: d.Subject(), Actor: d.Actor(), ClientID: d.ClientID(), Domain: d.AuthorizationDomain(),
		RequestID: d.RequestID(), CorrelationID: d.CorrelationID(), AuthorizationFingerprint: fp.String(),
	}
	// An admin whose identity cannot be recorded faithfully is not an admin:
	// it is refused before anything is read or written.
	if validAttribution(*admin) != nil {
		return auth.Decision{}, auth.Fingerprint{}, attribution{}, unauthorized()
	}
	return d, fp, attribution{source: SourceCollectorAdmin, at: now, admin: admin}, nil
}

// selfProvisioning reports whether p would make the admin a collector: its
// subject is the admin's subject or actor. A collector's principal is its
// (domain, subject, client); refusing on the subject alone also refuses the
// admin's own subject under another client.
func selfProvisioning(p Provisioning, d auth.Decision) bool {
	return p.Subject == d.Subject() || p.Subject == d.Actor()
}

// finishAdmin re-checks the admin's authorization after the write. A changed
// authorization after a write leaves the outcome for the caller to reconcile.
func (r *Registry) finishAdmin(ctx context.Context, fp auth.Fingerprint, row registrationRow, changed bool, e error) (AdminResult, error) {
	if e != nil {
		return AdminResult{}, e
	}
	if e := r.recheck(ctx, auth.OperationCollectorAdmin, fp); e != nil {
		if changed {
			return AdminResult{}, indeterminate(e)
		}
		return AdminResult{}, e
	}
	return AdminResult{Registration: row.Registration, Changed: changed}, nil
}

// AdminProvision is Provision for a collector-admin decision. generation is
// the generation the caller expects the registration to hold afterwards: 1 to
// create, the current generation to reactivate a revoked registration or to
// confirm an identical active one. Anything else, a different identity, or
// different terms on an active registration is a conflict; the request is
// never merged with what is stored. The collector must be in the admin's own
// authorization domain, and its subject may not be the admin's subject or
// actor.
func (r *Registry) AdminProvision(ctx context.Context, p Provisioning, generation int64) (AdminResult, error) {
	if _, e := (collector.Registration{CollectorID: p.CollectorID, Subject: p.Subject, ClientID: p.ClientID, Domain: p.Domain, AuthorityPolicyIDs: p.AuthorityPolicyIDs, Control: p.Control, Mode: p.Mode, Generation: 1, State: collector.Provisioned}).Canonical(); e != nil || generation <= 0 {
		return AdminResult{}, invalid()
	}
	d, fp, by, e := r.admin(ctx)
	if e != nil {
		return AdminResult{}, e
	}
	if !bytes.Equal(p.Domain, d.AuthorizationDomain()) {
		return AdminResult{}, denied("collector domain is not the admin's authorization domain")
	}
	if selfProvisioning(p, d) {
		return AdminResult{}, denied("an admin cannot provision itself as a collector")
	}
	row, changed, e := r.provision(ctx, p, generation, by)
	return r.finishAdmin(ctx, fp, row, changed, e)
}

// AdminRevoke is Revoke for a collector-admin decision. generation is the
// generation being revoked: revoking it again is a no-op, any other
// generation a conflict. A collector outside the admin's authorization domain
// reads as not found, exactly like one that does not exist.
func (r *Registry) AdminRevoke(ctx context.Context, id shoal.ID, generation int64) (AdminResult, error) {
	if shoal.ValidateRequiredID("collector ID", id) != nil || generation <= 0 {
		return AdminResult{}, invalid()
	}
	d, fp, by, e := r.admin(ctx)
	if e != nil {
		return AdminResult{}, e
	}
	domain := d.AuthorizationDomain()
	row, changed, e := r.revoke(ctx, id, generation, by, func(reg collector.Registration) error {
		if !bytes.Equal(reg.Domain, domain) {
			return auth.ObjectNotFound()
		}
		return nil
	})
	return r.finishAdmin(ctx, fp, row, changed, e)
}

// AdminRegistration reads a registration in the admin's own authorization
// domain with the change log of its most recent ViewGenerations generations,
// newest entry first, and the gaps and anomalies there. Quarantine is not
// part of it: observation status is computed from the current registration
// at every read and never cached here.
func (r *Registry) AdminRegistration(ctx context.Context, id shoal.ID) (AdminView, error) {
	if shoal.ValidateRequiredID("collector ID", id) != nil {
		return AdminView{}, invalid()
	}
	d, fp, _, e := r.admin(ctx)
	if e != nil {
		return AdminView{}, e
	}
	from := func(reg collector.Registration) int64 { return max(1, reg.Generation-ViewGenerations+1) }
	row, log, e := r.readLog(ctx, id, d.AuthorizationDomain(), from, func(reg collector.Registration) int64 { return reg.Generation })
	if e != nil {
		return AdminView{}, e
	}
	view := AdminView{Registration: row.Registration, EnrollmentID: currentEnrollment(row), FromGeneration: from(row.Registration), Gaps: log.Gaps, Anomalies: log.Anomalies}
	for i := len(log.Entries) - 1; i >= 0; i-- {
		view.Changes = append(view.Changes, log.Entries[i])
	}
	if e := r.recheck(ctx, auth.OperationCollectorAdmin, fp); e != nil {
		return AdminView{}, auth.ObjectNotFound()
	}
	return view, nil
}
