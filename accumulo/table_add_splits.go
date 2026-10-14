package accumulo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/tablenames"
	"github.com/phrocker/shoal-oss/internal/zk"
)

const (
	// splitRetryAttempts bounds how many times AddTableSplits re-resolves
	// tablets and resubmits the splits Accumulo reported as unapplied.
	// Accumulo's own putSplits loops forever; Shoal bounds the loop so a
	// permanently churning table surfaces ErrTableSplitsIncomplete instead
	// of hanging.
	splitRetryAttempts = 10

	// splitRetryInitialBackoff, splitRetryBackoffStep,
	// splitRetryMaxBackoff, and splitRetryBackoffFactor mirror Accumulo's
	// split Retry builder (retryAfter 100ms, incrementBy 100ms, maxWait 2s,
	// backOffFactor 1.5). With backOffFactor > 1, Accumulo's Retry uses the
	// factor plus ±5% jitter to produce the actual exponential wait
	// schedule; Shoal mirrors that schedule and still caps the outer split
	// loop at splitRetryAttempts.
	splitRetryInitialBackoff = 100 * time.Millisecond
	splitRetryBackoffStep    = 100 * time.Millisecond
	splitRetryMaxBackoff     = 2 * time.Second
	splitRetryBackoffFactor  = 1.5
)

// splitTarget carries the once-resolved state a split round needs.
type splitTarget struct {
	tableName string
	tableID   string
	address   string
	manager   managerclient.Adapter
	discovery *connectorDiscovery
	retry     splitRetryPolicy
}

// splitRetryPolicy bounds how long AddTableSplits keeps re-resolving tablets
// for splits Accumulo reported as unapplied.
type splitRetryPolicy struct {
	attempts       int
	initialBackoff time.Duration
	backoffStep    time.Duration
	maxBackoff     time.Duration
	backoffFactor  float64
	jitter         func() float64
}

func defaultSplitRetryPolicy() splitRetryPolicy {
	return splitRetryPolicy{
		attempts:       splitRetryAttempts,
		initialBackoff: splitRetryInitialBackoff,
		backoffStep:    splitRetryBackoffStep,
		maxBackoff:     splitRetryMaxBackoff,
		backoffFactor:  splitRetryBackoffFactor,
		jitter:         rand.Float64,
	}
}

// splitGroup is one manager TABLE_SPLIT FATE operation: the tablet being
// split plus every new split row that falls strictly inside it.
type splitGroup struct {
	extent TabletExtent
	rows   [][]byte
}

// splitPlan is one round's mapping of the still-pending split rows onto the
// table's current tablets.
type splitPlan struct {
	// groups are new split points, grouped by their containing
	// (PrevRow, EndRow] tablet, in tablet order.
	groups []splitGroup
	// existing are requested rows that already are a tablet's end row.
	existing []TabletExtent
	// unresolved are rows no current tablet covers, which means the tablet
	// list is stale or the table has a metadata gap.
	unresolved [][]byte
}

// AddTableSplits adds split points to an existing table through Accumulo 4's
// manager FATE TABLE_SPLIT operation, the same protocol
// TableOperations.addSplits uses.
//
// splits holds raw, binary-safe split rows; NUL bytes and arbitrary
// non-UTF-8 bytes are preserved exactly. The slice and its rows are copied
// before use, then sorted by unsigned lexicographic row order and
// deduplicated, so callers may reuse or mutate their input afterwards.
// A nil or empty collection, or any nil or zero-length row, is rejected with
// ErrInvalidTableSplit.
//
// tableName must satisfy Accumulo's existing-table-name validator before it
// is resolved; malformed non-empty names fail with ErrInvalidTableName. The
// table also must not be OFFLINE when planning starts, even if every
// requested split row already exists and only mergeability refreshes are
// needed.
//
// Each requested row is mapped to the tablet whose (PrevRow, EndRow] range
// contains it and every row landing in the same tablet is submitted as a
// single FATE operation, exactly as Accumulo's putSplits does. Rows that
// already are a tablet's end row are not resubmitted: Accumulo's addSplits
// instead refreshes the existing tablet's mergeability metadata to the user
// default ("never"), which Shoal does through the manager's
// updateTabletMergeability RPC.
//
// The call waits for every FATE operation it starts, always finishes it —
// even when the operation failed or ctx was canceled — and removes only the
// groups Accumulo reported as split (status "SPLIT_SUCCEEDED"). Groups whose
// tablet moved or split underneath the client are retried against freshly
// resolved tablets with bounded backoff; when rows still remain the call
// fails with ErrTableSplitsIncomplete. Unlike Accumulo, which fans the
// per-tablet operations out across two thread pools, Shoal submits them
// sequentially. To avoid stale recreated-table IDs, the table-name mapping
// is invalidated before ResolveID; after that, only the target table's
// tablet cache is invalidated during split planning and once more before
// return, regardless of outcome.
//
// Errors from the manager are mapped to ErrTableNotFound, ErrTableOffline,
// ErrPermissionDenied, ErrNamespaceNotFound, ErrInvalidTableName and
// ErrManagerUnavailable. The legacy tablet-server splitTablet RPC removed in
// Accumulo 4 is never used.
//
// AddTableSplits resolves tableName to a table ID itself and trusts that
// resolution completely: it has no way to know whether a caller already
// resolved the same name to a table ID earlier and expects this call to
// still be acting against that same table. A caller that does have such an
// expectation — internal/promotion.Promote, which pins the destination's
// table ID before this call and must not let a delete-and-recreate race
// silently redirect a split reconciliation onto the wrong table — should
// call AddTableSplitsForTable instead.
func (c *Connector) AddTableSplits(ctx context.Context, tableName string, splits [][]byte) error {
	return c.addTableSplits(ctx, tableName, "", splits)
}

// AddTableSplitsForTable behaves exactly like AddTableSplits, except it
// additionally requires that its own fresh resolution of table.Name still
// names the table ID the caller already pinned as table.ID, failing
// closed with ErrTableIdentityChanged before any split or mergeability
// mutation is attempted if it does not.
//
// This exists because AddTableSplits's invalidate-then-resolve of
// tableName, on its own, only protects against a *cached, stale* mapping —
// it says nothing about whether the table it resolves to is the same one
// an earlier, separate resolution (by the same or a different call)
// observed. Without this check, a caller like Promote that pins a
// destination table's ID before calling AddTableSplits could have that
// call silently resolve tableName to a *different*, freshly created
// table — deleted and recreated under the identical name in the window
// between the caller's own pin and this call's internal resolve — and
// proceed to add splits (or refresh mergeability) against that unrelated
// replacement table. A later identity check before BulkImport (see
// internal/promotion.verifyDestinationTableIdentity) can still detect
// that the table changed and abort the import, but it cannot undo a
// split or mergeability mutation this call already made against the
// wrong table in the meantime; failing here, before any such mutation is
// attempted, is the only way to avoid making it at all.
//
// table.ID must be non-empty (an empty expectation would defeat the
// point of calling this instead of AddTableSplits and is rejected with
// ErrInvalidTableName) and must have been obtained from a genuinely fresh
// resolution (accumulo.Connector.ResolveTableID, not TableByName, which
// may return an already-cached value) — otherwise this check could pass
// by comparing two equally stale IDs instead of observing a real change.
//
// Like every other identity-pinning check in this package, this narrows
// the vulnerable window rather than eliminating it: it only proves the
// table has not changed identity between the caller's pin and this
// call's own resolve. A delete-and-recreate landing after that resolve
// but during this call's own subsequent tablet-locate, FATE submission,
// or (for split rows that already are a tablet boundary)
// updateTabletMergeability round trips remains possible in principle,
// exactly as AddTableSplits's own doc comment already acknowledges for
// tablets moving or splitting underneath a normal call. The
// updateTabletMergeability path is a further, protocol-level limit on
// how tight this can ever be made: Accumulo's manager RPC for it takes
// the table's qualified *name*, not its ID (see
// managerclient.Pooled.UpdateTabletMergeability's own doc comment), so
// even a client that resolves and pins a table ID cannot make that one
// specific sub-operation itself identity-safe at the wire level — the
// manager re-resolves the name it is given, independent of any ID this
// client already checked. The genuinely new-split path does not share
// that limit: each TABLE_SPLIT FATE request already carries the
// resolved table ID directly (see splitFateRequest), so Accumulo's own
// manager, not just this client-side check, rejects it if the ID no
// longer names a table by the time the operation runs.
func (c *Connector) AddTableSplitsForTable(ctx context.Context, table Table, splits [][]byte) error {
	if table.ID == "" {
		return fmt.Errorf("%w: empty expected table ID for %q", ErrInvalidTableName, table.Name)
	}
	return c.addTableSplits(ctx, table.Name, table.ID, splits)
}

// addTableSplits is AddTableSplits and AddTableSplitsForTable's shared
// core. expectedTableID is empty for the former (no pin to check) and
// non-empty for the latter (see AddTableSplitsForTable's own doc comment
// for exactly what checking it against a fresh resolve does and does not
// prove).
func (c *Connector) addTableSplits(ctx context.Context, tableName, expectedTableID string, splits [][]byte) error {
	if err := validateExistingTableName(tableName); err != nil {
		return err
	}
	pending, err := normalizeSplitRows(splits)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return ErrConnectorClosed
	}
	discovery := c.discovery
	resolver := c.managerAddr
	manager := c.manager
	c.mu.RUnlock()
	if discovery == nil || resolver == nil {
		return ErrDiscoveryUnavailable
	}

	tableID, err := resolveFreshTableID(ctx, discovery.tables, tableName)
	if errors.Is(err, tablenames.ErrTableNotFound) {
		return fmt.Errorf("%w: table name %q", ErrTableNotFound, tableName)
	}
	if err != nil {
		return fmt.Errorf("accumulo: resolve table name %q: %w", tableName, err)
	}
	if expectedTableID != "" && tableID != expectedTableID {
		return fmt.Errorf(
			"%w: table %q (expected table ID %q, resolved %q); another actor likely deleted and recreated it — resolve the destination and retry",
			ErrTableIdentityChanged, tableName, expectedTableID, tableID,
		)
	}
	if err := requireTableNotOffline(ctx, discovery.states, tableID, tableName); err != nil {
		return err
	}

	address, err := resolver.Address(ctx)
	if errors.Is(err, zk.ErrManagerUnavailable) {
		return ErrManagerUnavailable
	}
	if err != nil {
		return fmt.Errorf("accumulo: discover manager: %w", err)
	}

	defer func() {
		discovery.tablets.InvalidateTable(tableID)
	}()

	return addSplits(ctx, splitTarget{
		tableName: tableName,
		tableID:   tableID,
		address:   address,
		manager:   manager,
		discovery: discovery,
		retry:     defaultSplitRetryPolicy(),
	}, pending)
}

func requireTableNotOffline(
	ctx context.Context,
	states tableStateReader,
	tableID, tableName string,
) error {
	if states == nil {
		return nil
	}
	state, err := states.TableState(ctx, tableID)
	if err != nil {
		return fmt.Errorf("accumulo: read table state for %q: %w", tableName, err)
	}
	if !state.Exists {
		return fmt.Errorf("%w: table %q", ErrTableNotFound, tableName)
	}
	if state.State == zk.TableStateOffline {
		return fmt.Errorf("%w: table %q", ErrTableOffline, tableName)
	}
	return nil
}

// addSplits runs bounded split rounds until nothing is pending, a manager
// operation fails, ctx ends, or the retry budget is exhausted.
func addSplits(ctx context.Context, target splitTarget, pending [][]byte) error {
	retry := newSplitRetrySchedule(target.retry)
	for attempt := 0; attempt < target.retry.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Accumulo's putSplits invalidates its tablet cache at the top of
		// every round so each round plans against real extents.
		target.discovery.tablets.InvalidateTable(target.tableID)
		tablets, err := target.discovery.tablets.LocateTable(ctx, target.tableID)
		if err != nil {
			return mapTabletDiscoveryError(target.tableID, nil, err)
		}

		plan := planTableSplits(target.tableID, tablets, pending)
		completed, opErr := applySplitPlan(ctx, target, plan)
		pending = removeSplitRows(pending, completed)
		if opErr != nil {
			return opErr
		}
		if len(pending) == 0 {
			return nil
		}
		if attempt+1 == target.retry.attempts {
			break
		}
		if err := waitForWriterRetry(ctx, retry.currentBackoff()); err != nil {
			return err
		}
		retry.advance()
	}
	return fmt.Errorf(
		"%w: table %q has %d unapplied split rows after %d attempts",
		ErrTableSplitsIncomplete,
		target.tableName,
		len(pending),
		target.retry.attempts,
	)
}

// applySplitPlan submits one round's mergeability refresh and FATE split
// operations, returning the rows Accumulo confirmed. Every group is
// attempted even after an earlier group fails, so a partial multi-tablet
// failure still reports — and retains — the groups that succeeded.
func applySplitPlan(
	ctx context.Context,
	target splitTarget,
	plan splitPlan,
) ([][]byte, error) {
	var (
		completed [][]byte
		failures  []error
	)
	if len(plan.existing) > 0 {
		updated, err := refreshExistingSplits(ctx, target, plan.existing)
		completed = append(completed, updated...)
		if err != nil {
			failures = append(failures, err)
		}
	}
	for _, group := range plan.groups {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		request, err := splitFateRequest(target, group)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		status, err := target.manager.ExecuteStatus(ctx, target.address, request)
		// A successful split whose FATE cleanup failed still split the
		// tablet: keep the rows, but surface the cleanup error.
		if status == managerclient.SplitSucceeded {
			completed = append(completed, group.rows...)
		}
		if err != nil {
			failures = append(failures, mapSplitError(target.tableName, err))
		}
	}
	return completed, errors.Join(failures...)
}

// refreshExistingSplits applies the user-default "never" mergeability to
// tablets whose end row was already the requested split point, mirroring
// the updateTabletMergeability call Accumulo's putSplits makes for existing
// splits. Only the extents the manager accepted are reported as completed;
// the rest are retried in the next round.
func refreshExistingSplits(
	ctx context.Context,
	target splitTarget,
	existing []TabletExtent,
) ([][]byte, error) {
	updates := make([]managerclient.MergeabilityUpdate, 0, len(existing))
	for _, extent := range existing {
		updates = append(updates, managerclient.MergeabilityUpdate{
			Extent: managerclient.TabletExtent{
				TableID:    extent.TableID,
				EndRow:     extent.EndRow,
				PrevEndRow: extent.PrevRow,
			},
			Mergeability: managerclient.NeverMergeable(),
		})
	}
	updated, err := target.manager.UpdateTabletMergeability(
		ctx,
		target.address,
		target.tableName,
		updates,
	)
	rows := make([][]byte, 0, len(updated))
	for _, extent := range updated {
		if len(extent.EndRow) == 0 {
			continue
		}
		rows = append(rows, extent.EndRow)
	}
	if err != nil {
		return rows, mapSplitError(target.tableName, err)
	}
	return rows, nil
}

// splitFateRequest builds the exact Accumulo 4 TABLE_SPLIT FATE request:
// the table's canonical ID, the tablet's end row (empty when unbounded),
// the tablet's previous end row (empty when unbounded), then one encoded
// split/mergeability payload per new split row.
func splitFateRequest(target splitTarget, group splitGroup) (managerclient.Request, error) {
	arguments := make([][]byte, 0, 3+len(group.rows))
	arguments = append(arguments,
		[]byte(target.tableID),
		splitBoundaryArgument(group.extent.EndRow),
		splitBoundaryArgument(group.extent.PrevRow),
	)
	for _, row := range group.rows {
		payload, err := encodeSplitMergeability(row, neverMergeable())
		if err != nil {
			return managerclient.Request{}, err
		}
		arguments = append(arguments, payload)
	}
	return managerclient.Request{
		Operation: managerclient.TableSplit,
		Instance:  fateInstanceForTable(target.tableName),
		Arguments: arguments,
		Options:   map[string]string{},
	}, nil
}

// splitBoundaryArgument encodes an extent boundary: Accumulo sends a
// zero-length buffer for an unbounded (null) end or previous end row.
func splitBoundaryArgument(row []byte) []byte {
	if len(row) == 0 {
		return []byte{}
	}
	return append([]byte{}, row...)
}

// planTableSplits maps every pending row onto the tablet that currently
// contains it. tablets must be in the ascending end-row order the locator
// cache maintains (a nil end row sorts last).
func planTableSplits(tableID string, tablets []metadata.TabletInfo, pending [][]byte) splitPlan {
	plan := splitPlan{}
	groups := make(map[int]int, len(tablets))
	seenExisting := make(map[int]struct{}, len(tablets))
	for _, row := range pending {
		index := findSplitTablet(tablets, row)
		if index < 0 {
			plan.unresolved = append(plan.unresolved, row)
			continue
		}
		tablet := tablets[index]
		extent := TabletExtent{
			TableID: tableID,
			PrevRow: cloneRow(tablet.PrevRow),
			EndRow:  cloneRow(tablet.EndRow),
		}
		if tablet.EndRow != nil && bytes.Equal(tablet.EndRow, row) {
			if _, ok := seenExisting[index]; !ok {
				seenExisting[index] = struct{}{}
				plan.existing = append(plan.existing, extent)
			}
			continue
		}
		position, ok := groups[index]
		if !ok {
			position = len(plan.groups)
			groups[index] = position
			plan.groups = append(plan.groups, splitGroup{extent: extent})
		}
		plan.groups[position].rows = append(plan.groups[position].rows, row)
	}
	return plan
}

// findSplitTablet returns the index of the tablet whose (PrevRow, EndRow]
// range contains row, or -1 when the tablet list does not cover it. It
// matches the locator cache's containment rule: a nil EndRow is positive
// infinity and a nil PrevRow is negative infinity.
func findSplitTablet(tablets []metadata.TabletInfo, row []byte) int {
	index := sort.Search(len(tablets), func(i int) bool {
		end := tablets[i].EndRow
		if end == nil {
			return true
		}
		return bytes.Compare(end, row) >= 0
	})
	if index == len(tablets) {
		return -1
	}
	if prev := tablets[index].PrevRow; prev != nil && bytes.Compare(row, prev) <= 0 {
		return -1
	}
	return index
}

// normalizeSplitRows validates, copies, sorts and deduplicates the caller's
// split rows.
func normalizeSplitRows(splits [][]byte) ([][]byte, error) {
	if len(splits) == 0 {
		return nil, fmt.Errorf("%w: no split rows", ErrInvalidTableSplit)
	}
	rows := make([][]byte, 0, len(splits))
	for i, row := range splits {
		if len(row) == 0 {
			return nil, fmt.Errorf("%w: split row %d is empty", ErrInvalidTableSplit, i)
		}
		rows = append(rows, append([]byte{}, row...))
	}
	sort.Slice(rows, func(i, j int) bool {
		return bytes.Compare(rows[i], rows[j]) < 0
	})
	deduplicated := rows[:1]
	for _, row := range rows[1:] {
		if bytes.Equal(deduplicated[len(deduplicated)-1], row) {
			continue
		}
		deduplicated = append(deduplicated, row)
	}
	return deduplicated, nil
}

// removeSplitRows drops every completed row from pending, preserving the
// sorted order of the remainder.
func removeSplitRows(pending, completed [][]byte) [][]byte {
	if len(completed) == 0 || len(pending) == 0 {
		return pending
	}
	done := make(map[string]struct{}, len(completed))
	for _, row := range completed {
		done[string(row)] = struct{}{}
	}
	remaining := pending[:0]
	for _, row := range pending {
		if _, ok := done[string(row)]; ok {
			continue
		}
		remaining = append(remaining, row)
	}
	return remaining
}

type splitRetrySchedule struct {
	policy        splitRetryPolicy
	backoff       time.Duration
	backoffFactor float64
}

func newSplitRetrySchedule(policy splitRetryPolicy) splitRetrySchedule {
	return splitRetrySchedule{
		policy:        policy,
		backoff:       policy.initialBackoff,
		backoffFactor: policy.backoffFactor,
	}
}

func (r *splitRetrySchedule) currentBackoff() time.Duration {
	return r.backoff
}

func (r *splitRetrySchedule) advance() {
	if r.backoff >= r.policy.maxBackoff {
		r.backoff = r.policy.maxBackoff
		return
	}
	if r.policy.backoffFactor <= 1 {
		r.backoff = nextLinearSplitBackoff(r.backoff, r.policy)
		return
	}
	jitter := 0.0
	if r.policy.jitter != nil {
		sample := r.policy.jitter()
		if sample < 0 {
			sample = 0
		} else if sample > 1 {
			sample = 1
		}
		jitter = (sample - 0.5) / 10.0
	}
	waitFactor := (1 + jitter) * r.backoffFactor
	r.backoffFactor *= r.policy.backoffFactor
	initialMillis := r.policy.initialBackoff.Milliseconds()
	if initialMillis <= 0 {
		initialMillis = 1
	}
	increment := time.Duration(math.Ceil(waitFactor*float64(initialMillis))) * time.Millisecond
	next := r.policy.initialBackoff + increment
	if next > r.policy.maxBackoff {
		r.backoff = r.policy.maxBackoff
		return
	}
	r.backoff = next
}

func nextLinearSplitBackoff(backoff time.Duration, policy splitRetryPolicy) time.Duration {
	next := backoff + policy.backoffStep
	if next > policy.maxBackoff {
		return policy.maxBackoff
	}
	return next
}

// mapSplitError maps manager failures onto the package's public sentinels
// while preserving the original error chain, so a joined FATE cleanup
// failure stays reachable through errors.Is alongside the sentinel.
func mapSplitError(tableName string, err error) error {
	if managerclient.IsRetryableEndpointError(err) {
		return fmt.Errorf("%w: table %q: %w", ErrManagerUnavailable, tableName, err)
	}
	var managerErr *managerclient.Error
	if !errors.As(err, &managerErr) {
		return fmt.Errorf("accumulo: add splits to table %q: %w", tableName, err)
	}
	name := managerErr.TableName
	if name == "" {
		name = tableName
	}
	var sentinel error
	switch managerErr.Kind {
	case managerclient.ErrorTableNotFound:
		sentinel = ErrTableNotFound
	case managerclient.ErrorTableOffline:
		sentinel = ErrTableOffline
	case managerclient.ErrorNamespaceNotFound:
		sentinel = ErrNamespaceNotFound
	case managerclient.ErrorInvalidName:
		sentinel = ErrInvalidTableName
	case managerclient.ErrorSecurity:
		sentinel = ErrPermissionDenied
	case managerclient.ErrorNotActive:
		sentinel = ErrManagerUnavailable
	default:
		return fmt.Errorf("accumulo: add splits to table %q: %w", name, err)
	}
	return fmt.Errorf("%w: table %q: %w", sentinel, name, err)
}
