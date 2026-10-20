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
	"unicode/utf16"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// logFileHeaderV4 is the magic string DfsLogger writes at the head of every
// v4 WAL file (DfsLogger.LOG_FILE_HEADER_V4). v3 ("--- Log File Header (v3)
// ---") is Accumulo 1.9-era and only ever appears uncrypted; we detect it
// but the qwal read fleet targets v4 clusters.
const (
	logFileHeaderV4 = "--- Log File Header (v4) ---"
	logFileHeaderV3 = "--- Log File Header (v3) ---"
	// NoFileEncrypter.getDecryptionParameters in the pinned Accumulo revision
	// returns NoCryptoService.VERSION as UTF-8, which is the seven ASCII bytes
	// below (not the ghost emoji itself).
	noCryptoParams = "U+1F47B"
	// Text fields are untrusted WAL bytes. Bound the allocation before
	// reading a corrupt Hadoop VInt; this is a reader safety limit, not an
	// Accumulo wire-format limit.
	maxWALTextBytes = 16 << 20
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

// ErrEncryptedWAL is returned when a v4 WAL does not carry the exact pinned
// NoCryptoService parameter block. This includes encrypted and unsupported
// legacy headers; the reader must not guess whether following bytes are clear.
var ErrEncryptedWAL = errors.New("qwal: WAL crypto params are not the pinned no-crypto sentinel")

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

// ColumnUpdate preserves the timestamp flag on the WAL wire. A missing client
// timestamp uses the enclosing ServerMutation's system time when materialized.
type ColumnUpdate struct {
	ColumnFamily, ColumnQualifier, ColumnVisibility []byte
	Timestamp                                       int64
	HasTimestamp                                    bool
	Deleted                                         bool
	Value                                           []byte
}

// ServerMutation is the native WAL representation. In particular, SystemTime
// and HasTimestamp must survive decoding independently: the client mutation
// API's latest-timestamp sentinel cannot represent an explicit MaxInt64.
type ServerMutation struct {
	Row        []byte
	SystemTime int64
	Updates    []ColumnUpdate
}

func (m ServerMutation) EffectiveTimestamp(update ColumnUpdate) int64 {
	if update.HasTimestamp {
		return update.Timestamp
	}
	return m.SystemTime
}

// LogFileValue is the decoded value half: the list of mutations carried by a
// MUTATION (exactly one) or MANY_MUTATIONS (zero or more) entry. Other event
// types carry an empty value frame.
type LogFileValue struct {
	Mutations []ServerMutation
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

// The reader materializes each native WAL record. These are supported-size
// bounds, not Accumulo format limits: larger valid records fail closed until
// the decoder can stream them without unbounded retained memory.
const (
	maxDecodedRecordBytes = 64 << 20
	maxMutationRowBytes   = 1 << 20
	maxMutationDataBytes  = 32 << 20
	maxMutationPoolBytes  = 48 << 20
	// The retained ColumnUpdate struct occupies 112 bytes on 64-bit Go.
	// Charge above that size so the decoded-record cap covers slice growth.
	mutationUpdateBudgetBytes = 128
)

// dataInput is a Java-DataInput-style cursor over a byte stream. It satisfies
// wire.ByteAndReader (io.Reader + io.ByteReader) so it can drive the shared
// VInt/VLong helpers in internal/rfile/wire.
type dataInput struct {
	r *bufio.Reader
	// Only set for the bounded packed Mutation.data substream. The bufio
	// reader can prefetch, so remaining bytes include both buffers.
	packed *byteSliceReader
}

func newDataInput(r io.Reader) *dataInput {
	return &dataInput{r: bufio.NewReader(r)}
}

func (d *dataInput) Read(p []byte) (int, error) { return d.r.Read(p) }
func (d *dataInput) ReadByte() (byte, error)    { return d.r.ReadByte() }

func (d *dataInput) packedRemaining() int64 {
	return int64(d.r.Buffered() + len(*d.packed))
}

func (d *dataInput) readFull(p []byte) error {
	_, err := io.ReadFull(d.r, p)
	return err
}

func (d *dataInput) readU8() (byte, error) { return d.r.ReadByte() }

func (d *dataInput) readBool() (bool, error) {
	b, err := d.r.ReadByte()
	if err != nil {
		return false, err
	}
	// Mutation's UnsynchronizedBuffer.Reader treats only 1 as true, while
	// DataInput.readBoolean (used by KeyExtent) treats any nonzero as true.
	// Reject malformed packed flags rather than changing a put to a delete.
	if d.packed != nil && b > 1 {
		return false, fmt.Errorf("qwal: invalid packed boolean %d", b)
	}
	return b != 0, nil
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

// readUTF decodes Java DataOutput.writeUTF's modified UTF-8, including its
// two-byte NUL and surrogate-pair representation of supplementary runes.
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
	units := make([]uint16, 0, n)
	for i := 0; i < len(body); {
		b := body[i]
		switch {
		case b&0x80 == 0:
			units = append(units, uint16(b))
			i++
		case b&0xe0 == 0xc0:
			if i+1 >= len(body) || body[i+1]&0xc0 != 0x80 {
				return "", errors.New("qwal: malformed modified UTF-8")
			}
			units = append(units, uint16(b&0x1f)<<6|uint16(body[i+1]&0x3f))
			i += 2
		case b&0xf0 == 0xe0:
			if i+2 >= len(body) || body[i+1]&0xc0 != 0x80 || body[i+2]&0xc0 != 0x80 {
				return "", errors.New("qwal: malformed modified UTF-8")
			}
			units = append(units, uint16(b&0x0f)<<12|uint16(body[i+1]&0x3f)<<6|uint16(body[i+2]&0x3f))
			i += 3
		default:
			return "", errors.New("qwal: unsupported modified UTF-8 lead byte")
		}
	}
	runes := make([]rune, 0, len(units))
	for i := 0; i < len(units); i++ {
		u := units[i]
		if u >= 0xd800 && u <= 0xdbff {
			if i+1 >= len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
				return "", errors.New("qwal: unpaired modified UTF-8 surrogate")
			}
			runes = append(runes, utf16.DecodeRune(rune(u), rune(units[i+1])))
			i++
		} else if u >= 0xdc00 && u <= 0xdfff {
			return "", errors.New("qwal: unpaired modified UTF-8 surrogate")
		} else {
			runes = append(runes, rune(u))
		}
	}
	return string(runes), nil
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
	if n > maxWALTextBytes {
		return nil, fmt.Errorf("qwal: Text length %d exceeds reader limit %d", n, maxWALTextBytes)
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
// positioned at the first entry, ErrEncryptedWAL unless the exact pinned
// no-crypto params are present, or ErrHeaderIncomplete on a short header.
//
// v4 layout (DfsLogger.getDecryptingStream + open()):
//
//	bytes  magic            ("--- Log File Header (v4) ---")
//	int32  cryptoParamsLen  (CryptoUtils.writeParams)
//	bytes  cryptoParams     (exactly ASCII "U+1F47B" => no crypto)
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
		// Reject any other length before allocating or reading a claimed block.
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
		if paramLen != int32(len(noCryptoParams)) {
			return ErrEncryptedWAL
		}
		var params [len(noCryptoParams)]byte
		if err := d.readFull(params[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return ErrHeaderIncomplete
			}
			return fmt.Errorf("qwal: read crypto params: %w", err)
		}
		if string(params[:]) != noCryptoParams {
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
	remaining := int64(maxDecodedRecordBytes) - int64(count)*64
	if remaining < 0 {
		return v, errors.New("qwal: decoded record exceeds memory limit")
	}
	// Exact capacity avoids geometric slice growth escaping the byte budget.
	v.Mutations = make([]ServerMutation, 0, count)
	for i := int32(0); i < count; i++ {
		m, err := d.readServerMutation(&remaining)
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
//	[if 0x02] legacy replication block (rejected: not emitted by pinned v4)
//	vlong  systemTime        (ServerMutation.readFields, VERSION2 only)
//
// The 0x80-clear (VERSION1 "old") path is not produced by any currently
// supported tserver; we reject it rather than carry a second decoder.
func (d *dataInput) readServerMutation(remaining *int64) (ServerMutation, error) {
	var empty ServerMutation
	first, err := d.readU8()
	if err != nil {
		return empty, err
	}
	if first&0x80 != 0x80 {
		return empty, fmt.Errorf("qwal: VERSION1 (old) mutation format not supported")
	}
	if first&^byte(0x81) != 0 {
		return empty, fmt.Errorf("qwal: unsupported VERSION2 mutation flags %#x", first)
	}
	valuesPresent := first&0x01 == 0x01

	rowLen, err := d.readVInt()
	if err != nil {
		return empty, fmt.Errorf("rowLen: %w", err)
	}
	if rowLen < 0 || rowLen > maxMutationRowBytes || int64(rowLen) > *remaining {
		return empty, fmt.Errorf("qwal: invalid row length %d", rowLen)
	}
	*remaining -= int64(rowLen)
	row := make([]byte, rowLen)
	if err := d.readFull(row); err != nil {
		return empty, fmt.Errorf("row: %w", err)
	}

	dataLen, err := d.readVInt()
	if err != nil {
		return empty, fmt.Errorf("dataLen: %w", err)
	}
	if dataLen < 0 || dataLen > maxMutationDataBytes || int64(dataLen)*2 > *remaining {
		return empty, fmt.Errorf("qwal: invalid data length %d", dataLen)
	}
	*remaining -= int64(dataLen) * 2 // packed bytes and decoded column copies
	data := make([]byte, dataLen)
	if err := d.readFull(data); err != nil {
		return empty, fmt.Errorf("data: %w", err)
	}

	entries, err := d.readVInt()
	if err != nil {
		return empty, fmt.Errorf("entries: %w", err)
	}
	if entries < 0 || entries > dataLen/6 || int64(entries)*mutationUpdateBudgetBytes > *remaining {
		return empty, fmt.Errorf("qwal: invalid entry count %d for %d packed bytes", entries, dataLen)
	}
	*remaining -= int64(entries) * mutationUpdateBudgetBytes

	var values [][]byte
	if valuesPresent {
		numValues, err := d.readVInt()
		if err != nil {
			return empty, fmt.Errorf("numValues: %w", err)
		}
		if numValues < 0 || int64(numValues)*24 > *remaining {
			return empty, fmt.Errorf("qwal: invalid value count %d", numValues)
		}
		*remaining -= int64(numValues) * 24
		values = make([][]byte, 0, numValues)
		for i := int32(0); i < numValues; i++ {
			vl, err := d.readVInt()
			if err != nil {
				return empty, fmt.Errorf("value %d len: %w", i, err)
			}
			if vl < 0 || vl > maxMutationPoolBytes || int64(vl) > *remaining {
				return empty, fmt.Errorf("qwal: invalid value length %d", vl)
			}
			*remaining -= int64(vl)
			val := make([]byte, vl)
			if err := d.readFull(val); err != nil {
				return empty, fmt.Errorf("value %d body: %w", i, err)
			}
			values = append(values, val)
		}
	}

	// ServerMutation: trailing VLong systemTime in the VERSION2 format.
	systemTime, err := d.readVLong()
	if err != nil {
		return empty, fmt.Errorf("systemTime: %w", err)
	}

	return decodeMutation(row, data, entries, values, systemTime)
}

// decodeMutation expands the packed `data` block into a ServerMutation.
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
func decodeMutation(row, data []byte, entries int32, values [][]byte, systemTime int64) (ServerMutation, error) {
	m := ServerMutation{Row: row, SystemTime: systemTime, Updates: make([]ColumnUpdate, 0, entries)}
	src := byteSliceReader(data)
	cur := newDataInput(&src)
	cur.packed = &src
	for i := int32(0); i < entries; i++ {
		cf, err := cur.readVBytes()
		if err != nil {
			return ServerMutation{}, fmt.Errorf("update %d cf: %w", i, err)
		}
		cq, err := cur.readVBytes()
		if err != nil {
			return ServerMutation{}, fmt.Errorf("update %d cq: %w", i, err)
		}
		cv, err := cur.readVBytes()
		if err != nil {
			return ServerMutation{}, fmt.Errorf("update %d cv: %w", i, err)
		}
		hasTs, err := cur.readBool()
		if err != nil {
			return ServerMutation{}, fmt.Errorf("update %d hasTs: %w", i, err)
		}
		// Preserve the raw flag and timestamp. Java's ServerColumnUpdate
		// substitutes systemTime only when the client timestamp is absent.
		var ts int64
		if hasTs {
			if ts, err = cur.readVLong(); err != nil {
				return ServerMutation{}, fmt.Errorf("update %d ts: %w", i, err)
			}
		}
		deleted, err := cur.readBool()
		if err != nil {
			return ServerMutation{}, fmt.Errorf("update %d deleted: %w", i, err)
		}
		valLen, err := cur.readVLong()
		if err != nil {
			return ServerMutation{}, fmt.Errorf("update %d valLen: %w", i, err)
		}
		var val []byte
		switch {
		case valLen < 0:
			idx := int(-valLen) - 1
			if idx < 0 || idx >= len(values) {
				return ServerMutation{}, fmt.Errorf("qwal: update %d value-pool index %d out of range (%d values)",
					i, idx, len(values))
			}
			val = values[idx]
		case valLen == 0:
			val = []byte{}
		default:
			if valLen > maxMutationDataBytes {
				return ServerMutation{}, fmt.Errorf("qwal: update %d inline value length %d exceeds limit", i, valLen)
			}
			if valLen > cur.packedRemaining() {
				return ServerMutation{}, fmt.Errorf("qwal: update %d inline value length %d exceeds packed bytes", i, valLen)
			}
			val = make([]byte, valLen)
			if err := cur.readFull(val); err != nil {
				return ServerMutation{}, fmt.Errorf("update %d val: %w", i, err)
			}
		}
		m.Updates = append(m.Updates, ColumnUpdate{
			ColumnFamily: cf, ColumnQualifier: cq, ColumnVisibility: cv,
			Timestamp: ts, HasTimestamp: hasTs, Deleted: deleted, Value: val,
		})
	}
	if _, err := cur.readU8(); err != io.EOF {
		if err != nil {
			return ServerMutation{}, fmt.Errorf("qwal: packed mutation trailing data: %w", err)
		}
		return ServerMutation{}, errors.New("qwal: packed mutation has trailing data")
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
	if n > maxMutationDataBytes {
		return nil, fmt.Errorf("qwal: vbytes length %d exceeds limit", n)
	}
	if d.packed != nil && n > d.packedRemaining() {
		return nil, fmt.Errorf("qwal: vbytes length %d exceeds packed bytes", n)
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
