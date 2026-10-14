/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package coordination

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"time"
)

const schemaV1 byte = 1

func E(value []byte) []byte {
	encoded := make([]byte, 0, len(value)+2)
	for _, item := range value {
		encoded = append(encoded, item)
		if item == 0 {
			encoded = append(encoded, 0xff)
		}
	}
	return append(encoded, 0, 0)
}

func DecodeE(data []byte) ([]byte, int, error) {
	value := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if data[i] != 0 {
			value = append(value, data[i])
			continue
		}
		if i+1 >= len(data) {
			return nil, 0, invalid("escaped component is truncated")
		}
		switch data[i+1] {
		case 0:
			return value, i + 2, nil
		case 0xff:
			value = append(value, 0)
			i++
		default:
			return nil, 0, invalid("escaped component is malformed")
		}
	}
	return nil, 0, invalid("escaped component has no terminator")
}

func U32(value uint32) []byte {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	return encoded[:]
}

func U64(value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return encoded[:]
}

func INV64(value uint64) []byte { return U64(^value) }

func TIME(value time.Time) ([]byte, error) {
	if err := validateTime("time", value, false); err != nil {
		return nil, err
	}
	value = value.UTC()
	encoded := make([]byte, 11)
	binary.BigEndian.PutUint16(encoded[0:2], uint16(value.Year()))
	encoded[2] = byte(value.Month())
	encoded[3] = byte(value.Day())
	encoded[4] = byte(value.Hour())
	encoded[5] = byte(value.Minute())
	encoded[6] = byte(value.Second())
	binary.BigEndian.PutUint32(encoded[7:11], uint32(value.Nanosecond()))
	return encoded, nil
}

func DecodeTIME(data []byte) (time.Time, error) {
	if len(data) != 11 {
		return time.Time{}, invalid("TIME must be exactly 11 canonical bytes")
	}
	year := int(binary.BigEndian.Uint16(data[0:2]))
	month := time.Month(data[2])
	day, hour, minute, second := int(data[3]), int(data[4]), int(data[5]), int(data[6])
	nanos := int(binary.BigEndian.Uint32(data[7:11]))
	if year < 1 || year > 9999 || month < 1 || month > 12 ||
		day < 1 || day > 31 || hour > 23 || minute > 59 ||
		second > 59 || nanos >= 1_000_000_000 {
		return time.Time{}, invalid("TIME contains invalid UTC fields")
	}
	value := time.Date(year, month, day, hour, minute, second, nanos, time.UTC)
	if value.Year() != year || value.Month() != month || value.Day() != day {
		return time.Time{}, invalid("TIME contains invalid calendar fields")
	}
	return value, nil
}

func INV_TIME(value time.Time) ([]byte, error) {
	encoded, err := TIME(value)
	if err != nil {
		return nil, err
	}
	for i := range encoded {
		encoded[i] = ^encoded[i]
	}
	return encoded, nil
}

func B8(tag byte, parts ...[]byte) byte {
	h := sha256.New()
	_, _ = h.Write(U32(1))
	_, _ = h.Write([]byte{tag})
	for _, part := range parts {
		_, _ = h.Write(U32(uint32(len(part))))
		_, _ = h.Write(part)
	}
	return h.Sum(nil)[0]
}

type RowKind byte

const (
	RowTxn       RowKind = 'T'
	RowAllocator RowKind = 'Q'
	RowOutcome   RowKind = 'O'
	RowManifest  RowKind = 'M'
)

type CoordinationRow struct {
	Kind       RowKind
	Domain     DomainID
	TXN        TXN
	Epoch      Epoch
	ChunkIndex uint32
}

func rowPrefix(kind RowKind, band byte) []byte {
	return []byte{schemaV1, byte(kind), band}
}

func TxnRow(domain DomainID, txn TXN) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if err := txn.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowTxn, B8('C', domain, txn))
	row = append(row, E(domain)...)
	row = append(row, E(txn)...)
	return row, nil
}

func AllocatorRow(domain DomainID) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowAllocator, B8('C', domain))
	return append(row, E(domain)...), nil
}

func OutcomeRow(domain DomainID, epoch Epoch) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if err := epoch.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowOutcome, B8('C', domain))
	row = append(row, E(domain)...)
	return append(row, U64(uint64(epoch))...), nil
}

func ManifestRow(domain DomainID, txn TXN, index uint32) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if err := txn.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowManifest, B8('M', domain, txn))
	row = append(row, E(domain)...)
	row = append(row, E(txn)...)
	return append(row, U32(index)...), nil
}

func ParseCoordinationRow(row []byte) (CoordinationRow, error) {
	if len(row) < 5 || row[0] != schemaV1 {
		return CoordinationRow{}, invalid("coordination row prefix is malformed")
	}
	result := CoordinationRow{Kind: RowKind(row[1])}
	domain, consumed, err := DecodeE(row[3:])
	if err != nil {
		return CoordinationRow{}, err
	}
	result.Domain = DomainID(domain)
	if err := result.Domain.Validate(); err != nil {
		return CoordinationRow{}, err
	}
	offset := 3 + consumed
	switch result.Kind {
	case RowAllocator:
		if offset != len(row) || row[2] != B8('C', result.Domain) {
			return CoordinationRow{}, invalid("allocator row has malformed or trailing components")
		}
	case RowTxn, RowManifest:
		txn, used, err := DecodeE(row[offset:])
		if err != nil {
			return CoordinationRow{}, err
		}
		result.TXN = TXN(txn)
		if err := result.TXN.Validate(); err != nil {
			return CoordinationRow{}, err
		}
		offset += used
		tag := byte('C')
		if result.Kind == RowManifest {
			tag = 'M'
		}
		if row[2] != B8(tag, result.Domain, result.TXN) {
			return CoordinationRow{}, invalid("coordination row partition band mismatch")
		}
		if result.Kind == RowTxn {
			if offset != len(row) {
				return CoordinationRow{}, invalid("transaction row has trailing components")
			}
		} else {
			if len(row)-offset != 4 {
				return CoordinationRow{}, invalid("manifest row chunk component is malformed")
			}
			result.ChunkIndex = binary.BigEndian.Uint32(row[offset:])
		}
	case RowOutcome:
		if len(row)-offset != 8 || row[2] != B8('C', result.Domain) {
			return CoordinationRow{}, invalid("outcome row has malformed or trailing components")
		}
		value := binary.BigEndian.Uint64(row[offset:])
		if value == 0 || value > uint64(^uint64(0)>>1) {
			return CoordinationRow{}, invalid("outcome row epoch is outside the supported range")
		}
		result.Epoch = Epoch(value)
	default:
		return CoordinationRow{}, invalid("coordination row kind is unsupported")
	}
	return result, nil
}

func HasRowPrefix(prefix, row []byte) bool { return bytes.HasPrefix(row, prefix) }
