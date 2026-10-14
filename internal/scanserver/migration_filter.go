package scanserver

import (
	"bytes"
	"strings"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

type metadataColumnFilter struct {
	columnFamily    string
	columnQualifier string
	currentRow      []byte
	current         []wholeRowCell
	matched         bool
	results         []*data.TKeyValue
}

func newMetadataColumnFilter(columnFamily, columnQualifier string) *metadataColumnFilter {
	return &metadataColumnFilter{
		columnFamily: columnFamily, columnQualifier: columnQualifier,
	}
}

func (p *metadataColumnFilter) offer(key *wire.Key, value []byte) {
	if p.currentRow != nil && !bytes.Equal(p.currentRow, key.Row) {
		p.flush()
	}
	if p.currentRow == nil {
		p.currentRow = cloneBytes(key.Row)
	}
	if string(key.ColumnFamily) == p.columnFamily &&
		(p.columnQualifier == "" || string(key.ColumnQualifier) == p.columnQualifier) {
		p.matched = true
	}
	p.current = append(p.current, wholeRowCell{key: *key.Clone(), value: cloneBytes(value)})
}

func (p *metadataColumnFilter) drain() []*data.TKeyValue {
	p.flush()
	results := p.results
	p.results = nil
	return results
}

func (p *metadataColumnFilter) err() error { return nil }

func (p *metadataColumnFilter) flush() {
	if p.matched {
		for _, cell := range p.current {
			p.results = append(p.results, thriftKeyValue(&cell.key, cell.value))
		}
	}
	p.currentRow = nil
	p.current = nil
	p.matched = false
}

func thriftKeyValue(key *wire.Key, value []byte) *data.TKeyValue {
	return &data.TKeyValue{
		Key: &data.TKey{
			Row:           cloneBytes(key.Row),
			ColFamily:     cloneBytes(key.ColumnFamily),
			ColQualifier:  cloneBytes(key.ColumnQualifier),
			ColVisibility: cloneBytes(key.ColumnVisibility),
			Timestamp:     key.Timestamp,
		},
		Value: cloneBytes(value),
	}
}

type gcWalsFilter struct {
	liveTservers map[string]struct{}
	currentRow   []byte
	current      []wholeRowCell
	hasLog       bool
	location     string
	results      []*data.TKeyValue
}

func newGcWalsFilter(encodedLiveTservers string) *gcWalsFilter {
	liveTservers := make(map[string]struct{})
	if encodedLiveTservers != "" {
		for _, server := range strings.Split(encodedLiveTservers, ",") {
			liveTservers[server] = struct{}{}
		}
	}
	return &gcWalsFilter{liveTservers: liveTservers}
}

func (p *gcWalsFilter) offer(key *wire.Key, value []byte) {
	if p.currentRow != nil && !bytes.Equal(p.currentRow, key.Row) {
		p.flush()
	}
	if p.currentRow == nil {
		p.currentRow = cloneBytes(key.Row)
	}
	switch string(key.ColumnFamily) {
	case metadata.CFLog:
		p.hasLog = true
	case metadata.CFCurrentLocation, metadata.CFFutureLocation:
		p.location = string(value) + "[" + string(key.ColumnQualifier) + "]"
	}
	p.current = append(p.current, wholeRowCell{key: *key.Clone(), value: cloneBytes(value)})
}

func (p *gcWalsFilter) drain() []*data.TKeyValue {
	p.flush()
	results := p.results
	p.results = nil
	return results
}

func (p *gcWalsFilter) err() error { return nil }

func (p *gcWalsFilter) flush() {
	_, locationIsLive := p.liveTservers[p.location]
	if p.hasLog || (p.location != "" && !locationIsLive) {
		for _, cell := range p.current {
			p.results = append(p.results, thriftKeyValue(&cell.key, cell.value))
		}
	}
	p.currentRow = nil
	p.current = nil
	p.hasLog = false
	p.location = ""
}
