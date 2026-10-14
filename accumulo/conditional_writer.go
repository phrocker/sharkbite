package accumulo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/internal/ingestclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

const (
	defaultConditionalWriterMaxRetries   = 3
	defaultConditionalWriterRetryBackoff = 100 * time.Millisecond
	maxConditionalWriterRetries          = 100
	maxConditionalWriterRetryBackoff     = time.Minute
	// One TXN-root predecessor plus absence checks for all 64 initial LPART
	// commit copies must fit in one row-atomic publication mutation.
	maxConditionalConditions     = 65
	maxConditionalComponentBytes = 1 << 20
	maxConditionalMutationBytes  = 16 << 20
)

var (
	// ErrConditionalUnknown means submission may have taken effect. The caller
	// must authoritatively reread and reconcile the row; blindly retrying can
	// violate a compare-and-set state machine.
	ErrConditionalUnknown = errors.New("accumulo: conditional mutation result is unknown")

	// ErrConditionalRetryExhausted means the bounded, pre-submission routing
	// retries were exhausted.
	ErrConditionalRetryExhausted = errors.New("accumulo: conditional routing retry limit exhausted")
)

// ConditionalStatus is the result of one exact-row conditional mutation.
// ConditionalUnknown is also the zero status returned for errors proven to
// occur before submission. Only errors.Is(err, ErrConditionalUnknown) means
// the mutation may have applied and requires authoritative reconciliation.
type ConditionalStatus uint8

const (
	// ConditionalUnknown is non-authoritative. For a pre-submission error it
	// is merely the zero status. If errors.Is(err, ErrConditionalUnknown), the
	// mutation may have applied and requires authoritative reconciliation.
	ConditionalUnknown ConditionalStatus = iota
	ConditionalAccepted
	ConditionalRejected
)

func (s ConditionalStatus) String() string {
	switch s {
	case ConditionalAccepted:
		return "Accepted"
	case ConditionalRejected:
		return "Rejected"
	default:
		return "Unknown"
	}
}

// Condition compares one exact cell. Conditions are immutable and safe for
// concurrent use.
type Condition struct {
	columnFamily     []byte
	columnQualifier  []byte
	columnVisibility []byte
	value            []byte
	valueSet         bool
	timestamp        int64
	timestampSet     bool
}

// NewAbsentCondition requires that the exact cell be absent.
func NewAbsentCondition(
	columnFamily, columnQualifier, columnVisibility []byte,
) (Condition, error) {
	return newCondition(columnFamily, columnQualifier, columnVisibility, nil, false)
}

// NewValueCondition requires that the exact cell have value. A nil value
// means an exact empty value, not absence.
func NewValueCondition(
	columnFamily, columnQualifier, columnVisibility, value []byte,
) (Condition, error) {
	return newCondition(columnFamily, columnQualifier, columnVisibility, value, true)
}

func newCondition(
	columnFamily, columnQualifier, columnVisibility, value []byte,
	valueSet bool,
) (Condition, error) {
	if len(columnFamily) == 0 {
		return Condition{}, errors.New("accumulo: conditional column family must be non-empty")
	}
	for name, component := range map[string][]byte{
		"column family":     columnFamily,
		"column qualifier":  columnQualifier,
		"column visibility": columnVisibility,
		"value":             value,
	} {
		if len(component) > maxConditionalComponentBytes {
			return Condition{}, fmt.Errorf(
				"accumulo: conditional %s exceeds %d bytes",
				name, maxConditionalComponentBytes,
			)
		}
	}
	if len(columnVisibility) != 0 {
		if _, err := NewColumnVisibility(columnVisibility); err != nil {
			return Condition{}, err
		}
	}
	condition := Condition{
		columnFamily:     cloneRow(columnFamily),
		columnQualifier:  cloneRow(columnQualifier),
		columnVisibility: cloneRow(columnVisibility),
		valueSet:         valueSet,
	}
	if valueSet {
		condition.value = make([]byte, len(value))
		copy(condition.value, value)
	}
	return condition, nil
}

// WithTimestamp returns a copy requiring the exact timestamp in addition to
// the cell's absence or value predicate.
func (c Condition) WithTimestamp(timestamp int64) Condition {
	c = c.clone()
	c.timestamp = timestamp
	c.timestampSet = true
	return c
}

func (c Condition) clone() Condition {
	c.columnFamily = cloneRow(c.columnFamily)
	c.columnQualifier = cloneRow(c.columnQualifier)
	c.columnVisibility = cloneRow(c.columnVisibility)
	if c.valueSet {
		value := make([]byte, len(c.value))
		copy(value, c.value)
		c.value = value
	} else {
		c.value = nil
	}
	return c
}

func (c Condition) toThrift() (*data.TCondition, error) {
	if len(c.columnFamily) == 0 {
		return nil, errors.New("accumulo: conditional column family must be non-empty")
	}
	wire := &data.TCondition{
		Cf:           cloneRow(c.columnFamily),
		Cq:           cloneRow(c.columnQualifier),
		Cv:           cloneRow(c.columnVisibility),
		Ts:           c.timestamp,
		HasTimestamp: c.timestampSet,
	}
	if c.valueSet {
		wire.Val = make([]byte, len(c.value))
		copy(wire.Val, c.value)
	}
	return wire, nil
}

// ConditionalMutation snapshots one non-empty mutation and its conditions.
// It is immutable and safe for concurrent use.
type ConditionalMutation struct {
	row        []byte
	mutation   *data.TMutation
	conditions []Condition
}

// NewConditionalMutation constructs one exact-row compare-and-mutate request.
func NewConditionalMutation(
	mutation *Mutation,
	conditions ...Condition,
) (*ConditionalMutation, error) {
	if mutation == nil {
		return nil, errors.New("accumulo: conditional mutation is nil")
	}
	if mutation.Size() == 0 {
		return nil, errors.New("accumulo: conditional mutation must contain at least one update")
	}
	if len(conditions) == 0 {
		return nil, errors.New("accumulo: conditional mutation requires at least one condition")
	}
	if len(conditions) > maxConditionalConditions {
		return nil, fmt.Errorf(
			"accumulo: conditional mutation exceeds %d conditions",
			maxConditionalConditions,
		)
	}
	wireMutation, err := mutation.toThrift()
	if err != nil {
		return nil, err
	}
	if len(wireMutation.Row) > maxConditionalComponentBytes {
		return nil, fmt.Errorf(
			"accumulo: conditional mutation row exceeds %d bytes",
			maxConditionalComponentBytes,
		)
	}
	encodedSize := mutationWireSize(wireMutation)
	for index, condition := range conditions {
		wire, conditionErr := condition.toThrift()
		if conditionErr != nil {
			return nil, fmt.Errorf("accumulo: condition %d: %w", index, conditionErr)
		}
		encodedSize += conditionWireSize(wire)
		if encodedSize > maxConditionalMutationBytes {
			return nil, fmt.Errorf(
				"accumulo: conditional mutation exceeds %d encoded bytes",
				maxConditionalMutationBytes,
			)
		}
	}
	if encodedSize > maxConditionalMutationBytes {
		return nil, fmt.Errorf(
			"accumulo: conditional mutation exceeds %d encoded bytes",
			maxConditionalMutationBytes,
		)
	}
	snapshot := &ConditionalMutation{
		row:        cloneRow(wireMutation.Row),
		mutation:   cloneConditionalThriftMutation(wireMutation),
		conditions: make([]Condition, len(conditions)),
	}
	for index, condition := range conditions {
		snapshot.conditions[index] = condition.clone()
	}
	return snapshot, nil
}

func conditionWireSize(condition *data.TCondition) int64 {
	if condition == nil {
		return 0
	}
	return int64(len(condition.Cf)+len(condition.Cq)+len(condition.Cv)+len(condition.Val)) + 32
}

// Row returns a defensive copy of the target row.
func (m *ConditionalMutation) Row() []byte {
	if m == nil {
		return nil
	}
	return cloneRow(m.row)
}

// ConditionalWriterOptions configures bounded pre-submission routing retries.
type ConditionalWriterOptions struct {
	// MaxRetries bounds additional attempts after the initial routing or
	// session-start attempt. Zero uses three retries.
	MaxRetries int

	// RetryBackoff is the fixed delay between safe retries. Zero uses 100 ms.
	RetryBackoff time.Duration

	Durability Durability

	// Authorizations are the trusted visibility labels used while evaluating
	// conditional row reads. The labels are validated and copied at
	// construction. An empty set preserves the historical blank-visibility
	// behavior.
	Authorizations [][]byte
}

type normalizedConditionalWriterOptions struct {
	maxRetries     int
	retryBackoff   time.Duration
	durability     ingestclient.Durability
	authorizations [][]byte
}

type conditionalRPC interface {
	ConditionalWriteWithDurability(
		context.Context,
		string,
		string,
		string,
		*data.TKeyExtent,
		*data.TConditionalMutation,
		ingestclient.Durability,
		...[][]byte,
	) (ingestclient.ConditionalOutcome, error)
}

// ConditionalWriter is a stateless table-bound writer. Write is safe for
// concurrent use. It owns no resources; closing the Connector disables it.
type ConditionalWriter struct {
	connector *Connector
	table     Table
	options   normalizedConditionalWriterOptions
	nextID    atomic.Int64

	resolve    func(context.Context) (Table, error)
	locate     func(context.Context, Table, []byte) (Tablet, error)
	invalidate func(Table, []byte) error
	write      func(
		context.Context,
		string,
		string,
		string,
		*data.TKeyExtent,
		*data.TConditionalMutation,
		ingestclient.Durability,
		[][]byte,
	) (ingestclient.ConditionalOutcome, error)
}

// NewConditionalWriter constructs a stateless writer for table.
func (c *Connector) NewConditionalWriter(
	table Table,
	options ConditionalWriterOptions,
) (*ConditionalWriter, error) {
	if c == nil {
		return nil, errors.New("accumulo: connector is nil")
	}
	if table.ID == "" && table.Name == "" {
		return nil, fmt.Errorf("%w: conditional writer table identity is empty", ErrTableNotFound)
	}
	normalized, err := normalizeConditionalWriterOptions(options)
	if err != nil {
		return nil, err
	}
	if _, err := c.discoveryState(); err != nil {
		return nil, err
	}
	rpc, ok := c.ingest.(conditionalRPC)
	if !ok {
		return nil, ErrUnsupportedOperation
	}
	writer := &ConditionalWriter{
		connector: c,
		table:     Table{Name: table.Name, ID: table.ID},
		options:   normalized,
	}
	writer.resolve = writer.resolveTable
	writer.locate = c.LocateTablet
	writer.invalidate = c.InvalidateTablet
	writer.write = func(
		ctx context.Context,
		address, serverLock, tableID string,
		extent *data.TKeyExtent,
		mutation *data.TConditionalMutation,
		durability ingestclient.Durability,
		authorizations [][]byte,
	) (ingestclient.ConditionalOutcome, error) {
		return rpc.ConditionalWriteWithDurability(
			ctx, address, serverLock, tableID, extent, mutation, durability, authorizations,
		)
	}
	return writer, nil
}

// Write applies mutation once its conditions are evaluated. Accepted and
// Rejected remain authoritative even when err reports session cleanup failure.
// If status is Unknown and errors.Is(err, ErrConditionalUnknown), the mutation
// may have taken effect: authoritatively reread and reconcile the row, and
// never blindly retry it. Other Unknown errors occurred before submission.
func (w *ConditionalWriter) Write(
	ctx context.Context,
	mutation *ConditionalMutation,
) (ConditionalStatus, error) {
	if err := ctx.Err(); err != nil {
		return ConditionalUnknown, err
	}
	if w == nil || w.connector == nil {
		return ConditionalUnknown, errors.New("accumulo: conditional writer is nil")
	}
	if mutation == nil || mutation.mutation == nil || len(mutation.row) == 0 {
		return ConditionalUnknown, errors.New("accumulo: conditional mutation is nil")
	}
	if _, err := w.connector.discoveryState(); err != nil {
		return ConditionalUnknown, err
	}
	table, err := w.resolve(ctx)
	if err != nil {
		return ConditionalUnknown, err
	}
	wire, err := mutation.toThrift(w.nextID.Add(1))
	if err != nil {
		return ConditionalUnknown, err
	}

	var lastErr error
	for attempt := 0; attempt <= w.options.maxRetries; attempt++ {
		tablet, locateErr := w.locate(ctx, table, mutation.row)
		if locateErr != nil {
			if !isRetryableWriterRoutingError(locateErr) {
				return ConditionalUnknown, locateErr
			}
			lastErr = locateErr
		} else if validateErr := validateConditionalTablet(table, mutation.row, tablet); validateErr != nil {
			lastErr = validateErr
		} else {
			outcome, writeErr := w.write(
				ctx,
				tablet.Server.HostPort,
				tablet.Server.ServerLock,
				table.ID,
				tabletExtentToThrift(tablet),
				wire,
				w.options.durability,
				cloneByteSlices(w.options.authorizations),
			)
			switch outcome.Status {
			case ingestclient.ConditionalAccepted:
				return ConditionalAccepted, writeErr
			case ingestclient.ConditionalRejected:
				return ConditionalRejected, writeErr
			}
			if outcome.Submitted {
				return ConditionalUnknown, errors.Join(ErrConditionalUnknown, writeErr)
			}
			lastErr = writeErr
		}

		if attempt == w.options.maxRetries {
			return ConditionalUnknown, errors.Join(ErrConditionalRetryExhausted, lastErr)
		}
		if invalidateErr := w.invalidate(table, mutation.row); invalidateErr != nil {
			return ConditionalUnknown, errors.Join(lastErr, invalidateErr)
		}
		if err := waitForWriterRetry(ctx, w.options.retryBackoff); err != nil {
			return ConditionalUnknown, err
		}
	}
	panic("unreachable")
}

func (m *ConditionalMutation) toThrift(id int64) (*data.TConditionalMutation, error) {
	if m == nil || m.mutation == nil || len(m.row) == 0 {
		return nil, errors.New("accumulo: conditional mutation is nil")
	}
	conditions := make([]*data.TCondition, len(m.conditions))
	for index, condition := range m.conditions {
		wire, err := condition.toThrift()
		if err != nil {
			return nil, fmt.Errorf("accumulo: condition %d: %w", index, err)
		}
		conditions[index] = wire
	}
	return &data.TConditionalMutation{
		Conditions: conditions,
		Mutation:   cloneConditionalThriftMutation(m.mutation),
		ID:         id,
	}, nil
}

func (w *ConditionalWriter) resolveTable(ctx context.Context) (Table, error) {
	switch {
	case w.table.ID == "":
		return w.connector.TableByName(ctx, w.table.Name)
	case w.table.Name == "":
		return w.connector.TableByID(ctx, w.table.ID)
	default:
		resolved, err := w.connector.TableByName(ctx, w.table.Name)
		if err != nil {
			return Table{}, err
		}
		if resolved.ID != w.table.ID {
			return Table{}, fmt.Errorf(
				"%w: table %q resolved to %q, expected %q",
				ErrTableIdentityChanged, w.table.Name, resolved.ID, w.table.ID,
			)
		}
		return w.table, nil
	}
}

func validateConditionalTablet(table Table, row []byte, tablet Tablet) error {
	if tablet.Extent.TableID == "" || tablet.Extent.TableID != table.ID {
		return fmt.Errorf(
			"%w: located extent table %q, expected %q",
			ErrNoTabletCoversRow, tablet.Extent.TableID, table.ID,
		)
	}
	if tablet.Server == nil || tablet.Server.HostPort == "" || tablet.Server.Session == "" {
		return fmt.Errorf("%w: table=%s row=%q", ErrTabletNotLocated, table.ID, row)
	}
	if tablet.Server.ServerLock == "" {
		return fmt.Errorf(
			"%w: table=%s row=%q has no full tablet-server lock identity",
			ErrUnsupportedOperation, table.ID, row,
		)
	}
	if (tablet.Extent.PrevRow != nil && bytes.Compare(row, tablet.Extent.PrevRow) <= 0) ||
		(tablet.Extent.EndRow != nil && bytes.Compare(row, tablet.Extent.EndRow) > 0) {
		return fmt.Errorf("%w: table=%s row=%q", ErrNoTabletCoversRow, table.ID, row)
	}
	return nil
}

func normalizeConditionalWriterOptions(
	options ConditionalWriterOptions,
) (normalizedConditionalWriterOptions, error) {
	if options.MaxRetries < 0 || options.MaxRetries > maxConditionalWriterRetries {
		return normalizedConditionalWriterOptions{}, fmt.Errorf(
			"accumulo: conditional writer MaxRetries must be between 0 and %d",
			maxConditionalWriterRetries,
		)
	}
	if options.MaxRetries == 0 {
		options.MaxRetries = defaultConditionalWriterMaxRetries
	}
	if options.RetryBackoff < 0 || options.RetryBackoff > maxConditionalWriterRetryBackoff {
		return normalizedConditionalWriterOptions{}, fmt.Errorf(
			"accumulo: conditional writer RetryBackoff must be between 0 and %s",
			maxConditionalWriterRetryBackoff,
		)
	}
	if options.RetryBackoff == 0 {
		options.RetryBackoff = defaultConditionalWriterRetryBackoff
	}
	if options.Durability > DurabilityNone {
		return normalizedConditionalWriterOptions{}, errors.New(
			"accumulo: conditional writer durability is invalid",
		)
	}
	authorizations, err := normalizeConditionalAuthorizations(options.Authorizations)
	if err != nil {
		return normalizedConditionalWriterOptions{}, err
	}
	return normalizedConditionalWriterOptions{
		maxRetries:     options.MaxRetries,
		retryBackoff:   options.RetryBackoff,
		durability:     ingestclient.Durability(options.Durability),
		authorizations: authorizations,
	}, nil
}

const (
	maxConditionalAuthorizations      = 1024
	maxConditionalAuthorizationBytes  = 1024
	maxConditionalAuthorizationsBytes = 1 << 20
)

func normalizeConditionalAuthorizations(authorizations [][]byte) ([][]byte, error) {
	if len(authorizations) > maxConditionalAuthorizations {
		return nil, fmt.Errorf(
			"%w: conditional authorization count exceeds %d",
			ErrInvalidAuthorizations, maxConditionalAuthorizations,
		)
	}
	result := make([][]byte, len(authorizations))
	total := 0
	for i, authorization := range authorizations {
		if len(authorization) == 0 || len(authorization) > maxConditionalAuthorizationBytes {
			return nil, fmt.Errorf(
				"%w: conditional authorization %d has invalid length",
				ErrInvalidAuthorizations, i,
			)
		}
		total += len(authorization)
		if total > maxConditionalAuthorizationsBytes {
			return nil, fmt.Errorf(
				"%w: conditional authorizations exceed %d bytes",
				ErrInvalidAuthorizations, maxConditionalAuthorizationsBytes,
			)
		}
		for _, character := range authorization {
			if !ValidAuthorizationCharacter(character) {
				return nil, fmt.Errorf(
					"%w: conditional authorization %q holds invalid character %q",
					ErrInvalidAuthorizations, authorization, rune(character),
				)
			}
		}
		result[i] = append([]byte(nil), authorization...)
	}
	return result, nil
}

func cloneConditionalThriftMutation(mutation *data.TMutation) *data.TMutation {
	if mutation == nil {
		return nil
	}
	return &data.TMutation{
		Row:     cloneRow(mutation.Row),
		Data:    cloneRow(mutation.Data),
		Values:  cloneByteSlices(mutation.Values),
		Entries: mutation.Entries,
	}
}
