// Package enginebackend adapts a *engine.Engine to the shoalql.Backend seam,
// so the SQL layer can run identically over RFile, Parquet, or mixed tables.
// It is deliberately a
// separate package: the core shoalql parser/planner/executor never import the
// engine, which keeps them unit-testable with in-memory fakes.
package enginebackend

import (
	"context"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/shoalql"
	"github.com/phrocker/shoal-oss/internal/vectorindex"
)

// Backend wraps an *engine.Engine as a shoalql.Backend.
type Backend struct {
	eng    *engine.Engine
	vector shoalql.VectorSearcher
}

// New builds a Backend over eng.
func New(eng *engine.Engine) *Backend { return &Backend{eng: eng} }

// NewWithVector builds a backend with the distributed semantic-index seam.
func NewWithVector(eng *engine.Engine, vector shoalql.VectorSearcher) *Backend {
	return &Backend{eng: eng, vector: vector}
}

var _ shoalql.Backend = (*Backend)(nil)
var _ shoalql.CapabilityProvider = (*Backend)(nil)
var _ shoalql.NeighborRequestBackend = (*Backend)(nil)
var _ shoalql.ApproximateVectorBackend = (*Backend)(nil)
var _ shoalql.VectorExplainBackend = (*Backend)(nil)

// BackendInfo declares the embedded engine's stable ShoalQL execution
// capabilities. Distributed scan and top-k merge are intentionally absent.
func (b *Backend) BackendInfo() shoalql.BackendInfo {
	info := shoalql.BackendInfo{
		Name: "embedded-engine",
		Mode: "local",
		Capabilities: []shoalql.Capability{
			shoalql.CapabilityRangeScan,
			shoalql.CapabilityColumnFamilyFilter,
			shoalql.CapabilityAsOfPushdown,
			shoalql.CapabilityAggregatePushdown,
			shoalql.CapabilityExactVectorKNN,
			shoalql.CapabilityRowLookup,
			shoalql.CapabilityGraphNeighbors,
			shoalql.CapabilityDocumentIndex,
		},
		StorageFormats:        []string{"rfile"},
		SelectedStorageFormat: "rfile",
	}
	if b.vector != nil {
		info.Capabilities = append(info.Capabilities,
			shoalql.CapabilityApproximateVector, shoalql.CapabilityDistributedTopK)
		info.Pushdowns = append(info.Pushdowns,
			"IVF cluster routing and sharded partial top-k use the persisted vector generation")
		info.OrderingAssumptions = append(info.OrderingAssumptions,
			"partial top-k merges score-descending with document-id ascending tie-break")
	}
	return info
}

func (b *Backend) SearchVector(ctx context.Context, request shoalql.VectorSearchRequest) ([]shoalql.VectorHit, vectorindex.Evidence, error) {
	if b.vector == nil {
		return nil, vectorindex.Evidence{}, fmt.Errorf("enginebackend: vector searcher is not configured")
	}
	return b.vector.SearchVector(ctx, request)
}

func (b *Backend) DescribeVector(ctx context.Context, index string) (vectorindex.Manifest, error) {
	provider, ok := b.vector.(interface {
		DescribeVector(context.Context, string) (vectorindex.Manifest, error)
	})
	if !ok {
		return vectorindex.Manifest{}, fmt.Errorf("enginebackend: vector index metadata unavailable")
	}
	return provider.DescribeVector(ctx, index)
}

// Scan implements shoalql.Backend. The pushdown stack is hosted above a
// whole-table merge (ScanHosted) so re-seeking iterators such as VectorKNN
// and TermIndex see every cell regardless of tablet boundaries. *engine.Scanner
// already satisfies shoalql.RowStream because iterrt.Key aliases wire.Key.
func (b *Backend) Scan(ctx context.Context, table string, r iterrt.Range, req shoalql.ScanRequest) (shoalql.RowStream, error) {
	opts := engine.ScanOptions{
		ColumnFamilies:          req.ColumnFamilies,
		ColumnFamiliesInclusive: req.CFInclusive,
	}
	return b.eng.ScanHostedContext(ctx, table, r, opts, req.Stack)
}

// LookupRows implements shoalql.Backend, buffering the engine's visitor
// callback into copied cells (KNN hydration sets are small).
func (b *Backend) LookupRows(_ context.Context, table string, rows [][]byte, req shoalql.ScanRequest) ([]shoalql.Cell, error) {
	opts := engine.ScanOptions{
		Stack:                   normalizedLookupStack(req.Stack),
		ColumnFamilies:          req.ColumnFamilies,
		ColumnFamiliesInclusive: req.CFInclusive,
	}
	var out []shoalql.Cell
	err := b.eng.LookupRows(table, rows, opts, func(_ int, key *iterrt.Key, value []byte) {
		out = append(out, shoalql.Cell{
			Key:   key.Clone(),
			Value: append([]byte(nil), value...),
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Neighbors implements shoalql.Backend.
func (b *Backend) Neighbors(_ context.Context, table string, rows [][]byte, edgeCF []byte) ([][]shoalql.Neighbor, error) {
	raw, err := b.eng.Neighbors(table, rows, edgeCF, engine.ScanOptions{})
	if err != nil {
		return nil, err
	}

	out := make([][]shoalql.Neighbor, len(raw))
	for i, ns := range raw {
		if len(ns) == 0 {
			continue
		}
		conv := make([]shoalql.Neighbor, len(ns))
		for j, n := range ns {
			conv[j] = shoalql.Neighbor{Target: n.Target, Value: n.Value}
		}
		out[i] = conv
	}
	return out, nil
}

// NeighborsWithRequest preserves AS OF and other pre-projection scan
// semantics. The ordinary adjacency fast path remains in use when no stack is
// needed.
func (b *Backend) NeighborsWithRequest(
	ctx context.Context,
	table string,
	rows [][]byte,
	edgeCF []byte,
	req shoalql.ScanRequest,
) ([][]shoalql.Neighbor, error) {
	if len(req.Stack) == 0 {
		return b.Neighbors(ctx, table, rows, edgeCF)
	}
	out := make([][]shoalql.Neighbor, len(rows))
	for i, row := range rows {
		r := iterrt.Range{
			Start:          &iterrt.Key{Row: append([]byte(nil), row...)},
			StartInclusive: true,
			End:            &iterrt.Key{Row: append(append([]byte(nil), row...), 0)},
			EndInclusive:   false,
		}
		stream, err := b.Scan(ctx, table, r, shoalql.ScanRequest{
			Stack:          req.Stack,
			ColumnFamilies: [][]byte{edgeCF},
			CFInclusive:    true,
		})
		if err != nil {
			return nil, err
		}
		for stream.Next() {
			key := stream.Key()
			out[i] = append(out[i], shoalql.Neighbor{
				Target: append([]byte(nil), key.ColumnQualifier...),
				Value:  append([]byte(nil), stream.Value()...),
			})
			if err := stream.Advance(); err != nil {
				stream.Close()
				return nil, err
			}
		}
		stream.Close()
	}
	return out, nil
}

func normalizedLookupStack(query []iterrt.IterSpec) []iterrt.IterSpec {
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
