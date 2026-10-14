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

import "testing"

func TestAnomalyDetect_BandFilterEmitsFlaggedRows(t *testing.T) {
	cells := []kv{
		{mk("r1", "attr", "name", "", 2), []byte("ok")},
		{mk("r1", "m", "score", "", 1), []byte("5")},
		{mk("r2", "attr", "name", "", 2), []byte("bad")},
		{mk("r2", "m", "score", "", 1), []byte("99")},
	}
	it, err := BuildStack(newSliceSource(cells...), []IterSpec{{Name: IterAnomalyDetect, Options: map[string]string{AnomalyDetectValueCF: "m", AnomalyDetectValueCQ: "score", AnomalyDetectMin: "0", AnomalyDetectMax: "10"}}}, IteratorEnvironment{Scope: ScopeScan})
	if err != nil {
		t.Fatalf("BuildStack: %v", err)
	}
	if err := it.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := drain(it)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d cells, want r2 row's 2 cells", len(got))
	}
	for _, c := range got {
		if string(c.k.Row) != "r2" {
			t.Fatalf("emitted in-band row %q", c.k.Row)
		}
	}
}

func TestAnomalyDetect_InBandSuppressed(t *testing.T) {
	cells := []kv{{mk("r1", "m", "score", "", 1), []byte("5")}}
	it, err := BuildStack(newSliceSource(cells...), []IterSpec{{Name: IterAnomalyDetect, Options: map[string]string{AnomalyDetectValueCF: "m", AnomalyDetectValueCQ: "score", AnomalyDetectMin: "0", AnomalyDetectMax: "10"}}}, IteratorEnvironment{Scope: ScopeScan})
	if err != nil {
		t.Fatalf("BuildStack: %v", err)
	}
	if err := it.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := drain(it)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d cells, want none", len(got))
	}
}
