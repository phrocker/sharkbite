package accumulo

import (
	"errors"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// MutationLatestTimestamp requests a tablet-server-assigned timestamp.
const MutationLatestTimestamp = cclient.MutationLatestTimestamp

// Mutation is an ordered collection of puts and deletes for one row.
//
// Mutation is not safe for concurrent modification.
type Mutation struct {
	mutation *cclient.Mutation
}

// NewMutation constructs a mutation for a non-empty row.
func NewMutation(row []byte) (*Mutation, error) {
	if len(row) == 0 {
		return nil, errors.New("accumulo: mutation row must be non-empty")
	}
	mutation, err := cclient.NewMutation(row)
	if err != nil {
		return nil, err
	}
	return &Mutation{mutation: mutation}, nil
}

// Row returns a copy of the mutation row.
func (m *Mutation) Row() []byte {
	return cloneRow(m.mutation.Row())
}

// Size returns the number of column updates.
func (m *Mutation) Size() int {
	return m.mutation.Size()
}

// Put adds a value with an explicit timestamp.
func (m *Mutation) Put(
	columnFamily, columnQualifier, columnVisibility []byte,
	timestamp int64,
	value []byte,
) {
	m.mutation.Put(columnFamily, columnQualifier, columnVisibility, timestamp, value)
}

// PutLatest adds a value whose timestamp will be assigned by the tablet server.
func (m *Mutation) PutLatest(
	columnFamily, columnQualifier, columnVisibility, value []byte,
) {
	m.mutation.PutLatest(columnFamily, columnQualifier, columnVisibility, value)
}

// Delete adds a tombstone with an explicit timestamp.
func (m *Mutation) Delete(
	columnFamily, columnQualifier, columnVisibility []byte,
	timestamp int64,
) {
	m.mutation.Delete(columnFamily, columnQualifier, columnVisibility, timestamp)
}

// DeleteLatest adds a tombstone whose timestamp will be assigned by the tablet server.
func (m *Mutation) DeleteLatest(columnFamily, columnQualifier, columnVisibility []byte) {
	m.mutation.DeleteLatest(columnFamily, columnQualifier, columnVisibility)
}

func (m *Mutation) toThrift() (*data.TMutation, error) {
	if m == nil || m.mutation == nil {
		return nil, errors.New("accumulo: mutation is nil")
	}
	return m.mutation.ToThrift()
}
