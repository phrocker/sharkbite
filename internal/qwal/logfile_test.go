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

// logfile_test.go exercises the WAL decoder against a synthetic fixture
// hand-encoded in Go to match the Java write() formats (LogFileKey.write,
// LogFileValue.write, Mutation.write VERSION2, ServerMutation.write,
// KeyExtent.writeTo). The doc references quarantined segments under
// /mnt/data/qwal/_quarantine/ — that path does not exist on this machine,
// so the fixture is constructed in-process instead.
package qwal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// walBuilder hand-encodes a WAL byte stream in the exact Java wire format.
type walBuilder struct {
	buf bytes.Buffer
}

func (w *walBuilder) u8(b byte)    { w.buf.WriteByte(b) }
func (w *walBuilder) raw(p []byte) { w.buf.Write(p) }

func (w *walBuilder) i32(v int32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(v))
	w.buf.Write(b[:])
}

func (w *walBuilder) i64(v int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	w.buf.Write(b[:])
}

func (w *walBuilder) utf(s string) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(len(s)))
	w.buf.Write(b[:])
	w.buf.WriteString(s)
}

func (w *walBuilder) vlong(v int64) {
	if _, err := wire.WriteVLong(&w.buf, v); err != nil {
		panic(err)
	}
}

func (w *walBuilder) vint(v int32) { w.vlong(int64(v)) }

// text encodes a Hadoop Text: VInt length + raw bytes.
func (w *walBuilder) text(p []byte) {
	w.vint(int32(len(p)))
	w.buf.Write(p)
}

// header writes the v4 magic and a zero-length crypto-params block (no crypto).
func (w *walBuilder) header() {
	w.buf.WriteString(logFileHeaderV4)
	w.i32(0) // CryptoUtils.writeParams: zero-length params == NoCryptoService
}

// vbytes encodes a Mutation.readBytes field: VInt length + raw bytes.
func vbytes(buf *bytes.Buffer, p []byte) {
	if _, err := wire.WriteVLong(buf, int64(len(p))); err != nil {
		panic(err)
	}
	buf.Write(p)
}

// encodeMutationData packs a single column update into the Mutation `data`
// block format consumed by decodeMutation / Java deserializeColumnUpdate.
func encodeColumnUpdate(cf, cq, cv []byte, hasTs bool, ts int64, deleted bool, val []byte) []byte {
	var b bytes.Buffer
	vbytes(&b, cf)
	vbytes(&b, cq)
	vbytes(&b, cv)
	if hasTs {
		b.WriteByte(1)
		if _, err := wire.WriteVLong(&b, ts); err != nil {
			panic(err)
		}
	} else {
		b.WriteByte(0)
	}
	if deleted {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	// valLen >= 0 inline value (no large-value pool used in the fixture).
	if _, err := wire.WriteVLong(&b, int64(len(val))); err != nil {
		panic(err)
	}
	b.Write(val)
	return b.Bytes()
}

// mutation encodes one ServerMutation in the VERSION2 format: a Mutation body
// (first byte 0x80, no values, no repl) followed by a trailing VLong
// systemTime.
func (w *walBuilder) mutation(row []byte, updates [][]byte, systemTime int64) {
	var data bytes.Buffer
	for _, u := range updates {
		data.Write(u)
	}
	w.u8(0x80) // VERSION2, values absent, repl absent
	w.vint(int32(len(row)))
	w.raw(row)
	w.vint(int32(data.Len()))
	w.raw(data.Bytes())
	w.vint(int32(len(updates))) // entry count
	w.vlong(systemTime)         // ServerMutation trailing systemTime
}

// emptyValue writes a LogFileValue frame carrying zero mutations. Java's
// LogReader/LogSorter call value.readFields() after EVERY key — including
// OPEN and DEFINE_TABLET — so every record on disk has a value frame, which
// for non-mutation events is just an int32 count of 0.
func (w *walBuilder) emptyValue() { w.i32(0) }

// buildFixture constructs a small but representative WAL: header, OPEN,
// DEFINE_TABLET, a single MUTATION, and a MANY_MUTATIONS carrying two.
func buildFixture(t *testing.T) []byte {
	t.Helper()
	w := &walBuilder{}
	w.header()

	// OPEN: ordinal 0, int32 version, UTF session — then an empty value frame.
	w.u8(byte(EventOpen))
	w.i32(logFileKeyVersion)
	w.utf("tserver-session-abc")
	w.emptyValue()

	// DEFINE_TABLET: ordinal 1, i64 seq, i32 tabletId, KeyExtent — empty value.
	w.u8(byte(EventDefineTablet))
	w.i64(10)
	w.i32(7)
	w.text([]byte("graph")) // tableId
	w.u8(1)                 // hasEndRow
	w.text([]byte("rowM"))  // endRow
	w.u8(1)                 // hasPrevEndRow
	w.text([]byte("rowA"))  // prevEndRow
	w.emptyValue()

	// MUTATION: ordinal 2, i64 seq, i32 tabletId, then LogFileValue (count=1).
	w.u8(byte(EventMutation))
	w.i64(11)
	w.i32(7)
	w.i32(1) // mutation count
	w.mutation([]byte("vertex:1"), [][]byte{
		encodeColumnUpdate([]byte("cf"), []byte("name"), []byte("VIS"), true, 1234, false, []byte("hello")),
	}, 9999)

	// MANY_MUTATIONS: ordinal 3, i64 seq, i32 tabletId, LogFileValue (count=2),
	// the second update being a delete.
	w.u8(byte(EventManyMutations))
	w.i64(12)
	w.i32(7)
	w.i32(2)
	w.mutation([]byte("vertex:2"), [][]byte{
		encodeColumnUpdate([]byte("e"), []byte("q1"), nil, false, 0, false, []byte("v1")),
	}, 10000)
	w.mutation([]byte("vertex:3"), [][]byte{
		encodeColumnUpdate([]byte("e"), []byte("q2"), nil, true, 0, true, nil),
	}, 10001)

	return w.buf.Bytes()
}

func TestDecodeSyntheticWAL(t *testing.T) {
	fixture := buildFixture(t)
	di := newDataInput(bytes.NewReader(fixture))
	if err := di.readHeader(); err != nil {
		t.Fatalf("readHeader: %v", err)
	}

	type want struct {
		event    LogEvent
		tabletID int32
		seq      int64
	}
	wants := []want{
		{EventOpen, -1, -1},
		{EventDefineTablet, 7, 10},
		{EventMutation, 7, 11},
		{EventManyMutations, 7, 12},
	}

	var entries []*Entry
	for {
		k, err := di.readKey()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("readKey: %v", err)
		}
		v, err := di.readValue()
		if err != nil {
			t.Fatalf("readValue for %s: %v", k.Event, err)
		}
		entries = append(entries, &Entry{Key: k, Value: v})
	}

	if len(entries) != len(wants) {
		t.Fatalf("entry count: want %d, got %d", len(wants), len(entries))
	}
	for i, wnt := range wants {
		got := entries[i].Key
		if got.Event != wnt.event || got.TabletID != wnt.tabletID || got.Seq != wnt.seq {
			t.Errorf("entry %d: want %v/tablet=%d/seq=%d, got %v/tablet=%d/seq=%d",
				i, wnt.event, wnt.tabletID, wnt.seq, got.Event, got.TabletID, got.Seq)
		}
	}

	// OPEN session.
	if entries[0].Key.TServerSession != "tserver-session-abc" {
		t.Errorf("OPEN session: got %q", entries[0].Key.TServerSession)
	}

	// DEFINE_TABLET extent.
	te := entries[1].Key.Tablet
	if te == nil {
		t.Fatal("DEFINE_TABLET: nil extent")
	}
	if te.TableID != "graph" || string(te.EndRow) != "rowM" || string(te.PrevEndRow) != "rowA" {
		t.Errorf("extent: got tableID=%q end=%q prev=%q", te.TableID, te.EndRow, te.PrevEndRow)
	}

	// MUTATION: one mutation, one Put update with timestamp + visibility.
	mu := entries[2].Value.Mutations
	if len(mu) != 1 {
		t.Fatalf("MUTATION: want 1 mutation, got %d", len(mu))
	}
	if string(mu[0].Row()) != "vertex:1" {
		t.Errorf("MUTATION row: got %q", mu[0].Row())
	}
	ents := mu[0].Entries()
	if len(ents) != 1 {
		t.Fatalf("MUTATION: want 1 column entry, got %d", len(ents))
	}
	e := ents[0]
	if string(e.ColFamily) != "cf" || string(e.ColQualifier) != "name" ||
		string(e.ColVisibility) != "VIS" || e.Timestamp != 1234 || e.Deleted ||
		string(e.Value) != "hello" {
		t.Errorf("MUTATION update: got cf=%q cq=%q cv=%q ts=%d del=%v val=%q",
			e.ColFamily, e.ColQualifier, e.ColVisibility, e.Timestamp, e.Deleted, e.Value)
	}

	// MANY_MUTATIONS: two mutations, second is a delete with server timestamp.
	mm := entries[3].Value.Mutations
	if len(mm) != 2 {
		t.Fatalf("MANY_MUTATIONS: want 2 mutations, got %d", len(mm))
	}
	del := mm[1].Entries()[0]
	if !del.Deleted {
		t.Errorf("MANY_MUTATIONS second update should be a delete")
	}
	// hasTs=false on disk decodes to ts 0 (Mutation.deserializeColumnUpdate).
	if del.Timestamp != 0 {
		t.Errorf("delete with hasTs=false should decode to ts 0, got %d", del.Timestamp)
	}
}

func TestReadHeaderEncryptedRejected(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(logFileHeaderV4)
	// Non-zero crypto params length => encrypted WAL.
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], 16)
	b.Write(lb[:])
	b.Write(make([]byte, 16))

	di := newDataInput(bytes.NewReader(b.Bytes()))
	err := di.readHeader()
	if !errors.Is(err, ErrEncryptedWAL) {
		t.Fatalf("want ErrEncryptedWAL, got %v", err)
	}
}

func TestReadHeaderIncomplete(t *testing.T) {
	// Writer died before the full magic was flushed.
	di := newDataInput(bytes.NewReader([]byte("--- Log File")))
	err := di.readHeader()
	if !errors.Is(err, ErrHeaderIncomplete) {
		t.Fatalf("want ErrHeaderIncomplete, got %v", err)
	}
}

func TestReadHeaderUnknownMagic(t *testing.T) {
	di := newDataInput(bytes.NewReader([]byte("not-a-wal-file-header-here!!")))
	err := di.readHeader()
	if err == nil || errors.Is(err, ErrHeaderIncomplete) || errors.Is(err, ErrEncryptedWAL) {
		t.Fatalf("want generic unrecognized-magic error, got %v", err)
	}
}

func TestInvalidEventOrdinal(t *testing.T) {
	w := &walBuilder{}
	w.header()
	w.u8(0xFF) // invalid event ordinal
	di := newDataInput(bytes.NewReader(w.buf.Bytes()))
	if err := di.readHeader(); err != nil {
		t.Fatalf("readHeader: %v", err)
	}
	if _, err := di.readKey(); err == nil {
		t.Fatal("want error on invalid event ordinal, got nil")
	}
}

func TestImplausibleMutationCount(t *testing.T) {
	w := &walBuilder{}
	w.header()
	w.u8(byte(EventMutation))
	w.i64(1)
	w.i32(1)
	w.i32(maxMutationsPerRecord + 1) // corrupt count
	di := newDataInput(bytes.NewReader(w.buf.Bytes()))
	if err := di.readHeader(); err != nil {
		t.Fatalf("readHeader: %v", err)
	}
	if _, err := di.readKey(); err != nil {
		t.Fatalf("readKey: %v", err)
	}
	if _, err := di.readValue(); err == nil {
		t.Fatal("want error on implausible mutation count, got nil")
	}
}
