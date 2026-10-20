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

package qwal

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

func TestNativeWriterMatchesPinnedJavaOracle(t *testing.T) {
	fixture, err := os.ReadFile("testdata/native_wal_oracle_172.bin")
	if err != nil {
		t.Fatal(err)
	}
	reader := newDataInput(bytes.NewReader(fixture))
	if err := reader.readHeader(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writer, err := NewNativeWriter(&output)
	if err != nil {
		t.Fatal(err)
	}
	entries := 0
	for {
		key, err := reader.readKey()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		value, err := reader.readValue()
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteEntry(Entry{Key: key, Value: value}); err != nil {
			t.Fatal(err)
		}
		entries++
	}
	if entries != 3 {
		t.Fatalf("decoded %d entries, want 3", entries)
	}
	if !bytes.Equal(output.Bytes(), fixture) {
		t.Fatalf("Go native WAL encoder differs from pinned Java oracle:\n got %x\nwant %x",
			output.Bytes(), fixture)
	}
}

func TestNativeWriterEventFramingAndValidation(t *testing.T) {
	var out bytes.Buffer
	w, err := NewNativeWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	headerSize := out.Len()
	if err := w.WriteEntry(Entry{Key: LogFileKey{Event: EventMutation}}); err == nil {
		t.Fatal("accepted a mutation before OPEN")
	}
	if out.Len() != headerSize {
		t.Fatal("validation failure wrote a partial entry")
	}
	if err := w.WriteEntry(Entry{Key: LogFileKey{Event: EventOpen, TServerSession: "x"}}); err != nil {
		t.Fatal(err)
	}
	validSize := out.Len()
	if err := w.WriteEntry(Entry{Key: LogFileKey{Event: EventDefineTablet}}); err == nil {
		t.Fatal("accepted DEFINE_TABLET without an extent")
	}
	for _, prev := range [][]byte{[]byte("a"), []byte("b")} {
		if err := w.WriteEntry(Entry{Key: LogFileKey{Event: EventDefineTablet,
			Tablet: &KeyExtent{TableID: "g", EndRow: []byte("a"), PrevEndRow: prev}}}); err == nil {
			t.Fatalf("accepted an extent with prevEndRow %q >= endRow", prev)
		}
		if out.Len() != validSize {
			t.Fatal("invalid extent wrote a partial entry")
		}
	}
	if err := w.WriteEntry(Entry{Key: LogFileKey{Event: EventDefineTablet,
		Tablet: &KeyExtent{TableID: "g\xff"}}}); err == nil {
		t.Fatal("accepted an invalid UTF-8 table ID")
	}
	if out.Len() != validSize {
		t.Fatal("invalid table ID wrote a partial entry")
	}
	if err := w.WriteEntry(Entry{Key: LogFileKey{Event: EventCompactionStart, Seq: 4, TabletID: 2,
		Filename: "f\x00👻"}}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEntry(Entry{Key: LogFileKey{Event: EventCompactionFinish, Seq: 5, TabletID: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEntry(Entry{Key: LogFileKey{Event: EventOpen}}); err == nil {
		t.Fatal("accepted duplicate OPEN")
	}
	if out.Len() <= validSize {
		t.Fatal("compaction entries were not written")
	}
	reader := newDataInput(bytes.NewReader(out.Bytes()))
	if err := reader.readHeader(); err != nil {
		t.Fatal(err)
	}
	for _, event := range []LogEvent{EventOpen, EventCompactionStart, EventCompactionFinish} {
		key, err := reader.readKey()
		if err != nil {
			t.Fatal(err)
		}
		if key.Event != event {
			t.Fatalf("got event %s, want %s", key.Event, event)
		}
		if key.Event == EventCompactionStart && key.Filename != "f\x00👻" {
			t.Fatalf("modified UTF filename changed: %q", key.Filename)
		}
		if _, err := reader.readValue(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reader.readKey(); err != io.EOF {
		t.Fatalf("trailing bytes: %v", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }

func TestNativeWriterRejectsShortWrites(t *testing.T) {
	if _, err := NewNativeWriter(shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("header short write: %v", err)
	}
	var out bytes.Buffer
	w, err := NewNativeWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	w.out = shortWriter{}
	open := Entry{Key: LogFileKey{Event: EventOpen, TServerSession: "x"}}
	if err := w.WriteEntry(open); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("entry short write: %v", err)
	}
	if err := w.WriteEntry(open); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("poisoned writer reused: %v", err)
	}
}

func TestNativeWriterRejectsOversizeWithoutWritingFrame(t *testing.T) {
	var out bytes.Buffer
	w, err := NewNativeWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteEntry(Entry{Key: LogFileKey{Event: EventOpen, TServerSession: "x"}}); err != nil {
		t.Fatal(err)
	}
	size := out.Len()
	oversize := Entry{Key: LogFileKey{Event: EventMutation, Seq: 1, TabletID: 7},
		Value: LogFileValue{Mutations: []ServerMutation{{Row: make([]byte, maxMutationRowBytes+1)}}}}
	if err := w.WriteEntry(oversize); err == nil {
		t.Fatal("accepted a mutation above the supported-size bound")
	}
	if out.Len() != size {
		t.Fatal("invalid mutation wrote a partial frame")
	}
	large := make([]byte, maxMutationDataBytes/2)
	oversize = Entry{Key: LogFileKey{Event: EventMutation, Seq: 1, TabletID: 7},
		Value: LogFileValue{Mutations: []ServerMutation{{Row: []byte("r"), Updates: []ColumnUpdate{{
			ColumnFamily: large, ColumnQualifier: large, ColumnVisibility: large,
		}}}}}}
	if err := w.WriteEntry(oversize); err == nil {
		t.Fatal("accepted a packed update above the supported-size bound")
	}
	if out.Len() != size {
		t.Fatal("oversize packed update wrote a partial frame")
	}
}
