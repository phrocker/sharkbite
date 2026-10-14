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

// logfile.go ports the on-disk WAL entry format so the qwal reader yields
// decoded (LogFileKey, LogFileValue) pairs rather than raw bytes.
//
// Java references — every field is cross-checked against these:
//   - DfsLogger.java          file header / crypto framing (LOG_FILE_HEADER_V4)
//   - LogFileKey.java         readFields(DataInput) — the key wire format
//   - LogFileValue.java       readFields(DataInput) — count-prefixed mutations
//   - LogEvents.java          event enum ordinal order (DO NOT REORDER)
//   - Mutation.java           readFields + deserializeColumnUpdate
//   - ServerMutation.java     trailing VLong systemTime in VERSION2 format
//   - KeyExtent.java          readFrom(DataInput)
package qwal

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// logFileHeaderV4 is the magic string DfsLogger writes at the head of every
// v4 WAL file (DfsLogger.LOG_FILE_HEADER_V4). v3 ("--- Log File Header (v3)
// ---") is Accumulo 1.9-era and only ever appears uncrypted; we detect it
// but the qwal read fleet targets v4 clusters.
const (
	logFileHeaderV4 = "--- Log File Header (v4) ---"
	logFileHeaderV3 = "--- Log File Header (v3) ---"
)

// LogEvent is the WAL entry type. Ordinal order is load-bearing — it must
// match LogEvents.java exactly (the enum is serialized as its ordinal).
type LogEvent int

const (
	EventOpen LogEvent = iota
	EventDefineTablet
	EventMutation
	EventManyMutations
	EventCompactionStart
	EventCompactionFinish
)

func (e LogEvent) String() string {
	switch e {
	case EventOpen:
		return "OPEN"
	case EventDefineTablet:
		return "DEFINE_TABLET"
	case EventMutation:
		return "MUTATION"
	case EventManyMutations:
		return "MANY_MUTATIONS"
	case EventCompactionStart:
		return "COMPACTION_START"
	case EventCompactionFinish:
		return "COMPACTION_FINISH"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", int(e))
	}
}

// logEventCount is the number of known events; ordinals at or above this are
// rejected (mirrors LogFileKey.readFields' bounds check).
const logEventCount = 6

// logFileKeyVersion is LogFileKey.VERSION — the int written immediately after
// an OPEN event's ordinal. A mismatch means an incompatible WAL format.
const logFileKeyVersion = 2

// ErrEncryptedWAL is returned when a WAL carries a non-empty crypto parameter
// block. Full WAL decryption is out of scope for W0; the reader detects it
// and refuses rather than silently mis-parsing ciphertext as entries.
var ErrEncryptedWAL = errors.New("qwal: WAL segment is encrypted; decryption is out of scope for W0")

// ErrHeaderIncomplete is returned when the file ends before the header is
// fully read — the tserver died mid-write before flushing the header. Mirrors
// DfsLogger.LogHeaderIncompleteException.
var ErrHeaderIncomplete = errors.New("qwal: WAL header incomplete (writer died before header flush)")

// KeyExtent is the tablet identity carried by a DEFINE_TABLET entry. It is a
// thin local mirror of cclient.KeyExtent's fields — KeyExtent.readFrom emits
// Hadoop Text (VInt-length-prefixed) fields, not the Thrift form.
type KeyExtent struct {
	TableID    string
	EndRow     []byte
	PrevEndRow []byte
}

// LogFileKey is the decoded key half of a WAL entry. Only the fields relevant
// to a given event are populated (see LogFileKey.readFields' switch).
type LogFileKey struct {
	Event LogEvent
	// TabletID is the per-WAL tablet identifier (set for all events except
	// OPEN, where the int slot instead carries the format version).
	TabletID int32
	// Seq is the per-tablet mutation sequence number (-1 when not applicable).
	Seq int64
	// TServerSession is set only for OPEN.
	TServerSession string
	// Filename is set only for COMPACTION_START.
	Filename string
	// Tablet is set only for DEFINE_TABLET.
	Tablet *KeyExtent
}

// LogFileValue is the decoded value half: the list of mutations carried by a
// MUTATION (exactly one) or MANY_MUTATIONS (zero or more) entry. Other event
// types carry an empty value frame.
type LogFileValue struct {
	Mutations []*cclient.Mutation
}

// Entry pairs a decoded key and value as surfaced by the reader stream.
type Entry struct {
	Key   LogFileKey
	Value LogFileValue
}

// maxMutationsPerRecord mirrors LogFileValue.MAX_MUTATIONS_PER_RECORD: a
// single record claiming more than this is almost certainly a corrupt frame;
// pre-allocating at the corrupt count would OOM the reader.
const maxMutationsPerRecord = 1_000_000

// dataInput is a Java-DataInput-style cursor over a byte stream. It satisfies
// wire.ByteAndReader (io.Reader + io.ByteReader) so it can drive the shared
// VInt/VLong helpers in internal/rfile/wire.
type dataInput struct {
	r *bufio.Reader
}

func newDataInput(r io.Reader) *dataInput {
	return &dataInput{r: bufio.NewReader(r)}
}

func (d *dataInput) Read(p []byte) (int, error) { return d.r.Read(p) }
func (d *dataInput) ReadByte() (byte, error)    { return d.r.ReadByte() }

func (d *dataInput) readFull(p []byte) error {
	_, err := io.ReadFull(d.r, p)
	return err
}

func (d *dataInput) readU8() (byte, error) { return d.r.ReadByte() }

func (d *dataInput) readBool() (bool, error) {
	b, err := d.r.ReadByte()
	return b != 0, err
}

func (d *dataInput) readInt32() (int32, error) {
	var buf [4]byte
	if err := d.readFull(buf[:]); err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(buf[:])), nil
}

func (d *dataInput) readInt64() (int64, error) {
	var buf [8]byte
	if err := d.readFull(buf[:]); err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(buf[:])), nil
}

// readUTF decodes a Java DataOutput.writeUTF string: 2-byte unsigned
// big-endian length + that many (modified) UTF-8 bytes.
func (d *dataInput) readUTF() (string, error) {
	var lenBuf [2]byte
	if err := d.readFull(lenBuf[:]); err != nil {
		return "", err
	}
	n := binary.BigEndian.Uint16(lenBuf[:])
	if n == 0 {
		return "", nil
	}
	body := make([]byte, n)
	if err := d.readFull(body); err != nil {
		return "", err
	}
	return string(body), nil
}

// readVInt / readVLong delegate to the shared Hadoop varint helpers.
func (d *dataInput) readVInt() (int32, error) {
	v, _, err := wire.ReadVInt(d)
	return v, err
}

func (d *dataInput) readVLong() (int64, error) {
	v, _, err := wire.ReadVLong(d)
	return v, err
}

// readText decodes a Hadoop Text: a VInt length prefix followed by that many
// raw bytes. Mirrors Text.readFields, which KeyExtent.readFrom relies on.
func (d *dataInput) readText() ([]byte, error) {
	n, err := d.readVInt()
	if err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, fmt.Errorf("qwal: negative Text length %d", n)
	}
	if n == 0 {
		return []byte{}, nil
	}
	body := make([]byte, n)
	if err := d.readFull(body); err != nil {
		return nil, err
	}
	return body, nil
}

// readHeader consumes the WAL file header. It returns nil once the cursor is
// positioned at the first entry, ErrEncryptedWAL if a crypto block is
// present, or ErrHeaderIncomplete on a short header.
//
// v4 layout (DfsLogger.getDecryptingStream + open()):
//
//	bytes  magic            ("--- Log File Header (v4) ---")
//	int32  cryptoParamsLen  (CryptoUtils.writeParams)
//	bytes  cryptoParams     (length above; empty/zero-length => no crypto)
//	... entries follow ...
func (d *dataInput) readHeader() error {
	magic := make([]byte, len(logFileHeaderV4))
	if err := d.readFull(magic); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrHeaderIncomplete
		}
		return fmt.Errorf("qwal: read WAL magic: %w", err)
	}
	switch string(magic) {
	case logFileHeaderV4:
		// CryptoUtils.writeParams: int32 length + that many param bytes.
		// The NoCryptoService writes a zero-length param block, so a 0 here
		// is the no-crypto case and we proceed straight to entries.
		paramLen, err := d.readInt32()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return ErrHeaderIncomplete
			}
			return fmt.Errorf("qwal: read crypto params length: %w", err)
		}
		if paramLen < 0 {
			return fmt.Errorf("qwal: negative crypto params length %d", paramLen)
		}
		if paramLen > 0 {
			return ErrEncryptedWAL
		}
		return nil
	case logFileHeaderV3:
		// v3 inlines a crypto-module classname string; only "NullCryptoModule"
		// was ever valid uncrypted. The qwal fleet targets v4 — surface v3 as
		// unsupported rather than guessing.
		return fmt.Errorf("qwal: WAL header v3 is not supported by the shoal reader")
	default:
		return fmt.Errorf("qwal: unrecognized WAL header magic %q", string(magic))
	}
}

// readKey decodes one LogFileKey. Direct port of LogFileKey.readFields. The
// event ordinal is bounds-checked exactly as the Java side does so a corrupt
// high-bit byte surfaces as a clean error instead of an index panic.
func (d *dataInput) readKey() (LogFileKey, error) {
	var k LogFileKey
	ev, err := d.readU8()
	if err != nil {
		return k, err // io.EOF here means clean end-of-stream; caller handles it.
	}
	if int(ev) >= logEventCount {
		return k, fmt.Errorf("qwal: invalid LogEvent ordinal %d (know %d types)", ev, logEventCount)
	}
	k.Event = LogEvent(ev)
	k.Seq = -1
	k.TabletID = -1

	switch k.Event {
	case EventOpen:
		// OPEN reuses the int32 slot for the format version, then a UTF
		// tserverSession string (LogFileKey.readFields).
		v, err := d.readInt32()
		if err != nil {
			return k, fmt.Errorf("qwal: OPEN version: %w", err)
		}
		if v != logFileKeyVersion {
			return k, fmt.Errorf("qwal: bad WAL version: expected %d, saw %d", logFileKeyVersion, v)
		}
		sess, err := d.readUTF()
		if err != nil {
			return k, fmt.Errorf("qwal: OPEN tserverSession: %w", err)
		}
		k.TServerSession = sess
	case EventCompactionFinish:
		if k.Seq, err = d.readInt64(); err != nil {
			return k, fmt.Errorf("qwal: COMPACTION_FINISH seq: %w", err)
		}
		if k.TabletID, err = d.readInt32(); err != nil {
			return k, fmt.Errorf("qwal: COMPACTION_FINISH tabletId: %w", err)
		}
	case EventCompactionStart:
		if k.Seq, err = d.readInt64(); err != nil {
			return k, fmt.Errorf("qwal: COMPACTION_START seq: %w", err)
		}
		if k.TabletID, err = d.readInt32(); err != nil {
			return k, fmt.Errorf("qwal: COMPACTION_START tabletId: %w", err)
		}
		if k.Filename, err = d.readUTF(); err != nil {
			return k, fmt.Errorf("qwal: COMPACTION_START filename: %w", err)
		}
	case EventDefineTablet:
		if k.Seq, err = d.readInt64(); err != nil {
			return k, fmt.Errorf("qwal: DEFINE_TABLET seq: %w", err)
		}
		if k.TabletID, err = d.readInt32(); err != nil {
			return k, fmt.Errorf("qwal: DEFINE_TABLET tabletId: %w", err)
		}
		ke, err := d.readKeyExtent()
		if err != nil {
			return k, fmt.Errorf("qwal: DEFINE_TABLET extent: %w", err)
		}
		k.Tablet = ke
	case EventManyMutations, EventMutation:
		if k.Seq, err = d.readInt64(); err != nil {
			return k, fmt.Errorf("qwal: %s seq: %w", k.Event, err)
		}
		if k.TabletID, err = d.readInt32(); err != nil {
			return k, fmt.Errorf("qwal: %s tabletId: %w", k.Event, err)
		}
	default:
		return k, fmt.Errorf("qwal: unknown log event %d", ev)
	}
	return k, nil
}

// readKeyExtent ports KeyExtent.readFrom: a Text tableId, then optional
// endRow/prevEndRow each guarded by a boolean.
func (d *dataInput) readKeyExtent() (*KeyExtent, error) {
	tid, err := d.readText()
	if err != nil {
		return nil, fmt.Errorf("tableId: %w", err)
	}
	ke := &KeyExtent{TableID: string(tid)}
	hasEnd, err := d.readBool()
	if err != nil {
		return nil, fmt.Errorf("hasEndRow: %w", err)
	}
	if hasEnd {
		if ke.EndRow, err = d.readText(); err != nil {
			return nil, fmt.Errorf("endRow: %w", err)
		}
	}
	hasPrev, err := d.readBool()
	if err != nil {
		return nil, fmt.Errorf("hasPrevEndRow: %w", err)
	}
	if hasPrev {
		if ke.PrevEndRow, err = d.readText(); err != nil {
			return nil, fmt.Errorf("prevEndRow: %w", err)
		}
	}
	return ke, nil
}

// readValue decodes one LogFileValue. Port of LogFileValue.readFields: an
// int32 mutation count followed by that many ServerMutation records.
func (d *dataInput) readValue() (LogFileValue, error) {
	var v LogFileValue
	count, err := d.readInt32()
	if err != nil {
		return v, fmt.Errorf("qwal: mutation count: %w", err)
	}
	if count < 0 || count > maxMutationsPerRecord {
		return v, fmt.Errorf("qwal: implausible mutation count %d (max %d); frame likely corrupt",
			count, maxMutationsPerRecord)
	}
	v.Mutations = make([]*cclient.Mutation, 0, count)
	for i := int32(0); i < count; i++ {
		m, err := d.readServerMutation()
		if err != nil {
			return v, fmt.Errorf("qwal: mutation %d: %w", i, err)
		}
		v.Mutations = append(v.Mutations, m)
	}
	return v, nil
}

// readServerMutation ports Mutation.readFields (VERSION2 path) plus
// ServerMutation's trailing systemTime VLong.
//
// VERSION2 layout (Mutation.write / readFields):
//
//	byte   first            high bit 0x80 set => VERSION2; 0x01 => values present
//	vint   rowLen ; bytes row
//	vint   dataLen ; bytes data        (column updates, decoded below)
//	vint   entries           number of column updates packed in `data`
//	[if 0x01] vint numValues ; for each: vint len ; bytes val   (large-value pool)
//	[if 0x02] vint numMutations ; for each: WritableUtils.readString  (legacy repl;
//	          0x02 is no longer written but consumed for back-compat)
//	vlong  systemTime        (ServerMutation.readFields, VERSION2 only)
//
// The 0x80-clear (VERSION1 "old") path is not produced by any currently
// supported tserver; we reject it rather than carry a second decoder.
func (d *dataInput) readServerMutation() (*cclient.Mutation, error) {
	first, err := d.readU8()
	if err != nil {
		return nil, err
	}
	if first&0x80 != 0x80 {
		return nil, fmt.Errorf("qwal: VERSION1 (old) mutation format not supported")
	}
	valuesPresent := first&0x01 == 0x01

	rowLen, err := d.readVInt()
	if err != nil {
		return nil, fmt.Errorf("rowLen: %w", err)
	}
	if rowLen < 0 {
		return nil, fmt.Errorf("qwal: negative row length %d", rowLen)
	}
	row := make([]byte, rowLen)
	if err := d.readFull(row); err != nil {
		return nil, fmt.Errorf("row: %w", err)
	}

	dataLen, err := d.readVInt()
	if err != nil {
		return nil, fmt.Errorf("dataLen: %w", err)
	}
	if dataLen < 0 {
		return nil, fmt.Errorf("qwal: negative data length %d", dataLen)
	}
	data := make([]byte, dataLen)
	if err := d.readFull(data); err != nil {
		return nil, fmt.Errorf("data: %w", err)
	}

	entries, err := d.readVInt()
	if err != nil {
		return nil, fmt.Errorf("entries: %w", err)
	}
	if entries < 0 {
		return nil, fmt.Errorf("qwal: negative entry count %d", entries)
	}

	var values [][]byte
	if valuesPresent {
		numValues, err := d.readVInt()
		if err != nil {
			return nil, fmt.Errorf("numValues: %w", err)
		}
		if numValues < 0 {
			return nil, fmt.Errorf("qwal: negative value count %d", numValues)
		}
		values = make([][]byte, 0, numValues)
		for i := int32(0); i < numValues; i++ {
			vl, err := d.readVInt()
			if err != nil {
				return nil, fmt.Errorf("value %d len: %w", i, err)
			}
			if vl < 0 {
				return nil, fmt.Errorf("qwal: negative value length %d", vl)
			}
			val := make([]byte, vl)
			if err := d.readFull(val); err != nil {
				return nil, fmt.Errorf("value %d body: %w", i, err)
			}
			values = append(values, val)
		}
	}

	if first&0x02 == 0x02 {
		// Legacy replication-sources block: a count followed by that many
		// WritableUtils strings (a VInt length + bytes). Consume and discard.
		numRepl, err := d.readVInt()
		if err != nil {
			return nil, fmt.Errorf("repl count: %w", err)
		}
		for i := int32(0); i < numRepl; i++ {
			if _, err := d.readText(); err != nil {
				return nil, fmt.Errorf("repl string %d: %w", i, err)
			}
		}
	}

	// ServerMutation: trailing VLong systemTime in the VERSION2 format.
	if _, err := d.readVLong(); err != nil {
		return nil, fmt.Errorf("systemTime: %w", err)
	}

	return decodeMutation(row, data, entries, values)
}

// decodeMutation expands the packed `data` block into a cclient.Mutation.
// Port of Mutation.deserializeColumnUpdate, applied `entries` times.
//
// Per-update layout inside `data`:
//
//	vbytes cf            (vint len + bytes)
//	vbytes cq
//	vbytes cv
//	bool   hasTs
//	[if hasTs] vlong ts
//	bool   deleted
//	vlong  valLen        (<0 => index into the large-value pool; 0 => empty)
//	[if valLen>0] bytes val
func decodeMutation(row, data []byte, entries int32, values [][]byte) (*cclient.Mutation, error) {
	m, err := cclient.NewMutation(row)
	if err != nil {
		return nil, err
	}
	src := byteSliceReader(data)
	cur := newDataInput(&src)
	for i := int32(0); i < entries; i++ {
		cf, err := cur.readVBytes()
		if err != nil {
			return nil, fmt.Errorf("update %d cf: %w", i, err)
		}
		cq, err := cur.readVBytes()
		if err != nil {
			return nil, fmt.Errorf("update %d cq: %w", i, err)
		}
		cv, err := cur.readVBytes()
		if err != nil {
			return nil, fmt.Errorf("update %d cv: %w", i, err)
		}
		hasTs, err := cur.readBool()
		if err != nil {
			return nil, fmt.Errorf("update %d hasTs: %w", i, err)
		}
		// Mirror Mutation.deserializeColumnUpdate: ts defaults to 0 when the
		// update carries no client timestamp (hasTs=false). The Long.MAX_VALUE
		// sentinel is a client-side write convention, not an on-disk value.
		var ts int64
		if hasTs {
			if ts, err = cur.readVLong(); err != nil {
				return nil, fmt.Errorf("update %d ts: %w", i, err)
			}
		}
		deleted, err := cur.readBool()
		if err != nil {
			return nil, fmt.Errorf("update %d deleted: %w", i, err)
		}
		valLen, err := cur.readVLong()
		if err != nil {
			return nil, fmt.Errorf("update %d valLen: %w", i, err)
		}
		var val []byte
		switch {
		case valLen < 0:
			idx := int(-valLen) - 1
			if idx < 0 || idx >= len(values) {
				return nil, fmt.Errorf("qwal: update %d value-pool index %d out of range (%d values)",
					i, idx, len(values))
			}
			val = values[idx]
		case valLen == 0:
			val = []byte{}
		default:
			val = make([]byte, valLen)
			if err := cur.readFull(val); err != nil {
				return nil, fmt.Errorf("update %d val: %w", i, err)
			}
		}
		if deleted {
			m.Delete(cf, cq, cv, ts)
		} else {
			m.Put(cf, cq, cv, ts, val)
		}
	}
	return m, nil
}

// readVBytes reads a VInt-length-prefixed byte slice — Mutation.readBytes.
func (d *dataInput) readVBytes() ([]byte, error) {
	n, err := d.readVLong()
	if err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, fmt.Errorf("qwal: negative vbytes length %d", n)
	}
	if n == 0 {
		return []byte{}, nil
	}
	body := make([]byte, n)
	if err := d.readFull(body); err != nil {
		return nil, err
	}
	return body, nil
}

// byteSliceReader is a tiny io.Reader over a []byte, used to drive a nested
// dataInput when decoding a Mutation's packed `data` block.
type byteSliceReader []byte

func (b *byteSliceReader) Read(p []byte) (int, error) {
	if len(*b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, *b)
	*b = (*b)[n:]
	return n, nil
}
