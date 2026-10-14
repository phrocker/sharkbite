package parquetfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/parquet-go/parquet-go"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/storage"
)

type FileOpener func() (storage.File, error)

type ReadStats struct {
	RowGroupsTotal       int
	RowGroupsRead        int
	RowGroupsPruned      int
	RowsSkippedPageIndex int64
	RowsDecoded          int64
}

type rowGroupIndex struct {
	group  parquet.RowGroup
	rowCol parquet.ColumnChunk
	minRow []byte
	maxRow []byte
}

type selectedGroup struct {
	index    int
	startRow int64
}

// Source is a re-seekable Parquet SKVI. It reads selected row groups lazily
// through ReaderAt and prunes groups by row min/max statistics and bloom filters.
type Source struct {
	file   storage.File
	opener FileOpener
	groups []rowGroupIndex

	rng       iterrt.Range
	cfs       [][]byte
	inclusive bool
	selected  []selectedGroup
	groupPos  int
	reader    *parquet.GenericReader[Cell]
	batch     []Cell
	batchPos  int
	batchLen  int
	topKey    *wire.Key
	topValue  []byte
	hasTop    bool
	err       error
	stats     ReadStats
}

func NewSource(file storage.File, opener FileOpener) (*Source, error) {
	if file == nil {
		return nil, errors.New("parquet: nil file")
	}
	pf, err := parquet.OpenFile(file, file.Size(), parquet.ReadBufferSize(64<<10))
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("parquet: open file: %w", err)
	}
	rowLeaf, ok := pf.Schema().Lookup("row")
	if !ok {
		_ = file.Close()
		return nil, errors.New("parquet: missing row column")
	}
	metaGroups := pf.Metadata().RowGroups
	rowGroups := pf.RowGroups()
	groups := make([]rowGroupIndex, len(rowGroups))
	for i, group := range rowGroups {
		index := rowGroupIndex{
			group:  group,
			rowCol: group.ColumnChunks()[rowLeaf.ColumnIndex],
		}
		stats := metaGroups[i].Columns[rowLeaf.ColumnIndex].MetaData.Statistics
		if stats.MinValue != nil {
			index.minRow = bytes.Clone(stats.MinValue)
		} else if stats.Min != nil {
			index.minRow = bytes.Clone(stats.Min)
		}
		if stats.MaxValue != nil {
			index.maxRow = bytes.Clone(stats.MaxValue)
		} else if stats.Max != nil {
			index.maxRow = bytes.Clone(stats.Max)
		}
		groups[i] = index
	}
	return &Source{
		file:   file,
		opener: opener,
		groups: groups,
		batch:  make([]Cell, 256),
		stats:  ReadStats{RowGroupsTotal: len(groups)},
	}, nil
}

func (s *Source) Init(source iterrt.SortedKeyValueIterator, _ map[string]string, _ iterrt.IteratorEnvironment) error {
	if source != nil {
		return errors.New("parquet: Source is a leaf iterator, source must be nil")
	}
	return nil
}

func (s *Source) Seek(r iterrt.Range, columnFamilies [][]byte, inclusive bool) error {
	s.closeReader()
	s.rng = r
	s.cfs = columnFamilies
	s.inclusive = inclusive
	s.selected = s.selected[:0]
	s.groupPos = 0
	s.batchPos = 0
	s.batchLen = 0
	s.clearTop()
	s.err = nil
	s.stats.RowGroupsRead = 0
	s.stats.RowGroupsPruned = 0
	s.stats.RowsSkippedPageIndex = 0
	s.stats.RowsDecoded = 0

	pointRow, isPoint := singleRow(r)
	for i := range s.groups {
		group := &s.groups[i]
		if !overlapsRowRange(group, r) {
			s.stats.RowGroupsPruned++
			continue
		}
		if isPoint {
			if bloom := group.rowCol.BloomFilter(); bloom != nil {
				ok, err := bloom.Check(parquet.ByteArrayValue(pointRow))
				if err == nil && !ok {
					s.stats.RowGroupsPruned++
					continue
				}
			}
		}
		startRow := pageStartRow(group, r)
		s.stats.RowsSkippedPageIndex += startRow
		s.selected = append(s.selected, selectedGroup{index: i, startRow: startRow})
	}
	return s.advance()
}

func (s *Source) Next() error {
	if s.err != nil {
		return s.err
	}
	if !s.hasTop {
		return errors.New("parquet: Source.Next called without a top")
	}
	s.clearTop()
	return s.advance()
}

func (s *Source) advance() error {
	for {
		if s.batchPos < s.batchLen {
			cell := &s.batch[s.batchPos]
			s.batchPos++
			s.stats.RowsDecoded++
			key := &wire.Key{
				Row:              cell.Row,
				ColumnFamily:     cell.CF,
				ColumnQualifier:  cell.CQ,
				ColumnVisibility: cell.CV,
				Timestamp:        cell.Timestamp,
				Deleted:          cell.Deleted,
			}
			if s.rng.BeforeStart(key) {
				continue
			}
			if s.rng.AfterEnd(key) {
				s.clearTop()
				s.closeReader()
				return nil
			}
			if !cfAllowed(key.ColumnFamily, s.cfs, s.inclusive) {
				continue
			}
			s.topKey = key
			s.topValue = cell.Value
			s.hasTop = true
			return nil
		}

		if s.reader == nil {
			if s.groupPos >= len(s.selected) {
				return nil
			}
			selected := s.selected[s.groupPos]
			group := s.groups[selected.index].group
			s.groupPos++
			s.reader = parquet.NewGenericRowGroupReader[Cell](group)
			if selected.startRow > 0 {
				if err := s.reader.SeekToRow(selected.startRow); err != nil {
					s.err = fmt.Errorf("parquet: seek row group to %d: %w", selected.startRow, err)
					return s.err
				}
			}
			s.stats.RowGroupsRead++
		}

		n, err := s.reader.Read(s.batch)
		s.batchPos = 0
		s.batchLen = n
		if errors.Is(err, io.EOF) {
			s.closeReader()
			if n == 0 {
				continue
			}
		} else if err != nil {
			s.err = fmt.Errorf("parquet: read row group: %w", err)
			return s.err
		}
	}
}

func pageStartRow(group *rowGroupIndex, r iterrt.Range) int64 {
	if r.InfiniteStart || r.Start == nil {
		return 0
	}
	columnIndex, err := group.rowCol.ColumnIndex()
	if err != nil || !columnIndex.IsAscending() {
		return 0
	}
	offsetIndex, err := group.rowCol.OffsetIndex()
	if err != nil || offsetIndex.NumPages() != columnIndex.NumPages() {
		return 0
	}
	for page := 0; page < columnIndex.NumPages(); page++ {
		if columnIndex.NullPage(page) {
			continue
		}
		if bytes.Compare(columnIndex.MaxValue(page).ByteArray(), r.Start.Row) >= 0 {
			return offsetIndex.FirstRowIndex(page)
		}
	}
	return group.group.NumRows()
}

func overlapsRowRange(group *rowGroupIndex, r iterrt.Range) bool {
	if len(group.minRow) == 0 && len(group.maxRow) == 0 {
		return true
	}
	if !r.InfiniteStart && r.Start != nil && group.maxRow != nil &&
		bytes.Compare(group.maxRow, r.Start.Row) < 0 {
		return false
	}
	if !r.InfiniteEnd && r.End != nil && group.minRow != nil &&
		bytes.Compare(group.minRow, r.End.Row) > 0 {
		return false
	}
	return true
}

func singleRow(r iterrt.Range) ([]byte, bool) {
	if r.InfiniteStart || r.InfiniteEnd || r.Start == nil || r.End == nil ||
		!r.StartInclusive || r.EndInclusive {
		return nil, false
	}
	if len(r.End.Row) != len(r.Start.Row)+1 || r.End.Row[len(r.End.Row)-1] != 0 ||
		!bytes.Equal(r.End.Row[:len(r.Start.Row)], r.Start.Row) {
		return nil, false
	}
	return r.Start.Row, true
}

func cfAllowed(cf []byte, families [][]byte, inclusive bool) bool {
	if len(families) == 0 {
		return !inclusive
	}
	found := false
	for _, family := range families {
		if bytes.Equal(cf, family) {
			found = true
			break
		}
	}
	if inclusive {
		return found
	}
	return !found
}

func (s *Source) HasTop() bool           { return s.hasTop }
func (s *Source) GetTopKey() *iterrt.Key { return s.topKey }
func (s *Source) GetTopValue() []byte    { return s.topValue }
func (s *Source) Stats() ReadStats       { return s.stats }

func (s *Source) DeepCopy(env iterrt.IteratorEnvironment) iterrt.SortedKeyValueIterator {
	if s.opener == nil {
		panic("parquet: Source.DeepCopy with nil opener")
	}
	file, err := s.opener()
	if err != nil {
		panic(fmt.Sprintf("parquet: Source.DeepCopy reopen failed: %v", err))
	}
	copy, err := NewSource(file, s.opener)
	if err != nil {
		panic(fmt.Sprintf("parquet: Source.DeepCopy open failed: %v", err))
	}
	_ = copy.Init(nil, nil, env)
	return copy
}

func (s *Source) Close() error {
	s.closeReader()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

func (s *Source) closeReader() {
	if s.reader != nil {
		_ = s.reader.Close()
		s.reader = nil
	}
}

func (s *Source) clearTop() {
	s.topKey = nil
	s.topValue = nil
	s.hasTop = false
}
