package accumulo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/phrocker/shoal-oss/internal/scanclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

const defaultScannerBatchSize int32 = 1024
const scannerCloseTimeout = 5 * time.Second

// ErrRangeSpansTablets indicates that a range exceeds the initial
// single-tablet scanner scope.
var ErrRangeSpansTablets = errors.New("accumulo: scan range spans tablets")

// Key is a Go-native Accumulo key.
type Key struct {
	Row              []byte
	ColumnFamily     []byte
	ColumnQualifier  []byte
	ColumnVisibility []byte
	Timestamp        int64
	// Deleted marks a deletion entry. Scans never surface deleted keys, so it
	// is false for scan results; it is part of the key's value semantics and
	// participates in Compare, where deletion markers sort first.
	Deleted bool
}

// KeyValue is one scanned Accumulo entry.
type KeyValue struct {
	Key   Key
	Value []byte
}

// Column selects a column family, optionally restricted to one qualifier and
// one visibility.
type Column struct {
	family     []byte
	qualifier  []byte
	visibility []byte
}

// NewColumnFamily selects every qualifier in family.
func NewColumnFamily(family []byte) Column {
	return Column{family: cloneRow(family)}
}

// NewColumn selects one family and qualifier pair.
func NewColumn(family, qualifier []byte) Column {
	return Column{
		family:    cloneRow(family),
		qualifier: cloneRow(qualifier),
	}
}

// Family returns a defensive copy of the selected column family.
func (c Column) Family() []byte { return cloneRow(c.family) }

// Qualifier returns a defensive copy of the selected qualifier. A nil
// qualifier means every qualifier in the family.
func (c Column) Qualifier() []byte { return cloneRow(c.qualifier) }

// IteratorSetting configures one server-side iterator for a scan.
type IteratorSetting struct {
	name      string
	className string
	priority  int32
	options   map[string]string
}

// NewIteratorSetting constructs a server-side scan iterator setting.
func NewIteratorSetting(
	name, className string,
	priority int32,
	options map[string]string,
) (IteratorSetting, error) {
	setting := IteratorSetting{
		name:      name,
		className: className,
		priority:  priority,
		options:   cloneStringMap(options),
	}
	if err := validateIteratorSetting(setting); err != nil {
		return IteratorSetting{}, err
	}
	return setting, nil
}

// Name returns the iterator's scan-local name.
func (s IteratorSetting) Name() string { return s.name }

// ClassName returns the iterator implementation class.
func (s IteratorSetting) ClassName() string { return s.className }

// Priority returns the positive iterator execution priority.
func (s IteratorSetting) Priority() int32 { return s.priority }

// Options returns a defensive copy of the iterator options.
func (s IteratorSetting) Options() map[string]string { return cloneStringMap(s.options) }

// ScannerOptions configures a Scanner.
type ScannerOptions struct {
	BatchSize      int32
	Authorizations [][]byte
	Columns        []Column
	Iterators      []IteratorSetting
	// Parallelism bounds concurrent tablet scans performed by BatchScanner.
	// Zero uses one worker. Scanner ignores this field.
	Parallelism int
	// UseMultiScan groups tablet ranges by server into Accumulo multi-scan
	// RPCs. Multi-scan result order is server-defined.
	UseMultiScan bool
}

// CleanupError reports that scan results are usable but the server-side scan
// session could not be closed.
type CleanupError struct {
	ScanID int64
	Err    error
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("accumulo: close scan %d: %v", e.ScanID, e.Err)
}

// Unwrap returns the close failure.
func (e *CleanupError) Unwrap() error { return e.Err }

// Scanner executes scans for one table through its Connector.
type Scanner struct {
	connector *Connector
	table     Table
	options   ScannerOptions
}

// NewScanner constructs a scanner for table.
func (c *Connector) NewScanner(table Table, options ScannerOptions) (*Scanner, error) {
	if table.ID == "" && table.Name == "" {
		return nil, fmt.Errorf("%w: scanner table identity is empty", ErrTableNotFound)
	}
	if options.BatchSize < 0 {
		return nil, errors.New("accumulo: scanner batch size must not be negative")
	}
	if options.Parallelism < 0 {
		return nil, errors.New("accumulo: scanner parallelism must not be negative")
	}
	if _, err := c.discoveryState(); err != nil {
		return nil, err
	}
	options.Authorizations = cloneByteSlices(options.Authorizations)
	options.Columns = cloneColumns(options.Columns)
	iterators, err := cloneIteratorSettings(options.Iterators)
	if err != nil {
		return nil, err
	}
	options.Iterators = iterators
	return &Scanner{
		connector: c,
		table: Table{
			Name: table.Name,
			ID:   table.ID,
		},
		options: options,
	}, nil
}

// Scan reads a range that must fit within one tablet. If the tablet server
// rejects a stale assignment, Scan invalidates discovery and retries once.
func (s *Scanner) Scan(ctx context.Context, scanRange *Range) ([]KeyValue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scanRange == nil {
		return nil, errors.New("accumulo: scan range is nil")
	}
	table, err := s.resolveTable(ctx)
	if err != nil {
		return nil, err
	}
	routingRow := scanRange.routingRow()
	tablet, err := s.locateTablet(ctx, table, routingRow)
	if err != nil {
		return nil, err
	}
	return s.scanLocated(ctx, table, routingRow, tablet, scanRange)
}

func (s *Scanner) scanLocated(
	ctx context.Context,
	table Table,
	routingRow []byte,
	tablet Tablet,
	scanRange *Range,
) ([]KeyValue, error) {
	var priorCleanup error
	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(priorCleanup, err)
		}
		if !scanRange.fitsTablet(tablet) {
			return nil, fmt.Errorf(
				"%w: table=%s start=%q end=%q tabletEnd=%q",
				ErrRangeSpansTablets,
				table.ID,
				scanRange.StartRow(),
				scanRange.EndRow(),
				tablet.Extent.EndRow,
			)
		}

		values, scanErr := s.scanTablet(ctx, tablet, scanRange)
		if scanErr == nil || !isStaleScanError(scanErr) {
			return values, errors.Join(priorCleanup, scanErr)
		}
		var cleanupErr *CleanupError
		errors.As(scanErr, &cleanupErr)
		if invalidateErr := s.connector.InvalidateTablet(table, routingRow); invalidateErr != nil {
			return values, errors.Join(priorCleanup, scanErr, invalidateErr)
		}
		if cleanupErr != nil {
			priorCleanup = errors.Join(priorCleanup, cleanupErr)
		}
		tablet, scanErr = s.locateTablet(ctx, table, routingRow)
		if scanErr != nil {
			return nil, errors.Join(priorCleanup, scanErr)
		}
	}
}

func (s *Scanner) locateTablet(ctx context.Context, table Table, routingRow []byte) (Tablet, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Tablet{}, err
		}
		tablet, err := s.connector.LocateTablet(ctx, table, routingRow)
		if err == nil {
			return tablet, nil
		}
		if !isStaleScanError(err) {
			return Tablet{}, err
		}
		if invalidateErr := s.connector.InvalidateTablet(table, routingRow); invalidateErr != nil {
			return Tablet{}, errors.Join(err, invalidateErr)
		}
	}
}

func (s *Scanner) resolveTable(ctx context.Context) (Table, error) {
	switch {
	case s.table.ID == "":
		return s.connector.TableByName(ctx, s.table.Name)
	case s.table.Name == "":
		return s.connector.TableByID(ctx, s.table.ID)
	default:
		return s.table, nil
	}
}

func (s *Scanner) scanTablet(ctx context.Context, tablet Tablet, scanRange *Range) ([]KeyValue, error) {
	if tablet.Server == nil || tablet.Server.HostPort == "" {
		return nil, fmt.Errorf("%w: table=%s", ErrTabletNotLocated, tablet.Extent.TableID)
	}
	batchSize := s.options.BatchSize
	if batchSize == 0 {
		batchSize = defaultScannerBatchSize
	}
	iterators, iteratorOptions := iteratorsToThrift(s.options.Iterators)
	initial, err := s.connector.scan.Start(ctx, tablet.Server.HostPort, scanclient.StartRequest{
		Extent:             tabletExtentToThrift(tablet),
		Range:              scanRange.toThrift(),
		Columns:            columnsToThrift(s.options.Columns),
		BatchSize:          batchSize,
		Iterators:          iterators,
		IteratorOptions:    iteratorOptions,
		Authorizations:     cloneByteSlices(s.options.Authorizations),
		ReadaheadThreshold: int64(batchSize),
	})
	if initial == nil && err != nil {
		return nil, err
	}
	if initial == nil {
		return nil, errors.New("accumulo: start scan returned no result")
	}

	values, appendErr := appendScanResult(nil, initial.Result_)
	resultErr := errors.Join(err, appendErr)
	more := initial.Result_ != nil && initial.Result_.More
	if appendErr != nil {
		more = false
	}
	if more && initial.ScanID == 0 {
		resultErr = errors.Join(resultErr, errors.New("accumulo: continuing scan has zero scan ID"))
		more = false
	}
	for more {
		result, continueErr := s.connector.scan.Continue(ctx, tablet.Server.HostPort, initial.ScanID, 0)
		if continueErr != nil {
			resultErr = errors.Join(resultErr, continueErr)
			break
		}
		var appendErr error
		values, appendErr = appendScanResult(values, result)
		resultErr = errors.Join(resultErr, appendErr)
		if appendErr != nil {
			break
		}
		more = result != nil && result.More
	}

	var cleanupErr error
	if initial.ScanID != 0 {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scannerCloseTimeout)
		defer cancel()
		if closeErr := s.connector.scan.CloseScan(closeCtx, tablet.Server.HostPort, initial.ScanID); closeErr != nil {
			cleanupErr = &CleanupError{ScanID: int64(initial.ScanID), Err: closeErr}
		}
	}
	return values, errors.Join(resultErr, cleanupErr)
}

func appendScanResult(values []KeyValue, result *data.ScanResult_) ([]KeyValue, error) {
	if result == nil {
		return values, nil
	}
	return appendKeyValues(values, result.Results)
}

func appendKeyValues(values []KeyValue, entries []*data.TKeyValue) ([]KeyValue, error) {
	for _, entry := range entries {
		if entry == nil || entry.Key == nil {
			return values, errors.New("accumulo: scan result contains a nil key")
		}
		values = append(values, KeyValue{
			Key: Key{
				Row:              cloneRow(entry.Key.Row),
				ColumnFamily:     cloneRow(entry.Key.ColFamily),
				ColumnQualifier:  cloneRow(entry.Key.ColQualifier),
				ColumnVisibility: cloneRow(entry.Key.ColVisibility),
				Timestamp:        entry.Key.Timestamp,
			},
			Value: cloneRow(entry.Value),
		})
	}
	return values, nil
}

func tabletExtentToThrift(tablet Tablet) *data.TKeyExtent {
	return &data.TKeyExtent{
		Table:      []byte(tablet.Extent.TableID),
		EndRow:     cloneRow(tablet.Extent.EndRow),
		PrevEndRow: cloneRow(tablet.Extent.PrevRow),
	}
}

func isStaleScanError(err error) bool {
	if errors.Is(err, ErrTabletNotLocated) {
		return true
	}
	var notServing *tabletserver.NotServingTabletException
	return errors.As(err, &notServing)
}

func cloneByteSlices(values [][]byte) [][]byte {
	if values == nil {
		return nil
	}
	cloned := make([][]byte, len(values))
	for i := range values {
		cloned[i] = cloneRow(values[i])
	}
	return cloned
}

func cloneColumns(columns []Column) []Column {
	if columns == nil {
		return nil
	}
	cloned := make([]Column, len(columns))
	for i, column := range columns {
		cloned[i] = NewColumnWithVisibility(column.family, column.qualifier, column.visibility)
	}
	return cloned
}

func columnsToThrift(columns []Column) []*data.TColumn {
	if len(columns) == 0 {
		return nil
	}
	wire := make([]*data.TColumn, len(columns))
	for i, column := range columns {
		wire[i] = &data.TColumn{
			ColumnFamily:     cloneRow(column.family),
			ColumnQualifier:  cloneRow(column.qualifier),
			ColumnVisibility: cloneRow(column.visibility),
		}
	}
	return wire
}

func validateIteratorSetting(setting IteratorSetting) error {
	switch {
	case setting.name == "":
		return errors.New("accumulo: iterator name is empty")
	case setting.className == "":
		return errors.New("accumulo: iterator class name is empty")
	case setting.priority <= 0:
		return errors.New("accumulo: iterator priority must be positive")
	default:
		return nil
	}
}

func cloneIteratorSettings(settings []IteratorSetting) ([]IteratorSetting, error) {
	if settings == nil {
		return nil, nil
	}
	cloned := make([]IteratorSetting, len(settings))
	names := make(map[string]struct{}, len(settings))
	priorities := make(map[int32]struct{}, len(settings))
	for i, setting := range settings {
		if err := validateIteratorSetting(setting); err != nil {
			return nil, err
		}
		if _, exists := names[setting.name]; exists {
			return nil, fmt.Errorf("accumulo: duplicate iterator name %q", setting.name)
		}
		if _, exists := priorities[setting.priority]; exists {
			return nil, fmt.Errorf("accumulo: duplicate iterator priority %d", setting.priority)
		}
		names[setting.name] = struct{}{}
		priorities[setting.priority] = struct{}{}
		cloned[i] = IteratorSetting{
			name:      setting.name,
			className: setting.className,
			priority:  setting.priority,
			options:   cloneStringMap(setting.options),
		}
	}
	return cloned, nil
}

func iteratorsToThrift(
	settings []IteratorSetting,
) ([]*data.IterInfo, map[string]map[string]string) {
	if len(settings) == 0 {
		return nil, nil
	}
	iterators := make([]*data.IterInfo, len(settings))
	var options map[string]map[string]string
	for i, setting := range settings {
		iterators[i] = &data.IterInfo{
			Priority:  setting.priority,
			ClassName: setting.className,
			IterName:  setting.name,
		}
		if len(setting.options) != 0 {
			if options == nil {
				options = make(map[string]map[string]string)
			}
			options[setting.name] = cloneStringMap(setting.options)
		}
	}
	return iterators, options
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
