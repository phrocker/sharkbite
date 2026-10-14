package scanserver

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/phrocker/shoal-oss/internal/ivfpq"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

const wholeRowIteratorClassName = "org.apache.accumulo.core.iterators.user.WholeRowIterator"
const grepIteratorClassName = "org.apache.accumulo.core.iterators.user.GrepIterator"
const rowFateStatusFilterClassName = "org.apache.accumulo.core.fate.user.RowFateStatusFilter"
const tabletManagementIteratorClassName = "org.apache.accumulo.server.manager.state.TabletManagementIterator"
const hasMigrationFilterClassName = "org.apache.accumulo.core.metadata.schema.filters.HasMigrationFilter"
const hasExternalCompactionsFilterClassName = "org.apache.accumulo.core.metadata.schema.filters.HasExternalCompactionsFilter"
const hasCurrentFilterClassName = "org.apache.accumulo.core.metadata.schema.filters.HasCurrentFilter"
const gcWalsFilterClassName = "org.apache.accumulo.core.metadata.schema.filters.GcWalsFilter"

// cellPostProcessor consumes cells from the heap-merge in the order
// they would have been emitted, then produces the final result list.
// Used when a server-side iterator can be replicated in Go and
// REPLACES the streaming output (e.g., IvfPqDistanceIterator emits
// top-K in score order, not the original key order).
type cellPostProcessor interface {
	// offer feeds one heap-merge output cell to the iterator. Caller
	// MUST not retain the wire.Key beyond this call (the iterator
	// copies whatever it needs internally).
	offer(k *wire.Key, v []byte)
	// drain returns the iterator's final output as a list of TKeyValue
	// in the order they should be wire-shipped. Calling this also
	// resets the iterator's internal state.
	drain() []*data.TKeyValue
	err() error
}

// buildPostProcessor inspects the request's iterator settings and
// returns a Go-native processor when one matches a class shoal knows
// how to run. Unknown iterators yield (nil, nil) — the caller falls
// back to the standard streaming path.
//
// Currently recognized:
//   - IvfPqDistanceIterator: replicated by internal/ivfpq.
//   - WholeRowIterator: required by Accumulo's metadata-table scans.
//
// Multiple iterators in ssiList: V1 only supports a single recognized
// iterator (the Java handler only wires one for /vector/search).
// If more than one shoal-known iterator is configured we error out
// rather than guess at the composition order.
func buildPostProcessor(ssiList []*data.IterInfo, ssio map[string]map[string]string) (cellPostProcessor, error) {
	if len(ssiList) == 0 {
		return nil, nil
	}
	// Sort by priority ascending so we report the lowest-priority
	// recognized iterator first if the request has multiple. (Java's
	// SortedKeyValueIterator stack runs lowest-priority first.)
	sorted := make([]*data.IterInfo, len(ssiList))
	copy(sorted, ssiList)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Priority < sorted[j].Priority
	})

	var picked cellPostProcessor
	for _, info := range sorted {
		if info == nil {
			continue
		}
		switch info.ClassName {
		case ivfpq.IteratorClassName:
			if picked != nil {
				return nil, fmt.Errorf("scanserver: multiple shoal-recognized iterators not supported in V1")
			}
			opts := ssio[info.IterName]
			it, err := ivfpq.NewFromOptions(opts)
			if err != nil {
				return nil, fmt.Errorf("scanserver: build ivfpq iterator (%s): %w", info.IterName, err)
			}
			picked = &ivfpqProcessor{it: it}
		case wholeRowIteratorClassName:
			if picked != nil {
				return nil, fmt.Errorf("scanserver: multiple shoal-recognized iterators not supported in V1")
			}
			maxBufferSize := int64(math.MaxInt64)
			if raw := ssio[info.IterName]["maxBufferSize"]; raw != "" {
				parsed, err := parseAccumuloMemory(raw)
				if err != nil {
					return nil, fmt.Errorf(
						"scanserver: build whole-row iterator (%s): invalid maxBufferSize %q: %w",
						info.IterName, raw, err,
					)
				}
				maxBufferSize = parsed
			}
			picked = &wholeRowProcessor{maxBufferSize: maxBufferSize}
		case grepIteratorClassName:
			if picked != nil {
				return nil, fmt.Errorf("scanserver: multiple shoal-recognized iterators not supported in V1")
			}
			processor, err := newGrepProcessor(ssio[info.IterName])
			if err != nil {
				return nil, fmt.Errorf(
					"scanserver: build grep iterator (%s): %w",
					info.IterName, err,
				)
			}
			picked = processor
		case rowFateStatusFilterClassName:
			if picked != nil {
				return nil, fmt.Errorf("scanserver: multiple shoal-recognized iterators not supported in V1")
			}
			statuses := make(map[string]struct{})
			for _, status := range strings.Split(ssio[info.IterName]["statuses"], ",") {
				if status != "" {
					statuses[status] = struct{}{}
				}
			}
			picked = &wholeRowProcessor{
				maxBufferSize: math.MaxInt64,
				accept: func(cells []wholeRowCell) bool {
					for _, cell := range cells {
						if string(cell.key.ColumnFamily) != "txadmin" ||
							string(cell.key.ColumnQualifier) != "status" {
							continue
						}
						if _, ok := statuses[string(cell.value)]; ok {
							return true
						}
					}
					return false
				},
			}
		case tabletManagementIteratorClassName:
			if picked != nil {
				return nil, fmt.Errorf("scanserver: multiple shoal-recognized iterators not supported in V1")
			}
			processor, err := newTabletManagementProcessor(ssio[info.IterName])
			if err != nil {
				return nil, fmt.Errorf(
					"scanserver: build tablet-management iterator (%s): %w",
					info.IterName, err,
				)
			}
			picked = processor
		case hasMigrationFilterClassName:
			if picked != nil {
				return nil, fmt.Errorf("scanserver: multiple shoal-recognized iterators not supported in V1")
			}
			picked = newMetadataColumnFilter(metadata.CFServer, "migration")
		case hasExternalCompactionsFilterClassName:
			if picked != nil {
				return nil, fmt.Errorf("scanserver: multiple shoal-recognized iterators not supported in V1")
			}
			picked = newMetadataColumnFilter("ecomp", "")
		case hasCurrentFilterClassName:
			if picked != nil {
				return nil, fmt.Errorf("scanserver: multiple shoal-recognized iterators not supported in V1")
			}
			picked = newMetadataColumnFilter(metadata.CFCurrentLocation, "")
		case gcWalsFilterClassName:
			if picked != nil {
				return nil, fmt.Errorf("scanserver: multiple shoal-recognized iterators not supported in V1")
			}
			picked = newGcWalsFilter(ssio[info.IterName]["liveTservers"])
		default:
			// Unknown iterator. Java would error if a class wasn't on
			// the classpath; shoal mirrors this rather than silently
			// giving wrong answers. The Java caller is expected to
			// only set iterators shoal knows about (currently just
			// IvfPqDistanceIterator); anything else means a routing
			// mistake and the caller should fall back to tserver.
			return nil, fmt.Errorf("scanserver: unsupported iterator class %q (priority=%d, name=%s)",
				info.ClassName, info.Priority, info.IterName)
		}
	}
	return picked, nil
}

// ivfpqProcessor adapts ivfpq.Iterator to the cellPostProcessor
// interface. Cells flowing in must be the (row, V:_pq) pairs the
// iterator expects; the heap-merge typically already filters to those
// via the column pushdown layer, but the iterator silently drops
// wrong-length values either way.
type ivfpqProcessor struct {
	it *ivfpq.Iterator
}

func (p *ivfpqProcessor) offer(k *wire.Key, v []byte) {
	// Clone the row + CF + CQ + CV bytes here — the heap-merge owns
	// these slices and can recycle them on the next iteration.
	p.it.Offer(
		cloneBytes(k.Row),
		cloneBytes(k.ColumnFamily),
		cloneBytes(k.ColumnQualifier),
		cloneBytes(k.ColumnVisibility),
		k.Timestamp,
		v, // Drain owns the score bytes; the input value is not retained.
	)
}

func (p *ivfpqProcessor) drain() []*data.TKeyValue {
	cells := p.it.Drain()
	out := make([]*data.TKeyValue, 0, len(cells))
	for _, c := range cells {
		out = append(out, &data.TKeyValue{
			Key: &data.TKey{
				Row:           c.Row,
				ColFamily:     c.CF,
				ColQualifier:  c.CQ,
				ColVisibility: c.CV,
				Timestamp:     c.TS,
			},
			Value: c.Value, // 4-byte big-endian float32 score
		})
	}
	return out
}

func (p *ivfpqProcessor) err() error { return nil }

type grepProcessor struct {
	term         []byte
	matchRow     bool
	matchColFam  bool
	matchColQual bool
	matchColVis  bool
	matchValue   bool
	results      []*data.TKeyValue
}

func newGrepProcessor(options map[string]string) (*grepProcessor, error) {
	term, ok := options["term"]
	if !ok {
		return nil, errors.New("missing term option")
	}
	return &grepProcessor{
		term:         []byte(term),
		matchRow:     javaBooleanOption(options, "matchRow", true),
		matchColFam:  javaBooleanOption(options, "matchColumnFamily", true),
		matchColQual: javaBooleanOption(options, "matchColumnQualifier", true),
		matchColVis:  javaBooleanOption(options, "matchColumnVisibility", false),
		matchValue:   javaBooleanOption(options, "matchValue", true),
	}, nil
}

func javaBooleanOption(options map[string]string, name string, fallback bool) bool {
	value, ok := options[name]
	if !ok {
		return fallback
	}
	return strings.EqualFold(value, "true")
}

func (p *grepProcessor) offer(k *wire.Key, value []byte) {
	if k == nil {
		return
	}
	matches := (p.matchRow && bytes.Contains(k.Row, p.term)) ||
		(p.matchColFam && bytes.Contains(k.ColumnFamily, p.term)) ||
		(p.matchColQual && bytes.Contains(k.ColumnQualifier, p.term)) ||
		(p.matchColVis && bytes.Contains(k.ColumnVisibility, p.term)) ||
		(p.matchValue && bytes.Contains(value, p.term))
	if !matches {
		return
	}
	p.results = append(p.results, &data.TKeyValue{
		Key: &data.TKey{
			Row:           cloneBytes(k.Row),
			ColFamily:     cloneBytes(k.ColumnFamily),
			ColQualifier:  cloneBytes(k.ColumnQualifier),
			ColVisibility: cloneBytes(k.ColumnVisibility),
			Timestamp:     k.Timestamp,
		},
		Value: cloneBytes(value),
	})
}

func (p *grepProcessor) drain() []*data.TKeyValue {
	results := p.results
	p.results = nil
	return results
}

func (p *grepProcessor) err() error { return nil }

type wholeRowCell struct {
	key   wire.Key
	value []byte
}

type wholeRowProcessor struct {
	currentRow    []byte
	current       []wholeRowCell
	bufferSize    int64
	maxBufferSize int64
	accept        func([]wholeRowCell) bool
	results       []*data.TKeyValue
	overflow      error
}

func (p *wholeRowProcessor) offer(k *wire.Key, value []byte) {
	if p.overflow != nil {
		return
	}
	if p.currentRow != nil && !bytes.Equal(p.currentRow, k.Row) {
		p.flush()
	}
	if p.currentRow == nil {
		p.currentRow = cloneBytes(k.Row)
	}
	p.bufferSize += int64(len(k.Row) + len(k.ColumnFamily) + len(k.ColumnQualifier) +
		len(k.ColumnVisibility) + len(value) + 9 + 128)
	limit := p.maxBufferSize
	if limit == 0 {
		limit = math.MaxInt64
	}
	if p.bufferSize > limit {
		p.overflow = fmt.Errorf(
			"scanserver: WholeRowIterator exceeded maxBufferSize %d for row %q",
			limit, k.Row,
		)
		return
	}
	p.current = append(p.current, wholeRowCell{
		key:   *k.Clone(),
		value: cloneBytes(value),
	})
}

func (p *wholeRowProcessor) drain() []*data.TKeyValue {
	if p.overflow != nil {
		return nil
	}
	p.flush()
	results := p.results
	p.results = nil
	return results
}

func (p *wholeRowProcessor) err() error { return p.overflow }

func (p *wholeRowProcessor) flush() {
	if len(p.current) == 0 {
		return
	}
	if p.accept == nil || p.accept(p.current) {
		p.results = append(p.results, encodeWholeRow(p.currentRow, p.current))
	}
	p.currentRow = nil
	p.current = nil
	p.bufferSize = 0
}

func writeWholeRowField(out *bytes.Buffer, value []byte) {
	_ = binary.Write(out, binary.BigEndian, int32(len(value)))
	_, _ = out.Write(value)
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func parseAccumuloMemory(raw string) (int64, error) {
	if raw == "" {
		return 0, errors.New("empty memory value")
	}
	multiplier := int64(1)
	number := raw
	switch strings.ToUpper(raw[len(raw)-1:]) {
	case "B":
		number = raw[:len(raw)-1]
	case "K":
		number = raw[:len(raw)-1]
		multiplier = 1 << 10
	case "M":
		number = raw[:len(raw)-1]
		multiplier = 1 << 20
	case "G":
		number = raw[:len(raw)-1]
		multiplier = 1 << 30
	}
	value, err := strconv.ParseInt(number, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid memory value")
	}
	if value > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("memory value overflows int64")
	}
	return value * multiplier, nil
}
