package parquetfile

import (
	"bytes"
	"fmt"
	"io"

	"github.com/parquet-go/parquet-go"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

const DefaultRowsPerRowGroup = 8192

type EncodeOptions struct {
	RowsPerRowGroup int64
	PageBufferSize  int
	Check           func() error
	Observe         func(int64)
	EmbeddingSpace  embeddingspace.FileState
}

// Cell is the Parquet representation of one Accumulo key/value entry.
// Every key component remains binary so arbitrary Accumulo keys round-trip.
type Cell struct {
	Row       []byte `parquet:"row"`
	CF        []byte `parquet:"cf"`
	CQ        []byte `parquet:"cq"`
	CV        []byte `parquet:"cv"`
	Timestamp int64  `parquet:"timestamp"`
	Deleted   bool   `parquet:"deleted"`
	Value     []byte `parquet:"value"`
}

// Encode drains an already-seeked, sorted iterator into a Parquet image.
func Encode(iter iterrt.SortedKeyValueIterator) ([]byte, int64, error) {
	return EncodeWithOptions(iter, EncodeOptions{})
}

// EncodeWithOptions drains an already-seeked, sorted iterator into a Parquet
// image with row-group statistics and a row bloom filter for scan pruning.
func EncodeWithOptions(iter iterrt.SortedKeyValueIterator, opts EncodeOptions) ([]byte, int64, error) {
	var buf bytes.Buffer
	count, err := EncodeToWithOptions(&buf, iter, opts)
	if err != nil {
		return nil, count, err
	}
	return buf.Bytes(), count, nil
}

// EncodeToWithOptions drains an already-seeked, sorted iterator into w.
func EncodeToWithOptions(w io.Writer, iter iterrt.SortedKeyValueIterator, opts EncodeOptions) (int64, error) {
	rowsPerGroup := opts.RowsPerRowGroup
	if rowsPerGroup <= 0 {
		rowsPerGroup = DefaultRowsPerRowGroup
	}
	pageBufferSize := opts.PageBufferSize
	if pageBufferSize <= 0 {
		pageBufferSize = 64 << 10
	}
	writerOptions := []parquet.WriterOption{
		parquet.MaxRowsPerRowGroup(rowsPerGroup),
		parquet.PageBufferSize(pageBufferSize),
		parquet.BloomFilters(parquet.SplitBlockFilter(10, "row")),
		parquet.SortingWriterConfig(parquet.SortingColumns(
			parquet.Ascending("row"),
			parquet.Ascending("cf"),
			parquet.Ascending("cq"),
			parquet.Ascending("cv"),
			parquet.Descending("timestamp"),
			parquet.Descending("deleted"),
		)),
	}
	if opts.EmbeddingSpace.State != "" {
		encoded, err := embeddingspace.Encode(opts.EmbeddingSpace)
		if err != nil {
			return 0, fmt.Errorf("parquet: encode embedding metadata: %w", err)
		}
		writerOptions = append(writerOptions,
			parquet.KeyValueMetadata(embeddingspace.ParquetMetadataKey, string(encoded)))
	}
	writer := parquet.NewGenericWriter[Cell](w, writerOptions...)
	var count int64
	batch := make([]Cell, 0, 512)
	writeBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		if _, err := writer.Write(batch); err != nil {
			return fmt.Errorf("parquet: write cells at %d: %w", count-int64(len(batch)), err)
		}
		batch = batch[:0]
		return nil
	}
	for iter.HasTop() {
		if opts.Check != nil {
			if err := opts.Check(); err != nil {
				return count, err
			}
		}
		k := iter.GetTopKey()
		batch = append(batch, Cell{
			Row:       bytes.Clone(k.Row),
			CF:        bytes.Clone(k.ColumnFamily),
			CQ:        bytes.Clone(k.ColumnQualifier),
			CV:        bytes.Clone(k.ColumnVisibility),
			Timestamp: k.Timestamp,
			Deleted:   k.Deleted,
			Value:     bytes.Clone(iter.GetTopValue()),
		})
		count++
		if opts.Observe != nil {
			opts.Observe(count)
		}
		if len(batch) == cap(batch) {
			if err := writeBatch(); err != nil {
				return count, err
			}
			if opts.Check != nil {
				if err := opts.Check(); err != nil {
					return count, err
				}
			}
		}
		if err := iter.Next(); err != nil {
			return count, fmt.Errorf("parquet: advance after cell %d: %w", count-1, err)
		}
	}
	if err := writeBatch(); err != nil {
		return count, err
	}
	if err := writer.Close(); err != nil {
		return count, fmt.Errorf("parquet: close writer: %w", err)
	}
	return count, nil
}

func ReadEmbeddingSpaceMetadata(src io.ReaderAt, size int64) (embeddingspace.FileState, error) {
	file, err := parquet.OpenFile(src, size, parquet.ReadBufferSize(64<<10))
	if err != nil {
		return embeddingspace.FileState{}, fmt.Errorf("parquet: open file: %w", err)
	}
	value, ok := file.Lookup(embeddingspace.ParquetMetadataKey)
	if !ok {
		return embeddingspace.Unknown(), nil
	}
	state, err := embeddingspace.Decode([]byte(value))
	if err != nil {
		return embeddingspace.FileState{}, fmt.Errorf("parquet: parse embedding metadata: %w", err)
	}
	return state, nil
}

// Decode reads a Parquet image into an immutable sorted cell slice.
func Decode(data []byte) ([]iterrt.Cell, error) {
	rows, err := parquet.Read[Cell](bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("parquet: read: %w", err)
	}
	cells := make([]iterrt.Cell, len(rows))
	for i := range rows {
		row := &rows[i]
		cells[i] = iterrt.Cell{
			Key: &wire.Key{
				Row:              row.Row,
				ColumnFamily:     row.CF,
				ColumnQualifier:  row.CQ,
				ColumnVisibility: row.CV,
				Timestamp:        row.Timestamp,
				Deleted:          row.Deleted,
			},
			Value: row.Value,
		}
		if i > 0 && cells[i-1].Key.Compare(cells[i].Key) > 0 {
			return nil, fmt.Errorf("parquet: cells are not sorted at row %d", i)
		}
	}
	return cells, nil
}

// Validate verifies the Parquet structure, decodes every row, and enforces
// Shoal's sorted-key invariant. validateKey may impose additional constraints.
func Validate(src io.ReaderAt, size int64, validateKey func(*wire.Key) error) error {
	if size < 0 {
		return fmt.Errorf("parquet: negative size %d", size)
	}
	file, err := parquet.OpenFile(src, size, parquet.ReadBufferSize(64<<10))
	if err != nil {
		return fmt.Errorf("parquet: open: %w", err)
	}
	var previous *wire.Key
	var row int64
	batch := make([]Cell, 256)
	for groupIndex, group := range file.RowGroups() {
		reader := parquet.NewGenericRowGroupReader[Cell](group)
		for {
			n, readErr := reader.Read(batch)
			for i := 0; i < n; i++ {
				key := parquetCellKey(&batch[i])
				if previous != nil && previous.Compare(key) > 0 {
					_ = reader.Close()
					return fmt.Errorf("parquet: cells are not sorted at row %d", row)
				}
				if validateKey != nil {
					if err := validateKey(key); err != nil {
						_ = reader.Close()
						return fmt.Errorf("parquet: validate row %d: %w", row, err)
					}
				}
				previous = key.Clone()
				row++
			}
			if readErr != nil {
				if readErr != io.EOF {
					_ = reader.Close()
					return fmt.Errorf("parquet: read row group %d: %w", groupIndex, readErr)
				}
				break
			}
			if n == 0 {
				_ = reader.Close()
				return fmt.Errorf("parquet: read row group %d: %w", groupIndex, io.ErrNoProgress)
			}
		}
		if err := reader.Close(); err != nil {
			return fmt.Errorf("parquet: close row group %d: %w", groupIndex, err)
		}
	}
	return nil
}

func parquetCellKey(row *Cell) *wire.Key {
	return &wire.Key{
		Row:              row.Row,
		ColumnFamily:     row.CF,
		ColumnQualifier:  row.CQ,
		ColumnVisibility: row.CV,
		Timestamp:        row.Timestamp,
		Deleted:          row.Deleted,
	}
}
