package scanserver

import (
	"context"
	"errors"
	"math"

	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
)

// ReadConditionalRow reads the immutable portion of one row. Hosted tablets
// overlay their active cells while holding the tablet operation lock.
func (s *Server) ReadConditionalRow(
	ctx context.Context,
	credentials *security.TCredentials,
	extent ingestrouter.Extent,
	row []byte,
	authorizations [][]byte,
) ([]ingestrouter.Cell, error) {
	wireExtent := &data.TKeyExtent{
		Table:      append([]byte(nil), extent.TableID...),
		EndRow:     append([]byte(nil), extent.EndRow...),
		PrevEndRow: append([]byte(nil), extent.PrevEndRow...),
	}
	result, err := s.StartScan(
		ctx, client.NewTInfo(), credentials, wireExtent,
		&data.TRange{
			Start: &data.TKey{Row: append([]byte(nil), row...), Timestamp: math.MaxInt64},
			Stop: &data.TKey{
				Row:       append(append([]byte(nil), row...), 0),
				Timestamp: math.MaxInt64,
			},
			StartKeyInclusive: true,
			StopKeyInclusive:  false,
		},
		nil, 0, nil, nil, authorizations, false, false, 0, nil, 0, "", nil, 0,
	)
	if err != nil {
		return nil, err
	}
	cells := make([]ingestrouter.Cell, 0)
	for {
		if result == nil || result.Result_ == nil {
			return nil, errors.New("scanserver: conditional row scan returned no result")
		}
		for _, kv := range result.Result_.Results {
			if kv == nil || kv.Key == nil {
				continue
			}
			cells = append(cells, ingestrouter.Cell{
				Row:              append([]byte(nil), kv.Key.Row...),
				ColumnFamily:     append([]byte(nil), kv.Key.ColFamily...),
				ColumnQualifier:  append([]byte(nil), kv.Key.ColQualifier...),
				ColumnVisibility: append([]byte(nil), kv.Key.ColVisibility...),
				Timestamp:        kv.Key.Timestamp,
				Value:            append([]byte(nil), kv.Value...),
			})
		}
		if !result.Result_.More {
			return cells, nil
		}
		next, err := s.ContinueScan(ctx, client.NewTInfo(), result.ScanID, 0)
		if err != nil {
			return nil, err
		}
		result.Result_ = next
	}
}
