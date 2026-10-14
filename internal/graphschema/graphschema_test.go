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

package graphschema

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func TestRowBuildersAndParsers(t *testing.T) {
	tests := []struct {
		name    string
		suffix  string
		wantRow []byte
		build   func(string) []byte
		parse   func([]byte) (string, bool)
	}{
		{
			name:    "event",
			suffix:  "01J0EVENT00000000000000000",
			wantRow: []byte("evt:01J0EVENT00000000000000000"),
			build:   EventRow,
			parse:   ParseEventID,
		},
		{
			name:    "entity",
			suffix:  "agent:planner",
			wantRow: []byte("ent:agent:planner"),
			build:   EntityRow,
			parse:   ParseEntityName,
		},
		{
			name:    "term",
			suffix:  "memory",
			wantRow: []byte("idx:memory"),
			build:   TermRow,
			parse:   ParseTerm,
		},
		{
			name:    "empty event suffix",
			suffix:  "",
			wantRow: []byte("evt:"),
			build:   EventRow,
			parse:   ParseEventID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRow := tt.build(tt.suffix)
			if !bytes.Equal(gotRow, tt.wantRow) {
				t.Fatalf("build row = %q, want %q", gotRow, tt.wantRow)
			}

			gotSuffix, ok := tt.parse(gotRow)
			if !ok {
				t.Fatalf("parse(%q) ok = false, want true", gotRow)
			}
			if gotSuffix != tt.suffix {
				t.Fatalf("parse(%q) suffix = %q, want %q", gotRow, gotSuffix, tt.suffix)
			}
		})
	}
}

func TestParsersRejectWrongPrefixes(t *testing.T) {
	tests := []struct {
		name  string
		row   []byte
		parse func([]byte) (string, bool)
	}{
		{name: "event rejects entity", row: EntityRow("agent"), parse: ParseEventID},
		{name: "event rejects partial prefix", row: []byte("ev:01J"), parse: ParseEventID},
		{name: "entity rejects term", row: TermRow("graph"), parse: ParseEntityName},
		{name: "term rejects event", row: EventRow("01J"), parse: ParseTerm},
		{name: "term rejects empty", row: nil, parse: ParseTerm},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.parse(tt.row)
			if ok {
				t.Fatalf("parse(%q) = (%q, true), want ok false", tt.row, got)
			}
		})
	}
}

func TestColumnFamilyAccessors(t *testing.T) {
	tests := []struct {
		name string
		got  []byte
		want string
	}{
		{name: "content", got: ContentCF(), want: ContentCFName},
		{name: "vector", got: VectorCF(), want: VectorCFName},
		{name: "attribute", got: AttributeCF(), want: AttributeCFName},
		{name: "temporal edge", got: TemporalEdgeCF(), want: TemporalEdgeCFName},
		{name: "causal edge", got: CausalEdgeCF(), want: CausalEdgeCFName},
		{name: "semantic edge", got: SemanticEdgeCF(), want: SemanticEdgeCFName},
		{name: "entity edge", got: EntityEdgeCF(), want: EntityEdgeCFName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.got) != tt.want {
				t.Fatalf("cf = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestEdgeCFRoundTrip(t *testing.T) {
	tests := []struct {
		edgeType EdgeType
		wantCF   string
	}{
		{edgeType: Temporal, wantCF: TemporalEdgeCFName},
		{edgeType: Causal, wantCF: CausalEdgeCFName},
		{edgeType: Semantic, wantCF: SemanticEdgeCFName},
		{edgeType: Entity, wantCF: EntityEdgeCFName},
	}

	for _, tt := range tests {
		t.Run(tt.wantCF, func(t *testing.T) {
			cf := EdgeCF(tt.edgeType)
			if string(cf) != tt.wantCF {
				t.Fatalf("EdgeCF(%v) = %q, want %q", tt.edgeType, cf, tt.wantCF)
			}

			gotType, ok := EdgeTypeFromCF(cf)
			if !ok {
				t.Fatalf("EdgeTypeFromCF(%q) ok = false, want true", cf)
			}
			if gotType != tt.edgeType {
				t.Fatalf("EdgeTypeFromCF(%q) = %v, want %v", cf, gotType, tt.edgeType)
			}
		})
	}
}

func TestEdgeCFUnknowns(t *testing.T) {
	if cf := EdgeCF(EdgeType(99)); cf != nil {
		t.Fatalf("EdgeCF(unknown) = %q, want nil", cf)
	}
	if got, ok := EdgeTypeFromCF([]byte("edge.unknown:")); ok {
		t.Fatalf("EdgeTypeFromCF(unknown) = (%v, true), want ok false", got)
	}
}

func TestPackUnpackWeight(t *testing.T) {
	tests := []float32{0, 1, -2.5, 3.25, float32(math.Inf(1)), float32(math.Inf(-1))}

	for _, weight := range tests {
		t.Run("", func(t *testing.T) {
			packed := PackWeight(weight)
			if len(packed) != 4 {
				t.Fatalf("len(PackWeight(%v)) = %d, want 4", weight, len(packed))
			}

			wantBits := math.Float32bits(weight)
			if gotBits := binary.BigEndian.Uint32(packed); gotBits != wantBits {
				t.Fatalf("packed bits = 0x%x, want 0x%x", gotBits, wantBits)
			}

			got, ok := UnpackWeight(packed)
			if !ok {
				t.Fatalf("UnpackWeight(PackWeight(%v)) ok = false, want true", weight)
			}
			if math.Float32bits(got) != wantBits {
				t.Fatalf("UnpackWeight(PackWeight(%v)) = %v, want same bits", weight, got)
			}
		})
	}
}

func TestUnpackWeightRejectsInvalidLengths(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, {0}, {0, 1, 2}, {0, 1, 2, 3, 4}} {
		if got, ok := UnpackWeight(raw); ok {
			t.Fatalf("UnpackWeight(%v) = (%v, true), want ok false", raw, got)
		}
	}
}
