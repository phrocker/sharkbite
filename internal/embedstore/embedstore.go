// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package embedstore adapts a *engine.Engine to the embedpb ShoalEmbed data
// shape (CreateTable / Write / Scan / Flush / Compact). It is the single
// source of truth for translating embedpb.ScanRequest pushdowns — term-index,
// vector k-NN, edge-expand and score-filter — into engine iterator stacks.
//
// The same translation backs two callers, keeping local and platform behavior
// byte-identical:
//   - the shoal-embed gRPC server (cmd/shoal-embed), which streams cells; and
//   - in-process policy callers such as the agentmem layer, which consume the
//     buffered Scan that returns []*embedpb.Cell.
//
// EngineStore's method set deliberately matches the agentmem EmbedStore
// interface so a *EngineStore can be handed straight to agentmem.New, letting
// the agentic loop run directly against a local engine with no gRPC hop.
package embedstore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
)

// DefaultScanBatchSize is the cell batch size used by streaming callers when a
// request does not specify one.
const DefaultScanBatchSize = 256

// Validation errors returned by Scanner/Scan for malformed requests. Callers
// that surface typed errors (e.g. the gRPC server) match on these to map them
// onto their own error codes.
var (
	// ErrMultiplePushdowns is returned when more than one of term_filter,
	// vector_search, edge_expand and score_filter is set on a request.
	ErrMultiplePushdowns = errors.New("at most one of term_filter, vector_search, edge_expand and score_filter may be set")
	// ErrVectorQueryRequired is returned when vector_search is set without a query.
	ErrVectorQueryRequired = errors.New("vector_search.query is required")
	// ErrVectorEmbeddingSpaceRequired is returned when a raw query vector has
	// no stable embedding-space identity.
	ErrVectorEmbeddingSpaceRequired = fmt.Errorf(
		"vector_search.embedding_space is required: %w",
		embeddingspace.ErrQueryIdentityRequired,
	)
	// ErrNegativeMaxHops is returned when edge_expand.max_hops is negative.
	ErrNegativeMaxHops = errors.New("edge_expand.max_hops must be non-negative")
	// ErrInvalidCondition is returned when a mutation condition has no predicate.
	ErrInvalidCondition = errors.New("mutation condition requires absent or value_equals")
	// ErrConditionalRejected is returned by Write when any conditional mutation
	// is rejected. Call WriteWithResults to inspect per-mutation outcomes.
	ErrConditionalRejected = errors.New("one or more conditional mutations were rejected")
)

// EngineStore is an engine-backed implementation of the embedpb data plane.
type EngineStore struct {
	eng *engine.Engine
	ts  atomic.Int64
}

// New wraps eng in an EngineStore.
func New(eng *engine.Engine) *EngineStore { return &EngineStore{eng: eng} }

// Engine returns the underlying engine for callers that need lifecycle or
// admin operations not exposed on EngineStore.
func (s *EngineStore) Engine() *engine.Engine { return s.eng }

// nextTimestamp produces a monotonically increasing write timestamp. It uses
// Unix microseconds as the base, advancing an atomic counter to guarantee
// uniqueness even within a sub-microsecond batch.
func (s *EngineStore) nextTimestamp() int64 {
	now := time.Now().UnixMicro()
	for {
		prev := s.ts.Load()
		next := now
		if next <= prev {
			next = prev + 1
		}
		if s.ts.CompareAndSwap(prev, next) {
			return next
		}
	}
}

// CreateTable creates table, optionally split on the given row prefixes.
func (s *EngineStore) CreateTable(ctx context.Context, table string, splits []string) error {
	return s.CreateTableWithEmbedding(ctx, table, splits, "")
}

// CreateTableWithEmbedding creates a table whose future immutable files carry
// defaultEmbedding's explicit state.
func (s *EngineStore) CreateTableWithEmbedding(
	_ context.Context,
	table string,
	splits []string,
	defaultEmbedding string,
) error {
	if table == "" {
		return errors.New("embedstore: table is required")
	}
	opts := engine.TableOptions{}
	if len(splits) > 0 {
		opts.Splits = engine.PrefixSplit(splits...)
	}
	if defaultEmbedding != "" {
		state, err := embeddingspace.Parse(defaultEmbedding)
		if err != nil {
			return err
		}
		opts.DefaultEmbedding = state
	}
	return s.eng.CreateTable(table, opts)
}

// Write applies mutations to table. Entries with a zero timestamp are stamped
// with a fresh monotonic timestamp.
func (s *EngineStore) Write(ctx context.Context, table string, muts []*embedpb.Mutation) error {
	results, err := s.WriteWithResults(ctx, table, muts)
	if err != nil {
		return err
	}
	for _, result := range results {
		if result.Status == embedpb.MutationStatus_MUTATION_STATUS_REJECTED {
			return ErrConditionalRejected
		}
	}
	return nil
}

// WriteWithResults applies mutations and returns one accepted/rejected result
// per mutation. Conditions and writes are evaluated atomically by the owning
// tablet; mutations without conditions are always accepted.
func (s *EngineStore) WriteWithResults(_ context.Context, table string, muts []*embedpb.Mutation) ([]*embedpb.MutationResult, error) {
	if table == "" {
		return nil, errors.New("embedstore: table is required")
	}
	mutations := make([]engine.ConditionalMutation, 0, len(muts))
	unconditional := make([]*cclient.Mutation, 0, len(muts))
	hasConditions := false
	for mutationIndex, pm := range muts {
		if pm == nil {
			return nil, fmt.Errorf("embedstore: mutation %d is nil", mutationIndex)
		}
		m, err := cclient.NewMutation(pm.Row)
		if err != nil {
			return nil, fmt.Errorf("embedstore: mutation %q: %w", pm.Row, err)
		}
		for _, e := range pm.Entries {
			if e == nil {
				return nil, fmt.Errorf("embedstore: mutation %q has nil entry", pm.Row)
			}
			ts := e.Timestamp
			if ts == 0 {
				ts = s.nextTimestamp()
			}
			if e.Delete {
				m.Delete(e.ColumnFamily, e.ColumnQualifier, e.ColumnVisibility, ts)
			} else {
				m.Put(e.ColumnFamily, e.ColumnQualifier, e.ColumnVisibility, ts, e.Value)
			}
		}
		conditions := make([]engine.Condition, 0, len(pm.Conditions))
		hasConditions = hasConditions || len(pm.Conditions) > 0
		for conditionIndex, condition := range pm.Conditions {
			if condition == nil {
				return nil, fmt.Errorf("embedstore: mutation %q condition %d is nil", pm.Row, conditionIndex)
			}
			converted := engine.Condition{
				ColumnFamily:     append([]byte(nil), condition.ColumnFamily...),
				ColumnQualifier:  append([]byte(nil), condition.ColumnQualifier...),
				ColumnVisibility: append([]byte(nil), condition.ColumnVisibility...),
			}
			if condition.Timestamp != nil {
				timestamp := *condition.Timestamp
				converted.Timestamp = &timestamp
			}
			switch predicate := condition.Predicate.(type) {
			case *embedpb.Condition_Absent:
				if !predicate.Absent {
					return nil, fmt.Errorf("embedstore: mutation %q condition %d: absent must be true: %w", pm.Row, conditionIndex, ErrInvalidCondition)
				}
				converted.Kind = engine.ConditionAbsent
			case *embedpb.Condition_ValueEquals:
				converted.Kind = engine.ConditionValueEquals
				converted.Value = append([]byte(nil), predicate.ValueEquals...)
			default:
				return nil, fmt.Errorf("embedstore: mutation %q condition %d: %w", pm.Row, conditionIndex, ErrInvalidCondition)
			}
			conditions = append(conditions, converted)
		}
		mutations = append(mutations, engine.ConditionalMutation{Mutation: m, Conditions: conditions})
		unconditional = append(unconditional, m)
	}
	if !hasConditions {
		if err := s.eng.Write(table, unconditional); err != nil {
			return nil, err
		}
		results := make([]*embedpb.MutationResult, len(muts))
		for i := range results {
			results[i] = &embedpb.MutationResult{Status: embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED}
		}
		return results, nil
	}
	accepted, err := s.eng.ConditionalWrite(table, mutations)
	if err != nil {
		return nil, err
	}
	results := make([]*embedpb.MutationResult, len(accepted))
	for i, ok := range accepted {
		status := embedpb.MutationStatus_MUTATION_STATUS_REJECTED
		if ok {
			status = embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED
		}
		results[i] = &embedpb.MutationResult{Status: status}
	}
	return results, nil
}

// Flush forces table's memtables to disk.
func (s *EngineStore) Flush(_ context.Context, table string) error {
	if table == "" {
		return errors.New("embedstore: table is required")
	}
	return s.eng.Flush(table)
}

// Compact flushes then compacts table.
func (s *EngineStore) Compact(_ context.Context, table string) error {
	if table == "" {
		return errors.New("embedstore: table is required")
	}
	if err := s.eng.Flush(table); err != nil {
		return err
	}
	return s.eng.Compact(table, nil)
}

// Scan runs req against table and returns the matching cells, honoring
// req.Limit (0 = no limit). It is the buffered counterpart of Scanner used by
// in-process callers such as agentmem.
func (s *EngineStore) Scan(ctx context.Context, table string, req *embedpb.ScanRequest) ([]*embedpb.Cell, error) {
	if table == "" {
		return nil, errors.New("embedstore: table is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sc, err := s.ScannerContext(ctx, table, req)
	if err != nil {
		return nil, err
	}
	defer sc.Close()

	limit := int(req.Limit)
	var cells []*embedpb.Cell
	for sc.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		k := sc.Key()
		cells = append(cells, &embedpb.Cell{
			Row:              k.Row,
			ColumnFamily:     k.ColumnFamily,
			ColumnQualifier:  k.ColumnQualifier,
			ColumnVisibility: k.ColumnVisibility,
			Timestamp:        k.Timestamp,
			Value:            sc.Value(),
		})
		if err := sc.Advance(); err != nil {
			return nil, fmt.Errorf("embedstore: scan advance: %w", err)
		}
		if limit > 0 && len(cells) >= limit {
			break
		}
	}
	return cells, nil
}

// ScanRange derives the engine scan range from a request's row bounds. A
// RowPrefix takes precedence; otherwise explicit StartRow/EndRow bounds apply,
// each defaulting to infinite when unset. An empty request scans everything.
//
// StartRow/EndRow are row-granular (the embedpb contract names them rows), but
// engine ranges are key-granular: a bare Key{Row: r} sorts before every cell in
// row r (its column family/qualifier are empty), so using it directly as an
// inclusive end would drop the row's actual cells. ScanRange therefore expands
// row bounds to whole-row key bounds, matching Accumulo's Range-over-rows:
//   - inclusive start r  -> Key{Row: r}            (covers all of r)
//   - exclusive start r  -> Key{Row: r + 0x00}     (skips all of r)
//   - inclusive end r    -> Key{Row: r + 0x00}, exclusive (covers all of r)
//   - exclusive end r    -> Key{Row: r}, exclusive (drops all of r)
func ScanRange(req *embedpb.ScanRequest) iterrt.Range {
	if req.RowPrefix != "" {
		startRow := []byte(req.RowPrefix)
		endRow := make([]byte, len(startRow))
		copy(endRow, startRow)
		endRow[len(endRow)-1]++
		return iterrt.Range{
			Start:          &iterrt.Key{Row: startRow},
			StartInclusive: true,
			End:            &iterrt.Key{Row: endRow},
			EndInclusive:   false,
		}
	}
	if len(req.StartRow) == 0 && len(req.EndRow) == 0 {
		return iterrt.InfiniteRange()
	}
	r := iterrt.Range{}
	if len(req.StartRow) > 0 {
		if req.StartInclusive {
			r.Start = &iterrt.Key{Row: req.StartRow}
		} else {
			r.Start = &iterrt.Key{Row: appendZero(req.StartRow)}
		}
		r.StartInclusive = true
	} else {
		r.InfiniteStart = true
	}
	if len(req.EndRow) > 0 {
		if req.EndInclusive {
			r.End = &iterrt.Key{Row: appendZero(req.EndRow)}
		} else {
			r.End = &iterrt.Key{Row: req.EndRow}
		}
		r.EndInclusive = false
	} else {
		r.InfiniteEnd = true
	}
	return r
}

// appendZero returns row with a trailing 0x00 byte — the smallest row that
// sorts strictly after every cell of row, used to make row bounds whole-row.
func appendZero(row []byte) []byte {
	out := make([]byte, len(row)+1)
	copy(out, row)
	return out
}

// Scanner builds the engine scanner for req over table. When req.TermFilter is
// set it runs the term-index pushdown; req.VectorSearch the vector k-NN
// pushdown; req.EdgeExpand graph expansion; req.ScoreFilter terminal score
// ranking. Each pushdown is hosted above a re-seekable whole-table merge so
// cells may span tablets, and at most one may be set. With none set it runs an
// ordinary version-capped scan. The caller owns Close on the returned scanner.
func (s *EngineStore) Scanner(table string, req *embedpb.ScanRequest) (*engine.Scanner, error) {
	return s.ScannerContext(context.Background(), table, req)
}

// ScannerContext is Scanner with cancellation for hosted source construction
// and embedding metadata reads.
func (s *EngineStore) ScannerContext(
	ctx context.Context,
	table string,
	req *embedpb.ScanRequest,
) (*engine.Scanner, error) {
	if table == "" {
		return nil, errors.New("embedstore: table is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	scanRange := ScanRange(req)

	set := 0
	if req.TermFilter != nil {
		set++
	}
	if req.VectorSearch != nil {
		set++
	}
	if req.EdgeExpand != nil {
		set++
	}
	if req.ScoreFilter != nil {
		set++
	}
	if set > 1 {
		return nil, ErrMultiplePushdowns
	}

	if tf := req.TermFilter; tf != nil {
		opts := map[string]string{
			iterrt.TermIndexCount: strconv.Itoa(len(tf.TermRows)),
		}
		for i, tr := range tf.TermRows {
			opts[fmt.Sprintf("%s%d", iterrt.TermIndexTermPrefix, i)] = string(tr)
		}
		if len(tf.PrimaryPrefix) > 0 {
			opts[iterrt.TermIndexPrimaryPrefix] = string(tf.PrimaryPrefix)
		}
		if tf.IdSource != "" {
			opts[iterrt.TermIndexIDSource] = tf.IdSource
		}
		if len(tf.PostingCf) > 0 {
			opts[iterrt.TermIndexPostingCF] = string(tf.PostingCf)
		}
		if tf.Phrase {
			opts[iterrt.TermIndexPhrase] = "true"
		}
		if nr := tf.NumericRange; nr != nil {
			if nr.LowerSet {
				opts[iterrt.TermIndexNumericLowerSet] = "true"
				opts[iterrt.TermIndexNumericLower] = strconv.FormatFloat(nr.Lower, 'g', -1, 64)
				if nr.LowerInclusive {
					opts[iterrt.TermIndexNumericLowerInclusive] = "true"
				}
			}
			if nr.UpperSet {
				opts[iterrt.TermIndexNumericUpperSet] = "true"
				opts[iterrt.TermIndexNumericUpper] = strconv.FormatFloat(nr.Upper, 'g', -1, 64)
				if nr.UpperInclusive {
					opts[iterrt.TermIndexNumericUpperInclusive] = "true"
				}
			}
		}
		return s.eng.ScanHostedContext(ctx, table, scanRange, engine.ScanOptions{},
			[]iterrt.IterSpec{{Name: iterrt.IterTermIndex, Options: opts}})
	}

	if vs := req.VectorSearch; vs != nil {
		if len(vs.Query) == 0 {
			return nil, ErrVectorQueryRequired
		}
		if err := embeddingspace.ValidateQueryStates(
			"embedded vector search", vs.EmbeddingSpace); err != nil {
			return nil, errors.Join(ErrVectorEmbeddingSpaceRequired, err)
		}
		opts := map[string]string{
			iterrt.VectorKNNQuery:          base64.StdEncoding.EncodeToString(vs.Query),
			iterrt.VectorKNNEmbeddingSpace: vs.EmbeddingSpace,
		}
		if vs.TopK > 0 {
			opts[iterrt.VectorKNNTopK] = strconv.Itoa(int(vs.TopK))
		}
		if len(vs.EmbeddingCf) > 0 {
			opts[iterrt.VectorKNNEmbeddingCF] = string(vs.EmbeddingCf)
		}
		if vs.Metric != "" {
			opts[iterrt.VectorKNNMetric] = vs.Metric
		}
		if vs.MinScoreSet {
			opts[iterrt.VectorKNNMinScore] = strconv.FormatFloat(float64(vs.MinScore), 'g', -1, 32)
		}
		return s.eng.ScanHostedContext(ctx, table, scanRange, engine.ScanOptions{},
			[]iterrt.IterSpec{{Name: iterrt.IterVectorKNN, Options: opts}})
	}

	if ee := req.EdgeExpand; ee != nil {
		if ee.MaxHops < 0 {
			return nil, ErrNegativeMaxHops
		}
		opts := map[string]string{
			iterrt.EdgeExpandAnchorCount: strconv.Itoa(len(ee.AnchorRows)),
		}
		for i, a := range ee.AnchorRows {
			opts[fmt.Sprintf("%s%d", iterrt.EdgeExpandAnchorPrefix, i)] = string(a)
		}
		if len(ee.EdgeCf) > 0 {
			opts[iterrt.EdgeExpandEdgeCF] = string(ee.EdgeCf)
		}
		if ee.EdgeField != "" {
			opts[iterrt.EdgeExpandEdgeField] = ee.EdgeField
		}
		if len(ee.FieldSep) > 0 {
			opts[iterrt.EdgeExpandFieldSep] = string(ee.FieldSep)
		}
		if ee.IdIndexSet {
			opts[iterrt.EdgeExpandIDIndex] = strconv.Itoa(int(ee.IdIndex))
		}
		if ee.RelIndex != 0 {
			opts[iterrt.EdgeExpandRelIndex] = strconv.Itoa(int(ee.RelIndex))
		}
		if len(ee.Relationships) > 0 {
			opts[iterrt.EdgeExpandRelCount] = strconv.Itoa(len(ee.Relationships))
			for i, r := range ee.Relationships {
				opts[fmt.Sprintf("%s%d", iterrt.EdgeExpandRelPrefix, i)] = r
			}
		}
		if len(ee.PrimaryPrefix) > 0 {
			opts[iterrt.EdgeExpandPrimaryPrefix] = string(ee.PrimaryPrefix)
		}
		if ee.IncludeAnchors {
			opts[iterrt.EdgeExpandIncludeAnchors] = "true"
		}
		if ee.MaxHops > 0 {
			opts[iterrt.EdgeExpandMaxHops] = strconv.Itoa(int(ee.MaxHops))
		}
		if len(ee.EdgeWeights) > 0 {
			opts[iterrt.EdgeExpandWeightCount] = strconv.Itoa(len(ee.EdgeWeights))
			for i, ew := range ee.EdgeWeights {
				opts[fmt.Sprintf("%s%d", iterrt.EdgeExpandWeightRelPrefix, i)] = ew.Relationship
				opts[fmt.Sprintf("%s%d", iterrt.EdgeExpandWeightValuePrefix, i)] =
					strconv.FormatFloat(float64(ew.Weight), 'g', -1, 32)
			}
		}
		return s.eng.ScanHostedContext(ctx, table, scanRange, engine.ScanOptions{},
			[]iterrt.IterSpec{{Name: iterrt.IterEdgeExpand, Options: opts}})
	}

	if sf := req.ScoreFilter; sf != nil {
		opts := map[string]string{}
		if len(sf.ScoreCf) > 0 {
			opts[iterrt.ScoreFilterScoreCF] = string(sf.ScoreCf)
		}
		if sf.Method != "" {
			opts[iterrt.ScoreFilterMethod] = sf.Method
		}
		if len(sf.Query) > 0 {
			opts[iterrt.ScoreFilterQuery] = base64.StdEncoding.EncodeToString(sf.Query)
		}
		if sf.TopK > 0 {
			opts[iterrt.ScoreFilterTopK] = strconv.Itoa(int(sf.TopK))
		}
		if len(sf.Params) > 0 {
			opts[iterrt.ScoreFilterParamCount] = strconv.Itoa(len(sf.Params))
			for i, p := range sf.Params {
				opts[fmt.Sprintf("%s%d", iterrt.ScoreFilterParamPrefix, i)] =
					strconv.FormatFloat(float64(p), 'g', -1, 32)
			}
		}
		if sf.TimestampAnchorMs != 0 {
			opts[iterrt.ScoreFilterTimestampAnchorMs] = strconv.FormatInt(sf.TimestampAnchorMs, 10)
		}
		if sf.HalfLifeMs != 0 {
			opts[iterrt.ScoreFilterHalfLifeMs] = strconv.FormatInt(sf.HalfLifeMs, 10)
		}
		return s.eng.ScanHostedContext(ctx, table, scanRange, engine.ScanOptions{},
			[]iterrt.IterSpec{{Name: iterrt.IterScoreFilter, Options: opts}})
	}

	stack := []iterrt.IterSpec{
		// Deleting sits closest to the source so tombstones resolve before
		// version-capping. propagateDeletes=false suppresses both the
		// tombstone and the cells it covers, giving read-your-deletes for
		// callers that issue Entry.Delete mutations.
		{Name: iterrt.IterDeleting, Options: map[string]string{iterrt.DeletingOptionPropagate: "false"}},
		{Name: iterrt.IterVersioning, Options: map[string]string{iterrt.VersioningOption: "1"}},
	}
	if req.AsOf > 0 {
		// As-of time-travel: drop cells newer than the ceiling BENEATH
		// deleting/versioning so the newest surviving version <= AsOf wins and
		// a tombstone written after AsOf is not yet in effect. Prepending
		// places it closest to the source (specs[0]).
		stack = append([]iterrt.IterSpec{
			{Name: iterrt.IterAsOf, Options: map[string]string{iterrt.AsOfOption: strconv.FormatInt(req.AsOf, 10)}},
		}, stack...)
	}
	return s.eng.Scan(table, scanRange, engine.ScanOptions{Stack: stack})
}
