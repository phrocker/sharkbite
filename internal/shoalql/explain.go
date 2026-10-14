package shoalql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/vectorindex"
)

// Capability is a stable backend feature identifier used by planning
// diagnostics and future backend selection.
type Capability string

const (
	CapabilityRangeScan          Capability = "range_scan"
	CapabilityColumnFamilyFilter Capability = "column_family_filter"
	CapabilityAsOfPushdown       Capability = "as_of_pushdown"
	CapabilityAggregatePushdown  Capability = "aggregate_pushdown"
	CapabilityExactVectorKNN     Capability = "exact_vector_knn"
	CapabilityRowLookup          Capability = "row_lookup"
	CapabilityGraphNeighbors     Capability = "graph_neighbors"
	CapabilityDocumentIndex      Capability = "document_index"
	CapabilityDistributedScan    Capability = "distributed_scan"
	CapabilityDistributedTopK    Capability = "distributed_topk_merge"
	CapabilityApproximateVector  Capability = "approximate_vector_index"
)

var knownCapabilities = []Capability{
	CapabilityAggregatePushdown,
	CapabilityApproximateVector,
	CapabilityAsOfPushdown,
	CapabilityColumnFamilyFilter,
	CapabilityDistributedScan,
	CapabilityDistributedTopK,
	CapabilityDocumentIndex,
	CapabilityExactVectorKNN,
	CapabilityGraphNeighbors,
	CapabilityRangeScan,
	CapabilityRowLookup,
}

// BackendInfo is the stable capability declaration exposed by a backend.
type BackendInfo struct {
	Name                  string       `json:"name"`
	Mode                  string       `json:"mode"`
	Capabilities          []Capability `json:"capabilities"`
	StorageFormats        []string     `json:"storage_formats,omitempty"`
	SelectedStorageFormat string       `json:"selected_storage_format,omitempty"`
	Pushdowns             []string     `json:"pushdowns,omitempty"`
	LocalMaterialization  []string     `json:"local_materialization,omitempty"`
	FallbackReasons       []string     `json:"fallback_reasons,omitempty"`
	OrderingAssumptions   []string     `json:"ordering_top_k_assumptions,omitempty"`
	FallbackIterators     []string     `json:"fallback_iterators,omitempty"`
}

// CapabilityProvider is optional so existing Backend implementations remain
// source-compatible. Backends participating in selection and EXPLAIN should
// provide a stable declaration.
type CapabilityProvider interface {
	BackendInfo() BackendInfo
}

// ExplainDetails is the versioned, machine-readable EXPLAIN contract.
type ExplainDetails struct {
	Version                 int                    `json:"version"`
	Format                  string                 `json:"format"`
	Backend                 BackendInfo            `json:"backend"`
	CapabilityContract      bool                   `json:"capability_contract"`
	Shape                   string                 `json:"shape"`
	Table                   string                 `json:"table"`
	Range                   string                 `json:"range"`
	Pushdowns               []string               `json:"pushdowns"`
	LocalMaterialization    []string               `json:"local_materialization"`
	FallbackReasons         []string               `json:"fallback_reasons"`
	UnsupportedCapabilities []Capability           `json:"unsupported_capabilities"`
	OrderingAssumptions     []string               `json:"ordering_top_k_assumptions"`
	VectorIndex             *vectorindex.Manifest  `json:"vector_index,omitempty"`
	VectorEmbeddingSpace    string                 `json:"vector_embedding_space,omitempty"`
	VectorFreshness         *vectorindex.Freshness `json:"vector_freshness,omitempty"`
	RecallClaimed           bool                   `json:"recall_claimed"`
}

func (e *Executor) explain(p *Plan) (*Result, error) {
	d := buildExplainDetails(e.be, p)
	if p.ExplainFormat == ExplainJSON {
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, fmt.Errorf("shoalql: marshal explain: %w", err)
		}
		return &Result{Columns: []string{"explain"}, Rows: []Row{{strVal(string(raw))}}}, nil
	}

	rows := []Row{
		{strVal("version"), strVal(strconv.Itoa(d.Version))},
		{strVal("format"), strVal(d.Format)},
		{strVal("backend"), strVal(d.Backend.Name)},
		{strVal("backend_mode"), strVal(d.Backend.Mode)},
		{strVal("capability_contract"), strVal(strconv.FormatBool(d.CapabilityContract))},
		{strVal("capabilities"), strVal(joinCapabilities(d.Backend.Capabilities))},
		{strVal("unsupported_capabilities"), strVal(joinCapabilities(d.UnsupportedCapabilities))},
		{strVal("storage_formats"), strVal(strings.Join(d.Backend.StorageFormats, ","))},
		{strVal("selected_storage_format"), strVal(d.Backend.SelectedStorageFormat)},
		{strVal("shape"), strVal(d.Shape)},
		{strVal("table"), strVal(d.Table)},
		{strVal("range"), strVal(d.Range)},
		{strVal("pushdowns"), strVal(strings.Join(d.Pushdowns, "; "))},
		{strVal("local_materialization"), strVal(strings.Join(d.LocalMaterialization, "; "))},
		{strVal("fallback_reasons"), strVal(strings.Join(d.FallbackReasons, "; "))},
		{strVal("ordering_top_k_assumptions"), strVal(strings.Join(d.OrderingAssumptions, "; "))},
	}
	if d.VectorEmbeddingSpace != "" {
		rows = append(rows,
			Row{strVal("vector_embedding_space"), strVal(d.VectorEmbeddingSpace)})
	}
	if d.VectorIndex != nil {
		rows = append(rows,
			Row{strVal("vector_generation"), strVal(strconv.FormatUint(d.VectorIndex.Generation, 10))},
			Row{strVal("vector_index_embedding_space"), strVal(d.VectorIndex.EmbeddingSpace)},
			Row{strVal("vector_codebook_version"), strVal(d.VectorIndex.CodebookVersion)},
			Row{strVal("vector_lineage"), strVal(fmt.Sprint(d.VectorIndex.Lineage))},
			Row{strVal("vector_indexed_watermark"), strVal(strconv.FormatInt(d.VectorIndex.IndexedWatermark, 10))},
			Row{strVal("vector_recall_claimed"), strVal(strconv.FormatBool(d.RecallClaimed))},
		)
	}
	return &Result{Columns: []string{"property", "value"}, Rows: rows}, nil
}

func buildExplainDetails(be Backend, p *Plan) ExplainDetails {
	d := ExplainDetails{
		Version:              1,
		Format:               string(p.ExplainFormat),
		Shape:                shapeName(p.Shape),
		Table:                p.Table,
		Range:                explainRange(p.Range),
		Pushdowns:            []string{},
		LocalMaterialization: []string{},
		FallbackReasons:      []string{},
		OrderingAssumptions:  []string{},
	}
	if d.Format == "" {
		d.Format = string(ExplainText)
	}
	if cp, ok := be.(CapabilityProvider); ok {
		d.CapabilityContract = true
		d.Backend = normalizedBackendInfo(cp.BackendInfo())
		d.UnsupportedCapabilities = unsupportedCapabilities(d.Backend.Capabilities)
		d.Pushdowns = append(d.Pushdowns, d.Backend.Pushdowns...)
		d.LocalMaterialization = append(d.LocalMaterialization, d.Backend.LocalMaterialization...)
		d.FallbackReasons = append(d.FallbackReasons, d.Backend.FallbackReasons...)
		d.OrderingAssumptions = append(d.OrderingAssumptions, d.Backend.OrderingAssumptions...)
	} else {
		d.Backend = BackendInfo{Name: "legacy", Mode: "unspecified", Capabilities: []Capability{}}
		d.UnsupportedCapabilities = []Capability{}
		d.FallbackReasons = append(d.FallbackReasons,
			"backend does not declare capabilities; legacy execution contract assumed")
	}

	d.Pushdowns = append(d.Pushdowns, "row range "+d.Range)
	if len(p.ColumnFamilies) > 0 {
		d.Pushdowns = append(d.Pushdowns, "column families "+quotedBytes(p.ColumnFamilies))
	}
	for _, spec := range p.Stack {
		if spec.Name == iterrt.IterVectorKNN && p.VectorMode == VectorApproximate {
			if p.VectorExactFallback {
				d.LocalMaterialization = append(d.LocalMaterialization,
					"conditional exact vector KNN fallback after an explicit freshness failure")
			}
			continue
		}
		if spec.Name == iterrt.IterVectorKNN &&
			!hasCapability(d.Backend.Capabilities, CapabilityExactVectorKNN) {
			continue
		}
		if stringListed(d.Backend.FallbackIterators, spec.Name) {
			d.LocalMaterialization = append(d.LocalMaterialization,
				"local iterator fallback: "+explainIterator(spec))
		} else {
			d.Pushdowns = append(d.Pushdowns, explainIterator(spec))
		}
	}

	switch p.Shape {
	case ShapeVectorKNN:
		d.VectorEmbeddingSpace = p.VectorEmbeddingSpace
		addVectorExplain(be, p, &d)
		if !hasCapability(d.Backend.Capabilities, CapabilityExactVectorKNN) &&
			p.VectorMode != VectorApproximate {
			d.FallbackReasons = append(d.FallbackReasons,
				"exact vector execution is refused because the backend cannot prove per-file embedding identity")
		} else if hasCapability(d.Backend.Capabilities, CapabilityDistributedScan) {
			d.OrderingAssumptions = append(d.OrderingAssumptions,
				"distributed scan candidates are merged into one exact score-descending top-k with ascending-key tie-break")
		} else {
			d.OrderingAssumptions = append(d.OrderingAssumptions,
				"single-backend exact top-k is score-descending with ascending-key tie-break",
				"no distributed partial top-k merge is performed")
		}
		if p.NeedsHydration {
			d.LocalMaterialization = append(d.LocalMaterialization,
				"lookup winning row keys and hydrate projected cells")
		}
	case ShapeAggregate:
		d.OrderingAssumptions = append(d.OrderingAssumptions,
			"aggregation groups are emitted in backend iterator order")
	case ShapeDocument:
		if p.VectorMode == VectorApproximate {
			d.VectorEmbeddingSpace = p.VectorEmbeddingSpace
			addVectorExplain(be, p, &d)
		}
		for _, term := range p.DocTerms {
			d.Pushdowns = append(d.Pushdowns,
				fmt.Sprintf("document index term %s=%q", term.Field, term.Value))
		}
		if stringListed(d.Backend.FallbackIterators, iterrt.IterDocumentIndex) {
			d.LocalMaterialization = append(d.LocalMaterialization,
				"local iterator fallback: document index iterator")
		} else {
			d.Pushdowns = append(d.Pushdowns, "document index iterator")
		}
		d.LocalMaterialization = append(d.LocalMaterialization,
			"intersect candidate shard sets", "reconstruct and project documents")
		if len(p.DocResidual) > 0 {
			d.LocalMaterialization = append(d.LocalMaterialization,
				"evaluate document id/type residual predicates")
			d.FallbackReasons = append(d.FallbackReasons,
				"document id/type predicates are not field-index pushdowns")
		}
		d.OrderingAssumptions = append(d.OrderingAssumptions,
			"candidate shards are sorted ascending before document reconstruction")
	default:
		d.OrderingAssumptions = append(d.OrderingAssumptions,
			"backend scan keys are ascending; logical rows are contiguous")
	}

	if len(p.Residual) > 0 {
		for _, residual := range p.Residual {
			d.LocalMaterialization = append(d.LocalMaterialization, explainResidual(residual))
		}
		d.FallbackReasons = append(d.FallbackReasons,
			"scalar or MATCH predicates without an index pushdown are evaluated locally")
	}
	for _, col := range p.Projection {
		if col.Kind == OutExpand {
			d.LocalMaterialization = append(d.LocalMaterialization,
				"resolve graph neighbors for materialized source rows")
			break
		}
	}
	return d
}

func addVectorExplain(be Backend, p *Plan, d *ExplainDetails) {
	if p.VectorMode != VectorApproximate {
		d.FallbackReasons = append(d.FallbackReasons, "exact vector mode selected explicitly")
		return
	}
	freshness := p.VectorFreshness
	d.VectorFreshness = &freshness
	d.Pushdowns = append(d.Pushdowns,
		fmt.Sprintf("IVF-PQ index=%s nprobe=%d", p.VectorIndex, p.VectorNProbe))
	d.OrderingAssumptions = append(d.OrderingAssumptions,
		"sharded partial top-k merge is score-descending with document-id ascending tie-break")
	if p.VectorExactFallback {
		d.FallbackReasons = append(d.FallbackReasons,
			"exact fallback is enabled explicitly when generation/freshness requirements are not met")
	}
	if provider, ok := be.(VectorExplainBackend); ok {
		manifest, err := provider.DescribeVector(context.Background(), p.VectorIndex)
		if err == nil {
			d.VectorIndex = &manifest
			spaceCompatible := embeddingspace.EnsureSameIdentity(
				"explain approximate vector query",
				manifest.EmbeddingSpace,
				p.VectorEmbeddingSpace,
			) == nil
			d.RecallClaimed = spaceCompatible && manifest.Recall.Benchmarked()
			if !spaceCompatible {
				d.FallbackReasons = append(d.FallbackReasons,
					"query embedding space is incompatible with the vector index; recall is not claimed")
			}
			if !d.RecallClaimed {
				d.FallbackReasons = append(d.FallbackReasons,
					"index has no reproducible corpus benchmark; EXPLAIN makes no recall claim")
			}
		}
	}
}

func normalizedBackendInfo(info BackendInfo) BackendInfo {
	if info.Name == "" {
		info.Name = "unnamed"
	}
	if info.Mode == "" {
		info.Mode = "unspecified"
	}
	info.Capabilities = append([]Capability(nil), info.Capabilities...)
	info.StorageFormats = append([]string(nil), info.StorageFormats...)
	info.Pushdowns = append([]string(nil), info.Pushdowns...)
	info.LocalMaterialization = append([]string(nil), info.LocalMaterialization...)
	info.FallbackReasons = append([]string(nil), info.FallbackReasons...)
	info.OrderingAssumptions = append([]string(nil), info.OrderingAssumptions...)
	info.FallbackIterators = append([]string(nil), info.FallbackIterators...)
	sort.Slice(info.Capabilities, func(i, j int) bool { return info.Capabilities[i] < info.Capabilities[j] })
	sort.Strings(info.StorageFormats)
	return info
}

func stringListed(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasCapability(values []Capability, want Capability) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func unsupportedCapabilities(supported []Capability) []Capability {
	set := make(map[Capability]bool, len(supported))
	for _, capability := range supported {
		set[capability] = true
	}
	var out []Capability
	for _, capability := range knownCapabilities {
		if !set[capability] {
			out = append(out, capability)
		}
	}
	return out
}

func shapeName(shape PlanShape) string {
	switch shape {
	case ShapeVectorKNN:
		return "vector_knn"
	case ShapeAggregate:
		return "aggregate"
	case ShapeDocument:
		return "document"
	default:
		return "scan"
	}
}

func explainIterator(spec iterrt.IterSpec) string {
	switch spec.Name {
	case iterrt.IterAsOf:
		return "as-of timestamp <= " + spec.Options[iterrt.AsOfOption]
	case iterrt.IterGraphAggregation:
		return fmt.Sprintf("aggregate %s group by %s",
			spec.Options[iterrt.GraphAggregationOp], spec.Options[iterrt.GraphAggregationGroupBy])
	case iterrt.IterVectorKNN:
		return fmt.Sprintf("exact vector KNN space=%q metric=%s top_k=%s embedding_cf=%q",
			spec.Options[iterrt.VectorKNNEmbeddingSpace],
			spec.Options[iterrt.VectorKNNMetric], spec.Options[iterrt.VectorKNNTopK],
			spec.Options[iterrt.VectorKNNEmbeddingCF])
	default:
		return "iterator " + spec.Name
	}
}

func explainResidual(r ResidualFilter) string {
	if r.IsMatch {
		return fmt.Sprintf("evaluate MATCH on cf=%q cq=%q terms=%q", r.CF, r.CQ, r.Terms)
	}
	return fmt.Sprintf("evaluate predicate cf=%q cq=%q %s %q", r.CF, r.CQ, compareName(r.Op), r.Str)
}

func compareName(op CompareOp) string {
	switch op {
	case OpEq:
		return "="
	case OpGE:
		return ">="
	case OpGT:
		return ">"
	case OpLE:
		return "<="
	case OpLT:
		return "<"
	case OpLike:
		return "LIKE"
	default:
		return "?"
	}
}

func explainRange(r iterrt.Range) string {
	start := "-inf"
	end := "+inf"
	if !r.InfiniteStart && r.Start != nil {
		start = explainBytes(r.Start.Row)
	}
	if !r.InfiniteEnd && r.End != nil {
		end = explainBytes(r.End.Row)
	}
	left, right := "(", ")"
	if r.StartInclusive {
		left = "["
	}
	if r.EndInclusive {
		right = "]"
	}
	return left + start + "," + end + right
}

func explainBytes(b []byte) string {
	for _, r := range string(b) {
		if !unicode.IsPrint(r) {
			return fmt.Sprintf("%x", b)
		}
	}
	return strconv.Quote(string(b))
}

func quotedBytes(values [][]byte) string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = explainBytes(value)
	}
	return "[" + strings.Join(out, ",") + "]"
}

func joinCapabilities(values []Capability) string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = string(value)
	}
	return strings.Join(out, ",")
}
