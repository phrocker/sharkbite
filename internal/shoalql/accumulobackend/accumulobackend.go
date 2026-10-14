// Package accumulobackend adapts the public Accumulo client APIs to ShoalQL.
package accumulobackend

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/shoalql"
	"github.com/phrocker/shoal-oss/internal/vectorindex"
)

// ErrHistoricalVersionsUnavailable prevents a standard version-pruned
// Accumulo scan from silently changing AS OF semantics.
var ErrHistoricalVersionsUnavailable = errors.New(
	"shoalql accumulo: AS OF requires scanner access to historical versions",
)

// Client is the narrow replay seam used by the backend. ConnectorClient is the
// production implementation and uses only public scanner, batch scanner, and
// batch writer APIs.
type Client interface {
	Scan(context.Context, accumulo.Table, []*accumulo.Range, accumulo.ScannerOptions) ([]accumulo.KeyValue, error)
	Write(context.Context, accumulo.Table, []*accumulo.Mutation, accumulo.BatchWriterOptions) error
}

// ConnectorClient adapts an Accumulo Connector.
type ConnectorClient struct {
	Connector *accumulo.Connector
}

func (c ConnectorClient) Scan(
	ctx context.Context,
	table accumulo.Table,
	ranges []*accumulo.Range,
	options accumulo.ScannerOptions,
) ([]accumulo.KeyValue, error) {
	if c.Connector == nil {
		return nil, errors.New("shoalql accumulo: connector is nil")
	}
	scanner, err := c.Connector.NewBatchScanner(table, options)
	if err != nil {
		return nil, err
	}
	return scanner.Scan(ctx, ranges)
}

func (c ConnectorClient) Write(
	ctx context.Context,
	table accumulo.Table,
	mutations []*accumulo.Mutation,
	options accumulo.BatchWriterOptions,
) error {
	if c.Connector == nil {
		return errors.New("shoalql accumulo: connector is nil")
	}
	writer, err := c.Connector.NewBatchWriter(table, options)
	if err != nil {
		return err
	}
	for _, mutation := range mutations {
		if err := writer.Add(ctx, mutation); err != nil {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return errors.Join(err, writer.Close(closeCtx))
		}
	}
	return writer.Close(ctx)
}

// Options configures distributed scans and their explicit local fallback.
type Options struct {
	Authorizations [][]byte
	BatchSize      int32
	Parallelism    int
	DefaultTimeout time.Duration
	StorageFormats []string
	// HistoricalVersions confirms that scans retain enough versions for
	// local AS OF evaluation. Standard Accumulo scan-time versioning usually
	// does not, so this must be enabled only by a compatible server/replay.
	HistoricalVersions bool
	VectorSearcher     shoalql.VectorSearcher
}

// Backend executes ShoalQL against Accumulo.
type Backend struct {
	client Client
	opts   Options
}

// New constructs an Accumulo-backed ShoalQL backend.
func New(client Client, opts Options) *Backend {
	opts.Authorizations = cloneBytes2(opts.Authorizations)
	opts.StorageFormats = append([]string(nil), opts.StorageFormats...)
	if len(opts.StorageFormats) == 0 {
		opts.StorageFormats = []string{"rfile", "parquet"}
	}
	return &Backend{client: client, opts: opts}
}

var _ shoalql.Backend = (*Backend)(nil)
var _ shoalql.CapabilityProvider = (*Backend)(nil)
var _ shoalql.NeighborRequestBackend = (*Backend)(nil)
var _ shoalql.ApproximateVectorBackend = (*Backend)(nil)
var _ shoalql.VectorExplainBackend = (*Backend)(nil)

// BackendInfo describes both native pushdowns and the deliberate exact
// materialization fallback. Approximate vector support is not declared.
func (b *Backend) BackendInfo() shoalql.BackendInfo {
	capabilities := []shoalql.Capability{
		shoalql.CapabilityRangeScan,
		shoalql.CapabilityColumnFamilyFilter,
		shoalql.CapabilityRowLookup,
		shoalql.CapabilityGraphNeighbors,
		shoalql.CapabilityDocumentIndex,
		shoalql.CapabilityDistributedScan,
	}
	info := shoalql.BackendInfo{
		Name:                  "accumulo-client",
		Mode:                  "distributed",
		Capabilities:          capabilities,
		StorageFormats:        append([]string(nil), b.opts.StorageFormats...),
		SelectedStorageFormat: "tablet-managed",
		Pushdowns: []string{
			"row ranges, column families, authorizations, and scan pagination use public Accumulo client APIs",
		},
		LocalMaterialization: []string{
			"scanner pages are key-sorted and replayed through Shoal iterator stacks when no compatible server iterator is available",
		},
		FallbackReasons: []string{
			"distributed IVF-PQ build, freshness, and routing lifecycle is unavailable; approximate vector search is unsupported",
			"exact vector search is refused until metadata-table per-file embedding states are supplied to planning",
		},
		OrderingAssumptions: []string{
			"tablet results are globally key-sorted before fallback execution",
		},
		FallbackIterators: []string{
			iterrt.IterGraphAggregation,
			iterrt.IterDocumentIndex,
		},
	}
	if b.opts.HistoricalVersions {
		info.FallbackIterators = append(info.FallbackIterators, iterrt.IterAsOf)
	} else {
		info.FallbackReasons = append(info.FallbackReasons,
			"AS OF is rejected unless the scanner is configured to retain historical versions")
	}
	if b.opts.VectorSearcher != nil {
		info.Capabilities = append(info.Capabilities,
			shoalql.CapabilityApproximateVector, shoalql.CapabilityDistributedTopK)
		info.Pushdowns = append(info.Pushdowns,
			"IVF cluster prefixes fan out through the configured persisted vector generation")
		info.OrderingAssumptions = append(info.OrderingAssumptions,
			"tablet partial top-k results merge score-descending with document-id ascending tie-break")
		info.FallbackReasons = removeFallback(info.FallbackReasons,
			"distributed IVF-PQ build, freshness, and routing lifecycle is unavailable; approximate vector search is unsupported")
	}
	return info
}

func (b *Backend) SearchVector(ctx context.Context, request shoalql.VectorSearchRequest) ([]shoalql.VectorHit, vectorindex.Evidence, error) {
	if b.opts.VectorSearcher == nil {
		return nil, vectorindex.Evidence{}, errors.New("shoalql accumulo: vector searcher is not configured")
	}
	if len(request.Authorizations) == 0 {
		request.Authorizations = make(map[string]bool, len(b.opts.Authorizations))
		for _, authorization := range b.opts.Authorizations {
			request.Authorizations[string(authorization)] = true
		}
	}
	return b.opts.VectorSearcher.SearchVector(ctx, request)
}

func (b *Backend) DescribeVector(ctx context.Context, index string) (vectorindex.Manifest, error) {
	provider, ok := b.opts.VectorSearcher.(interface {
		DescribeVector(context.Context, string) (vectorindex.Manifest, error)
	})
	if !ok {
		return vectorindex.Manifest{}, errors.New("shoalql accumulo: vector index metadata unavailable")
	}
	return provider.DescribeVector(ctx, index)
}

func removeFallback(values []string, remove string) []string {
	out := values[:0]
	for _, value := range values {
		if value != remove {
			out = append(out, value)
		}
	}
	return out
}

// Scan pushes the physical range, columns, authorizations, cancellation, and
// RPC page size to Accumulo. The returned replay applies timestamp/delete
// semantics and unsupported Shoal iterators explicitly and deterministically.
func (b *Backend) Scan(
	ctx context.Context,
	table string,
	r iterrt.Range,
	req shoalql.ScanRequest,
) (shoalql.RowStream, error) {
	if hasIterator(req.Stack, iterrt.IterAsOf) && !b.opts.HistoricalVersions {
		return nil, ErrHistoricalVersionsUnavailable
	}
	if hasIterator(req.Stack, iterrt.IterVectorKNN) {
		return nil, fmt.Errorf(
			"%w: Accumulo exact vector scans require the metadata-table file-state snapshot",
			embeddingspace.ErrQueryMetadataMissing,
		)
	}
	ctx, cancel := b.withTimeout(ctx)
	defer cancel()
	scanRange, err := accumuloRange(r)
	if err != nil {
		return nil, err
	}

	values, err := b.scan(ctx, table, []*accumulo.Range{scanRange}, req)
	if err != nil {
		return nil, err
	}
	return replay(values, r, req, b.opts.Authorizations)
}

// LookupRows uses BatchScanner row ranges, preserving the caller's row order
// only at the ShoalQL hydration layer while returning key-sorted cells.
func (b *Backend) LookupRows(
	ctx context.Context,
	table string,
	rows [][]byte,
	req shoalql.ScanRequest,
) ([]shoalql.Cell, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	ctx, cancel := b.withTimeout(ctx)
	defer cancel()
	ranges := make([]*accumulo.Range, 0, len(rows))
	for _, row := range rows {
		r, err := accumulo.NewRangeRow(row)
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, r)
	}
	values, err := b.scan(ctx, table, ranges, req)
	if err != nil {
		return nil, err
	}
	stream, err := replay(values, iterrt.InfiniteRange(), req, b.opts.Authorizations)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var cells []shoalql.Cell
	for stream.Next() {
		cells = append(cells, shoalql.Cell{
			Key:   stream.Key().Clone(),
			Value: append([]byte(nil), stream.Value()...),
		})
		if err := stream.Advance(); err != nil {
			return nil, err
		}
	}
	return cells, nil
}

// Neighbors resolves edge cells with one BatchScanner request.
func (b *Backend) Neighbors(
	ctx context.Context,
	table string,
	rows [][]byte,
	edgeCF []byte,
) ([][]shoalql.Neighbor, error) {
	return b.NeighborsWithRequest(ctx, table, rows, edgeCF, shoalql.ScanRequest{})
}

// NeighborsWithRequest preserves AS OF semantics when historical scanner
// replay is explicitly available.
func (b *Backend) NeighborsWithRequest(
	ctx context.Context,
	table string,
	rows [][]byte,
	edgeCF []byte,
	req shoalql.ScanRequest,
) ([][]shoalql.Neighbor, error) {
	out := make([][]shoalql.Neighbor, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	req.ColumnFamilies = [][]byte{edgeCF}
	req.CFInclusive = true
	cells, err := b.LookupRows(ctx, table, rows, req)
	if err != nil {
		return nil, err
	}
	index := make(map[string][]int, len(rows))
	for i, row := range rows {
		index[string(row)] = append(index[string(row)], i)
	}
	for _, cell := range cells {
		indexes, ok := index[string(cell.Key.Row)]
		if !ok || string(cell.Key.ColumnFamily) != string(edgeCF) {
			continue
		}
		for _, i := range indexes {
			out[i] = append(out[i], shoalql.Neighbor{
				Target: append([]byte(nil), cell.Key.ColumnQualifier...),
				Value:  append([]byte(nil), cell.Value...),
			})
		}
	}
	for i := range out {
		sort.Slice(out[i], func(a, z int) bool {
			return string(out[i][a].Target) < string(out[i][z].Target)
		})
	}
	return out, nil
}

// WriteCells persists promoted RFile/Parquet-originated logical cells through
// the public BatchWriter API. File provenance does not affect cell semantics.
func (b *Backend) WriteCells(
	ctx context.Context,
	table string,
	cells []shoalql.Cell,
	options accumulo.BatchWriterOptions,
) error {
	if b.client == nil {
		return errors.New("shoalql accumulo: client is nil")
	}
	ctx, cancel := b.withTimeout(ctx)
	defer cancel()
	byRow := make(map[string]*accumulo.Mutation)
	var rows []string
	for _, cell := range cells {
		if cell.Key == nil || len(cell.Key.Row) == 0 {
			return errors.New("shoalql accumulo: cell has empty key")
		}
		row := string(cell.Key.Row)
		current := byRow[row]
		if current == nil {
			var err error
			current, err = accumulo.NewMutation(cell.Key.Row)
			if err != nil {
				return err
			}
			byRow[row] = current
			rows = append(rows, row)
		}
		if cell.Key.Deleted {
			current.Delete(cell.Key.ColumnFamily, cell.Key.ColumnQualifier,
				cell.Key.ColumnVisibility, cell.Key.Timestamp)
		} else {
			current.Put(cell.Key.ColumnFamily, cell.Key.ColumnQualifier,
				cell.Key.ColumnVisibility, cell.Key.Timestamp, cell.Value)
		}
	}
	sort.Strings(rows)
	mutations := make([]*accumulo.Mutation, 0, len(rows))
	for _, row := range rows {
		mutations = append(mutations, byRow[row])
	}
	return b.client.Write(ctx, accumulo.Table{Name: table}, mutations, options)
}

func (b *Backend) scan(
	ctx context.Context,
	table string,
	ranges []*accumulo.Range,
	req shoalql.ScanRequest,
) ([]accumulo.KeyValue, error) {
	if b.client == nil {
		return nil, errors.New("shoalql accumulo: client is nil")
	}
	columns := make([]accumulo.Column, len(req.ColumnFamilies))
	if len(req.ColumnFamilies) > 0 && !req.CFInclusive {
		columns = nil
	} else {
		for i, cf := range req.ColumnFamilies {
			columns[i] = accumulo.NewColumnFamily(cf)
		}
	}
	values, err := b.client.Scan(ctx, accumulo.Table{Name: table}, ranges, accumulo.ScannerOptions{
		BatchSize:      b.opts.BatchSize,
		Authorizations: cloneBytes2(b.opts.Authorizations),
		Columns:        columns,
		Parallelism:    b.opts.Parallelism,
		UseMultiScan:   false,
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(values, func(i, j int) bool {
		return values[i].Key.Compare(values[j].Key) < 0
	})
	return values, nil
}

func (b *Backend) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if b.opts.DefaultTimeout <= 0 {
		return ctx, func() {}
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, b.opts.DefaultTimeout)
}

func accumuloRange(r iterrt.Range) (*accumulo.Range, error) {
	var start, end *accumulo.Key
	if !r.InfiniteStart && r.Start != nil {
		k := toAccumuloKey(r.Start)
		start = &k
	}
	if !r.InfiniteEnd && r.End != nil {
		k := toAccumuloKey(r.End)
		end = &k
	}
	out, err := accumulo.NewKeyRange(start, r.StartInclusive, end, r.EndInclusive)
	if err != nil {
		return nil, fmt.Errorf("shoalql accumulo: range: %w", err)
	}
	return out, nil
}

func toAccumuloKey(k *iterrt.Key) accumulo.Key {
	out := accumulo.NewKeyWithColumns(k.Row, k.ColumnFamily, k.ColumnQualifier,
		k.ColumnVisibility, k.Timestamp)
	out.Deleted = k.Deleted
	return out
}

func replay(
	values []accumulo.KeyValue,
	r iterrt.Range,
	req shoalql.ScanRequest,
	auths [][]byte,
) (*iteratorStream, error) {
	cells := make([]iterrt.Cell, len(values))
	for i, value := range values {
		k := value.Key
		cells[i] = iterrt.Cell{
			Key: &iterrt.Key{
				Row:              append([]byte(nil), k.Row...),
				ColumnFamily:     append([]byte(nil), k.ColumnFamily...),
				ColumnQualifier:  append([]byte(nil), k.ColumnQualifier...),
				ColumnVisibility: append([]byte(nil), k.ColumnVisibility...),
				Timestamp:        k.Timestamp,
				Deleted:          k.Deleted,
			},
			Value: append([]byte(nil), value.Value...),
		}
	}
	specs := normalizedScanStack(req.Stack)
	top, err := iterrt.BuildStack(iterrt.NewSliceSource(cells), specs, iterrt.IteratorEnvironment{
		Scope:          iterrt.ScopeScan,
		Authorizations: scanAuthorizations(auths),
	})
	if err != nil {
		return nil, err
	}
	if err := top.Seek(r, req.ColumnFamilies, req.CFInclusive); err != nil {
		return nil, err
	}
	return &iteratorStream{top: top}, nil
}

func scanAuthorizations(auths [][]byte) [][]byte {
	if auths == nil {
		return [][]byte{}
	}
	return cloneBytes2(auths)
}

func normalizedScanStack(query []iterrt.IterSpec) []iterrt.IterSpec {
	var before, after []iterrt.IterSpec
	for _, spec := range query {
		if spec.Name == iterrt.IterAsOf {
			before = append(before, spec)
		} else {
			after = append(after, spec)
		}
	}
	out := append([]iterrt.IterSpec(nil), before...)
	out = append(out,
		iterrt.IterSpec{Name: iterrt.IterDeleting, Options: map[string]string{
			iterrt.DeletingOptionPropagate: "false",
		}},
		iterrt.IterSpec{Name: iterrt.IterVisibility},
		iterrt.IterSpec{Name: iterrt.IterVersioning, Options: map[string]string{
			iterrt.VersioningOption: "1",
		}},
	)
	return append(out, after...)
}

func hasIterator(specs []iterrt.IterSpec, name string) bool {
	for _, spec := range specs {
		if spec.Name == name {
			return true
		}
	}
	return false
}

type iteratorStream struct {
	top    iterrt.SortedKeyValueIterator
	closed bool
}

func (s *iteratorStream) Next() bool {
	return !s.closed && s.top != nil && s.top.HasTop()
}

func (s *iteratorStream) Key() *iterrt.Key {
	if !s.Next() {
		return nil
	}
	return s.top.GetTopKey()
}

func (s *iteratorStream) Value() []byte {
	if !s.Next() {
		return nil
	}
	return s.top.GetTopValue()
}

func (s *iteratorStream) Advance() error {
	if !s.Next() {
		return nil
	}
	return s.top.Next()
}

func (s *iteratorStream) Close() { s.closed = true }

func cloneBytes2(values [][]byte) [][]byte {
	if values == nil {
		return nil
	}
	out := make([][]byte, len(values))
	for i := range values {
		out[i] = append([]byte(nil), values[i]...)
	}
	return out
}
