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

package iterrt

import "github.com/phrocker/shoal-oss/internal/rfile/wire"

// kv is a test cell.
type kv struct {
	k *Key
	v []byte
}

// newSliceSource builds a leaf SortedKeyValueIterator over test cells,
// reusing the production SliceSource so the tests exercise the real
// thing. Input must be in wire.Key.Compare order.
func newSliceSource(cells ...kv) *SliceSource {
	cs := make([]Cell, len(cells))
	for i, c := range cells {
		cs[i] = Cell{Key: c.k, Value: c.v}
	}
	return NewSliceSource(cs)
}

// drain collects every (key,value) an iterator surfaces from the current
// position. Keys are cloned so the result survives iterator reuse.
func drain(it SortedKeyValueIterator) ([]kv, error) {
	var out []kv
	for it.HasTop() {
		out = append(out, kv{k: it.GetTopKey().Clone(), v: append([]byte(nil), it.GetTopValue()...)})
		if err := it.Next(); err != nil {
			return out, err
		}
	}
	return out, nil
}

// mk builds a Key from string parts. ts descends per the Accumulo
// convention; deleted defaults false.
func mk(row, cf, cq, cv string, ts int64) *Key {
	return &wire.Key{
		Row:              []byte(row),
		ColumnFamily:     []byte(cf),
		ColumnQualifier:  []byte(cq),
		ColumnVisibility: visBytes(cv),
		Timestamp:        ts,
	}
}

func mkDel(row, cf, cq, cv string, ts int64) *Key {
	k := mk(row, cf, cq, cv, ts)
	k.Deleted = true
	return k
}

func visBytes(cv string) []byte {
	if cv == "" {
		return nil
	}
	return []byte(cv)
}
