package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/parquet-go/parquet-go"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/parquetfile"
)

type scanCursor interface {
	Next() bool
	Key() *iterrt.Key
	Value() []byte
	Advance() error
}

type scanOutput interface {
	Write(*iterrt.Key, []byte) error
	Close() error
}

const parquetRowsPerRowGroup = 64 * 1024

func newScanOutput(format string, output io.Writer) (scanOutput, error) {
	switch strings.ToLower(format) {
	case "json":
		return &jsonScanOutput{encoder: json.NewEncoder(output)}, nil
	case "parquet":
		return &parquetScanOutput{writer: parquet.NewGenericWriter[parquetfile.Cell](
			output,
			parquet.MaxRowsPerRowGroup(parquetRowsPerRowGroup),
		)}, nil
	default:
		return nil, fmt.Errorf("unknown output format %q (want json or parquet)", format)
	}
}

func writeScan(cursor scanCursor, output scanOutput, limit int) (count int, err error) {
	defer func() {
		if closeErr := output.Close(); err == nil {
			err = closeErr
		}
	}()

	for cursor.Next() {
		if err := output.Write(cursor.Key(), cursor.Value()); err != nil {
			return count, fmt.Errorf("write output: %w", err)
		}
		if err := cursor.Advance(); err != nil {
			return count, fmt.Errorf("advance: %w", err)
		}
		count++
		if limit > 0 && count >= limit {
			break
		}
	}
	return count, nil
}

type jsonScanOutput struct {
	encoder *json.Encoder
}

func (o *jsonScanOutput) Write(key *iterrt.Key, value []byte) error {
	return o.encoder.Encode(CellJSON{
		Row:     string(key.Row),
		CF:      string(key.ColumnFamily),
		CQ:      string(key.ColumnQualifier),
		CV:      string(key.ColumnVisibility),
		TS:      key.Timestamp,
		Val:     string(value),
		Deleted: key.Deleted,
	})
}

func (*jsonScanOutput) Close() error { return nil }

type parquetScanOutput struct {
	writer *parquet.GenericWriter[parquetfile.Cell]
}

func (o *parquetScanOutput) Write(key *iterrt.Key, value []byte) error {
	_, err := o.writer.Write([]parquetfile.Cell{{
		Row:       key.Row,
		CF:        key.ColumnFamily,
		CQ:        key.ColumnQualifier,
		CV:        key.ColumnVisibility,
		Timestamp: key.Timestamp,
		Deleted:   key.Deleted,
		Value:     value,
	}})
	return err
}

func (o *parquetScanOutput) Close() error { return o.writer.Close() }
