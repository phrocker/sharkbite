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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// NativeWriter writes the pinned Accumulo v4 no-crypto WAL framing. It is a
// wire encoder only: its caller must supply shared durable storage, commit
// ordering, and a manager-compatible recovery protocol before acknowledging
// writes. A failed underlying Write poisons the encoder; discard the file.
type NativeWriter struct {
	out     io.Writer
	entries int
	failed  error
}

// NewNativeWriter writes the pinned v4 header. The returned writer requires
// OPEN as its first entry.
func NewNativeWriter(out io.Writer) (*NativeWriter, error) {
	if out == nil {
		return nil, errors.New("qwal: nil WAL output")
	}
	var header bytes.Buffer
	header.WriteString(logFileHeaderV4)
	writeInt32(&header, int32(len(noCryptoParams)))
	header.WriteString(noCryptoParams)
	if err := writeAll(out, header.Bytes()); err != nil {
		return nil, fmt.Errorf("qwal: write native WAL header: %w", err)
	}
	return &NativeWriter{out: out}, nil
}

// WriteEntry encodes a complete key/value pair before writing it to the
// underlying stream. Validation failure writes no bytes for this entry.
func (w *NativeWriter) WriteEntry(entry Entry) error {
	if w.failed != nil {
		return w.failed
	}
	if w.entries == 0 && entry.Key.Event != EventOpen {
		return errors.New("qwal: first native WAL entry must be OPEN")
	}
	if w.entries > 0 && entry.Key.Event == EventOpen {
		return errors.New("qwal: duplicate OPEN entry")
	}
	var frame bytes.Buffer
	if err := encodeNativeKey(&frame, entry.Key); err != nil {
		return err
	}
	if err := encodeNativeValue(&frame, entry.Key.Event, entry.Value); err != nil {
		return err
	}
	if err := writeAll(w.out, frame.Bytes()); err != nil {
		w.failed = fmt.Errorf("qwal: write native WAL entry: %w", err)
		return w.failed
	}
	w.entries++
	return nil
}

func writeAll(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	return nil
}

func writeInt32(w *bytes.Buffer, n int32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(n))
	w.Write(b[:])
}

func writeInt64(w *bytes.Buffer, n int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	w.Write(b[:])
}

func writeVInt(w *bytes.Buffer, n int32) {
	// bytes.Buffer's WriteByte cannot fail.
	_, _ = wire.WriteVInt(w, n)
}

func writeVLong(w *bytes.Buffer, n int64) {
	_, _ = wire.WriteVLong(w, n)
}

// Java DataOutput.writeUTF encodes UTF-16 code units, with NUL as C0 80.
// wire.WriteUTF writes UTF-8 bytes and must not be used for WAL key strings.
func writeModifiedUTF(w *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return errors.New("qwal: invalid UTF-8 in native WAL key string")
	}
	var body bytes.Buffer
	for _, r := range s {
		if r <= 0xffff {
			writeUTF16Unit(&body, uint16(r))
		} else {
			hi, lo := utf16.EncodeRune(r)
			writeUTF16Unit(&body, uint16(hi))
			writeUTF16Unit(&body, uint16(lo))
		}
		if body.Len() > 0xffff {
			return errors.New("qwal: modified UTF-8 exceeds Java writeUTF limit")
		}
	}
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(body.Len()))
	w.Write(length[:])
	w.Write(body.Bytes())
	return nil
}

func writeUTF16Unit(w *bytes.Buffer, u uint16) {
	switch {
	case u >= 1 && u <= 0x7f:
		w.WriteByte(byte(u))
	case u <= 0x7ff:
		w.WriteByte(0xc0 | byte(u>>6))
		w.WriteByte(0x80 | byte(u&0x3f))
	default:
		w.WriteByte(0xe0 | byte(u>>12))
		w.WriteByte(0x80 | byte((u>>6)&0x3f))
		w.WriteByte(0x80 | byte(u&0x3f))
	}
}

func writeText(w *bytes.Buffer, b []byte) error {
	if len(b) > maxWALTextBytes {
		return fmt.Errorf("qwal: Text length %d exceeds reader limit", len(b))
	}
	writeVInt(w, int32(len(b)))
	w.Write(b)
	return nil
}

func encodeNativeKey(w *bytes.Buffer, key LogFileKey) error {
	if key.Event < 0 || key.Event >= logEventCount {
		return fmt.Errorf("qwal: invalid LogEvent %d", key.Event)
	}
	w.WriteByte(byte(key.Event))
	switch key.Event {
	case EventOpen:
		writeInt32(w, logFileKeyVersion)
		return writeModifiedUTF(w, key.TServerSession)
	case EventDefineTablet:
		if key.Tablet == nil {
			return errors.New("qwal: DEFINE_TABLET requires extent")
		}
		if key.Tablet.EndRow != nil && key.Tablet.PrevEndRow != nil &&
			bytes.Compare(key.Tablet.PrevEndRow, key.Tablet.EndRow) >= 0 {
			return errors.New("qwal: DEFINE_TABLET prevEndRow must precede endRow")
		}
		writeInt64(w, key.Seq)
		writeInt32(w, key.TabletID)
		if err := writeText(w, []byte(key.Tablet.TableID)); err != nil {
			return err
		}
		if key.Tablet.EndRow != nil {
			w.WriteByte(1)
			if err := writeText(w, key.Tablet.EndRow); err != nil {
				return err
			}
		} else {
			w.WriteByte(0)
		}
		if key.Tablet.PrevEndRow != nil {
			w.WriteByte(1)
			if err := writeText(w, key.Tablet.PrevEndRow); err != nil {
				return err
			}
		} else {
			w.WriteByte(0)
		}
		return nil
	case EventMutation, EventManyMutations, EventCompactionFinish:
		writeInt64(w, key.Seq)
		writeInt32(w, key.TabletID)
		return nil
	case EventCompactionStart:
		writeInt64(w, key.Seq)
		writeInt32(w, key.TabletID)
		return writeModifiedUTF(w, key.Filename)
	default:
		return fmt.Errorf("qwal: unsupported event %d", key.Event)
	}
}

func encodeNativeValue(w *bytes.Buffer, event LogEvent, value LogFileValue) error {
	n := len(value.Mutations)
	if n > maxMutationsPerRecord {
		return fmt.Errorf("qwal: mutation count %d exceeds reader limit", n)
	}
	switch event {
	case EventMutation:
		if n != 1 {
			return fmt.Errorf("qwal: MUTATION requires one mutation, got %d", n)
		}
	case EventManyMutations:
		// Java permits an empty batch.
	default:
		if n != 0 {
			return fmt.Errorf("qwal: %s requires an empty value, got %d mutations", event, n)
		}
	}
	writeInt32(w, int32(n))
	remaining := int64(maxDecodedRecordBytes) - int64(n)*64
	if remaining < 0 {
		return errors.New("qwal: decoded record exceeds reader memory limit")
	}
	for _, m := range value.Mutations {
		if err := encodeServerMutation(w, m, &remaining); err != nil {
			return err
		}
	}
	return nil
}

func encodeServerMutation(w *bytes.Buffer, m ServerMutation, remaining *int64) error {
	if len(m.Row) > maxMutationRowBytes || int64(len(m.Row)) > *remaining {
		return fmt.Errorf("qwal: invalid row length %d", len(m.Row))
	}
	*remaining -= int64(len(m.Row))
	var data bytes.Buffer
	var pool [][]byte
	for _, u := range m.Updates {
		for _, part := range [][]byte{u.ColumnFamily, u.ColumnQualifier, u.ColumnVisibility} {
			if len(part) > maxMutationDataBytes {
				return fmt.Errorf("qwal: column length %d exceeds reader limit", len(part))
			}
			writeVLong(&data, int64(len(part)))
			data.Write(part)
		}
		if u.HasTimestamp {
			data.WriteByte(1)
			writeVLong(&data, u.Timestamp)
		} else {
			data.WriteByte(0)
		}
		if u.Deleted {
			data.WriteByte(1)
		} else {
			data.WriteByte(0)
		}
		if len(u.Value) < 1<<15 {
			writeVLong(&data, int64(len(u.Value)))
			data.Write(u.Value)
		} else {
			if len(u.Value) > maxMutationPoolBytes {
				return fmt.Errorf("qwal: pooled value length %d exceeds reader limit", len(u.Value))
			}
			pool = append(pool, u.Value)
			writeVLong(&data, -int64(len(pool)))
		}
		if data.Len() > maxMutationDataBytes {
			return fmt.Errorf("qwal: packed mutation length %d exceeds reader limit", data.Len())
		}
	}
	if len(m.Updates) > data.Len()/6 {
		return errors.New("qwal: too many updates for packed mutation")
	}
	cost := int64(data.Len())*2 + int64(len(m.Updates))*mutationUpdateBudgetBytes + int64(len(pool))*24
	for _, v := range pool {
		cost += int64(len(v))
	}
	if cost > *remaining {
		return errors.New("qwal: decoded record exceeds reader memory limit")
	}
	*remaining -= cost
	first := byte(0x80)
	if len(pool) > 0 {
		first |= 0x01
	}
	w.WriteByte(first)
	writeVInt(w, int32(len(m.Row)))
	w.Write(m.Row)
	writeVInt(w, int32(data.Len()))
	w.Write(data.Bytes())
	writeVInt(w, int32(len(m.Updates)))
	if len(pool) > 0 {
		writeVInt(w, int32(len(pool)))
		for _, v := range pool {
			writeVInt(w, int32(len(v)))
			w.Write(v)
		}
	}
	writeVLong(w, m.SystemTime)
	return nil
}
