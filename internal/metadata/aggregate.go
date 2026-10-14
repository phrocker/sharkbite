package metadata

import (
	"bytes"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// AggregateRows folds a sorted KV stream from the metadata table into one
// TabletInfo per row. Tablets in metadata are a single row each, with
// multiple columns — file:<path>, loc:<lockID>, ~tab:~pr — so we group on
// row-change. Caller is responsible for ensuring kvs are sorted by row.
//
// This is the pure aggregation logic; the Walker uses it after pulling
// kvs off the wire.
//
// IMPORTANT: Accumulo's Thrift scan results use **relative-key
// compression** — a TKeyValue with empty Row/ColFamily/ColQualifier means
// "same as previous". We materialize each KV against the running cursor
// before grouping. Reference: sharkbite ThriftWrapper.h:251-303.
func AggregateRows(kvs []*data.TKeyValue) ([]TabletInfo, error) {
	if len(kvs) == 0 {
		return nil, nil
	}

	var (
		out     []TabletInfo
		current TabletInfo
		curRow  []byte
		started bool

		// Relative-key cursor: each TKeyValue inherits Row/CF/CQ/CV from
		// the prior one when its corresponding field is empty.
		prevRow, prevCF, prevCQ, prevCV []byte
	)

	flush := func() error {
		if !started {
			return nil
		}
		// Skip rows we filtered (system rows). They contributed nothing
		// to `current` so emitting would yield an empty tablet.
		if !isTabletRow(curRow) {
			return nil
		}
		tableID, endRow, err := DecodeTabletRow(curRow)
		if err != nil {
			return fmt.Errorf("row %q: %w", curRow, err)
		}
		if err := finalizeFileEmbeddings(&current); err != nil {
			return fmt.Errorf("row %q: %w", curRow, err)
		}
		current.TableID = tableID
		current.EndRow = endRow
		out = append(out, current)
		return nil
	}

	for i, kv := range kvs {
		if kv == nil || kv.Key == nil {
			return nil, fmt.Errorf("nil KeyValue or Key in metadata stream")
		}
		// Materialize the cursor against the previous KV. The first KV
		// must have all fields populated; subsequent KVs may have empty
		// fields meaning "inherit".
		row := kv.Key.Row
		if len(row) == 0 {
			if i == 0 {
				return nil, fmt.Errorf("first KV in stream has empty row (no prior to inherit from)")
			}
			row = prevRow
		}
		cf := kv.Key.ColFamily
		if len(cf) == 0 && i > 0 {
			cf = prevCF
		}
		cq := kv.Key.ColQualifier
		if len(cq) == 0 && i > 0 {
			cq = prevCQ
		}
		cv := kv.Key.ColVisibility
		if len(cv) == 0 && i > 0 {
			cv = prevCV
		}

		if !started || !bytes.Equal(row, curRow) {
			if err := flush(); err != nil {
				return nil, err
			}
			current = TabletInfo{}
			curRow = row
			started = true
		}
		// Skip system rows that aren't tablet entries: deletion markers
		// (~del...), bulk-load progress (~blip...), and any other ~-prefix
		// system row. Tablet rows always start with the tableID, never `~`.
		// We still update prevRow/prevCF/etc. so relative-key inheritance
		// stays accurate for the next real row.
		if !isTabletRow(row) {
			prevRow, prevCF, prevCQ, prevCV = row, cf, cq, cv
			continue
		}
		// Build a materialized view of the column for applyColumn so it
		// doesn't have to know about inheritance.
		materialized := &data.TKeyValue{
			Key: &data.TKey{
				Row:           row,
				ColFamily:     cf,
				ColQualifier:  cq,
				ColVisibility: cv,
				Timestamp:     kv.Key.Timestamp,
			},
			Value: kv.Value,
		}
		if err := applyColumn(&current, materialized); err != nil {
			return nil, fmt.Errorf("row %q: %w", row, err)
		}
		prevRow, prevCF, prevCQ, prevCV = row, cf, cq, cv
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

func applyColumn(t *TabletInfo, kv *data.TKeyValue) error {
	cf := string(kv.Key.ColFamily)
	cq := kv.Key.ColQualifier
	val := kv.Value

	switch cf {
	case CFFile:
		stf, err := DecodeStoredTabletFile(cq)
		if err != nil {
			return fmt.Errorf("file qualifier: %w", err)
		}
		size, ne, time, err := DecodeDataFileValue(val)
		if err != nil {
			return fmt.Errorf("file:%s: %w", stf.Path, err)
		}
		// Copy cq so the FileEntry doesn't alias the Thrift buffer.
		raw := append([]byte(nil), cq...)
		// No file.embedding column has been decoded for this entry yet.
		// Absence of the column is absence of information, not a
		// statement that the file holds no vectors, so the entry starts
		// out unknown and only an explicitly decoded column may move it
		// to a positive claim.
		embedding := embeddingspace.Unknown()
		var rawEmbedding []byte
		if t.fileEmbeddings != nil {
			if decoded, ok := t.fileEmbeddings[string(raw)]; ok {
				embedding = decoded.state
				rawEmbedding = decoded.raw
			}
		}
		t.Files = append(t.Files, FileEntry{
			Path:         stf.Path,
			StartRow:     stf.StartRow,
			EndRow:       stf.EndRow,
			Size:         size,
			NumEntries:   ne,
			Time:         time,
			Embedding:    embedding,
			RawQualifier: raw,
			RawValue:     append([]byte(nil), val...),
			RawEmbedding: rawEmbedding,
		})
	case CFFileEmbedding:
		state, err := embeddingspace.Decode(val)
		if err != nil {
			return fmt.Errorf("file embedding:%q: %w", cq, err)
		}
		raw := append([]byte(nil), cq...)
		rawValue := append([]byte(nil), val...)
		if t.fileEmbeddings == nil {
			t.fileEmbeddings = make(map[string]decodedEmbedding)
		}
		t.fileEmbeddings[string(raw)] = decodedEmbedding{state: state, raw: rawValue}
		for i := range t.Files {
			if bytes.Equal(t.Files[i].RawQualifier, raw) {
				t.Files[i].Embedding = state
				t.Files[i].RawEmbedding = rawValue
			}
		}
	case CFCurrentLocation:
		// Qualifier = serialized lockID; value = host:port.
		// We keep the raw qualifier as Session — that's what the server
		// uses to verify it's still the holder. We could decode and re-
		// serialize, but the raw form round-trips faithfully and avoids
		// reserialization-format drift.
		if t.Location != nil {
			return fmt.Errorf("multiple loc: entries")
		}
		t.Location = &Location{
			HostPort: string(val),
			Session:  string(cq),
		}
	case CFFutureLocation:
		if t.FutureLocation != nil {
			return fmt.Errorf("multiple future: entries")
		}
		t.FutureLocation = &Location{
			HostPort: string(val),
			Session:  string(cq),
		}
	case CFLog:
		log, err := DecodeLogEntry(cq)
		if err != nil {
			return fmt.Errorf("log qualifier: %w", err)
		}
		t.Logs = append(t.Logs, log)
	case CFServer:
		switch string(cq) {
		case CQDirectory:
			if t.Directory != "" {
				return fmt.Errorf("multiple srv:dir entries")
			}
			t.Directory = string(val)
		case CQTime:
			if t.Time != "" {
				return fmt.Errorf("multiple srv:time entries")
			}
			t.Time = string(val)
		case CQLock:
			if t.ServerLock != "" {
				return fmt.Errorf("multiple srv:lock entries")
			}
			t.ServerLock = string(val)
		}
	case CFTabletSection:
		if string(cq) == CQPrevRow {
			if t.PrevRowSet {
				return fmt.Errorf("multiple ~tab:~pr entries")
			}
			t.PrevRow = decodePrevRow(val)
			t.PrevRowSet = true
		}
		// Other ~tab qualifiers (split-ratio, dir, etc.) ignored in V0.
	default:
		// Unknown CF — silently ignore. Forward-compat with newer Accumulo
		// columns we don't care about for read-fleet purposes.
	}
	return nil
}

func finalizeFileEmbeddings(t *TabletInfo) error {
	if t == nil {
		return nil
	}
	for i := range t.Files {
		if t.Files[i].Embedding.State == "" {
			// A tablet whose file entry never met a file.embedding
			// column tells us nothing about that file's vectors.
			// Reporting no_embeddings here would manufacture the one
			// claim that merges freely with every space.
			t.Files[i].Embedding = embeddingspace.Unknown()
		}
	}
	for qualifier := range t.fileEmbeddings {
		found := false
		for _, file := range t.Files {
			if string(file.RawQualifier) == qualifier {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("orphan %s:%s column", CFFileEmbedding, qualifier)
		}
	}
	t.fileEmbeddings = nil
	return nil
}

// isTabletRow returns false for the system rows that share the metadata
// + root tablets with real tablet entries: deletion markers (~del...),
// bulk-load progress (~blip...), and any other ~-prefixed system row.
// Real tablet rows always start with the tableID character (digits,
// letters, '+', '!'), never '~'.
func isTabletRow(row []byte) bool {
	return len(row) > 0 && row[0] != '~'
}

// decodePrevRow strips the leading sentinel byte that
// TabletColumnFamily.PREV_ROW_COLUMN uses to encode "no prev row".
//
// Authoritative encoding per Java MetadataSchema.encodePrevEndRow:
//
//	null prev (first tablet of table) → single byte 0x00
//	present prev (any bytes incl empty) → 0x01 + bytes
//
// Java decoder: `if (value[0] != 0) per = bytes[1:]` — i.e. byte0 != 0
// means PRESENT, byte0 == 0 means ABSENT/nil. Surfaced in production
// when shoal-bootstrap metadata walks sent PrevEndRow=[]byte{} for a
// metadata tablet whose ~pr value was the single byte 0x00 = nil. The
// tserver rejected with NotServingTabletException because the actual
// tablet has prevRow=nil and Java treats nil != []byte{} for tablet-
// extent matching.
//
// Reference: core/.../metadata/schema/MetadataSchema.java:165-184.
func decodePrevRow(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	if value[0] == 0x00 {
		// Absent: first tablet of table, no prev row.
		return nil
	}
	// Present: byte[0] is the 0x01 marker, rest is the prev row bytes
	// (which may legitimately be empty after the marker — meaning
	// "prev row is the empty row").
	if len(value) == 1 {
		return []byte{}
	}
	out := make([]byte, len(value)-1)
	copy(out, value[1:])
	return out
}
