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

package itercfg

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	gozk "github.com/go-zookeeper/zk"

	"github.com/phrocker/shoal-oss/internal/iterrt"
)

type rawConfigLocator struct {
	instancePath string
	data         map[string][]byte
}

func (l rawConfigLocator) InstancePath() string { return l.instancePath }

func (l rawConfigLocator) GetRaw(_ context.Context, path string) ([]byte, error) {
	value, ok := l.data[path]
	if !ok {
		return nil, fmt.Errorf("get %s: %w", path, gozk.ErrNoNode)
	}
	return append([]byte(nil), value...), nil
}

func TestEffectivePropertiesMergesConfigurationHierarchy(t *testing.T) {
	locator := rawConfigLocator{
		instancePath: "/accumulo/iid",
		data: map[string][]byte{
			"/accumulo/iid/config": buildSyntheticPropBlob(t, map[string]string{
				"general.custom.system": "system",
				"table.file.max":        "10",
			}, false),
			"/accumulo/iid/tables/5/namespace": []byte("ns1"),
			"/accumulo/iid/namespaces/ns1/config": buildSyntheticPropBlob(t, map[string]string{
				"table.file.max": "12",
			}, false),
			"/accumulo/iid/tables/5/config": buildSyntheticPropBlob(t, map[string]string{
				"table.file.max": "15",
			}, false),
		},
	}
	got, err := NewResolver(locator, 0, nil).EffectiveProperties(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	if got["general.custom.system"] != "system" || got["table.file.max"] != "15" {
		t.Fatalf("EffectiveProperties = %#v", got)
	}
	if got["table.file.type"] != "rf" || got["table.file.compress.type"] != "gz" {
		t.Fatalf("EffectiveProperties omitted pinned defaults: %#v", got)
	}
}

func TestEffectivePropertiesOverridesPinnedDefaults(t *testing.T) {
	locator := rawConfigLocator{
		instancePath: "/accumulo/iid",
		data: map[string][]byte{
			"/accumulo/iid/config": buildSyntheticPropBlob(t, map[string]string{
				"table.file.compress.type": "zstd",
			}, false),
			"/accumulo/iid/tables/5/namespace": []byte("+default"),
		},
	}
	got, err := NewResolver(locator, 0, nil).EffectiveProperties(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	if got["table.file.compress.type"] != "zstd" {
		t.Fatalf("compression type = %q, want zstd", got["table.file.compress.type"])
	}
}

func TestEffectivePropertiesAllowsMissingOptionalConfigurationNodes(t *testing.T) {
	locator := rawConfigLocator{
		instancePath: "/accumulo/iid",
		data: map[string][]byte{
			"/accumulo/iid/config":              buildSyntheticPropBlob(t, map[string]string{"table.file.max": "10"}, false),
			"/accumulo/iid/tables/+r/namespace": []byte("+accumulo"),
		},
	}
	got, err := NewResolver(locator, 0, nil).EffectiveProperties(context.Background(), "+r")
	if err != nil {
		t.Fatal(err)
	}
	if got["table.file.max"] != "10" {
		t.Fatalf("EffectiveProperties = %#v", got)
	}
}

// graphVidxStack mirrors a representative graph_vidx iterator config:
// latentEdgeDiscovery at priority 10, vers at priority 20.
func TestParseStack_GraphVidxMajc(t *testing.T) {
	props := map[string]string{
		"table.iterator.majc.latentEdgeDiscovery":                         "10,org.apache.accumulo.core.graph.LatentEdgeDiscoveryIterator",
		"table.iterator.majc.latentEdgeDiscovery.opt.maxCellBuffer":       "200",
		"table.iterator.majc.latentEdgeDiscovery.opt.maxPairsPerCell":     "500",
		"table.iterator.majc.latentEdgeDiscovery.opt.similarityThreshold": "0.85",
		"table.iterator.majc.vers":                                        "20,org.apache.accumulo.core.iterators.user.VersioningIterator",
		"table.iterator.majc.vers.opt.maxVersions":                        "10",
	}
	got, err := parseStack("tbl-vidx", iterrt.ScopeMajc, "table.iterator.majc.", props)
	if err != nil {
		t.Fatalf("parseStack: %v", err)
	}
	if !got.HasShoalCoverage() {
		t.Fatalf("expected full coverage, got skipped=%+v", got.Skipped)
	}
	if got.RegistryVersion != iterrt.CapabilityRegistryVersion {
		t.Fatalf("RegistryVersion = %d, want %d", got.RegistryVersion, iterrt.CapabilityRegistryVersion)
	}
	if len(got.Stack) != 2 {
		t.Fatalf("stack len = %d, want 2; stack=%+v", len(got.Stack), got.Stack)
	}
	// Priority ascending → latentEdge (10) before versioning (20).
	if got.Stack[0].Name != iterrt.IterLatentEdgeDiscovery {
		t.Errorf("stack[0].Name = %q, want %q", got.Stack[0].Name, iterrt.IterLatentEdgeDiscovery)
	}
	if got.Stack[1].Name != iterrt.IterVersioning {
		t.Errorf("stack[1].Name = %q, want %q", got.Stack[1].Name, iterrt.IterVersioning)
	}
	if got.Stack[1].Options["maxVersions"] != "10" {
		t.Errorf("versioning maxVersions = %q, want 10", got.Stack[1].Options["maxVersions"])
	}
	if got.Stack[0].Options["similarityThreshold"] != "0.85" {
		t.Errorf("latentEdge similarityThreshold = %q, want 0.85", got.Stack[0].Options["similarityThreshold"])
	}
}

// Unknown classes fail closed and are reported in the returned diagnostics.
func TestParseStack_RejectsUnknownClass(t *testing.T) {
	props := map[string]string{
		"table.iterator.majc.weird":               "5,com.example.NotPortedYet",
		"table.iterator.majc.weird.opt.something": "1",
		"table.iterator.majc.vers":                "20,org.apache.accumulo.core.iterators.user.VersioningIterator",
	}
	got, err := parseStack("tbl", iterrt.ScopeMajc, "table.iterator.majc.", props)
	if err == nil {
		t.Fatal("expected parseStack error")
	}
	var classErr *iterrt.UnsupportedIteratorClassError
	if !errors.As(err, &classErr) {
		t.Fatalf("expected UnsupportedIteratorClassError, got %T: %v", err, err)
	}
	if len(got.Stack) != 1 || got.Stack[0].Name != iterrt.IterVersioning {
		t.Errorf("stack = %+v, want [versioning]", got.Stack)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].Class != "com.example.NotPortedYet" {
		t.Errorf("skipped = %+v, want one NotPortedYet entry", got.Skipped)
	}
}

// Malformed headers fail closed with an explicit config error.
func TestParseStack_MalformedHeader(t *testing.T) {
	props := map[string]string{
		"table.iterator.majc.bad": "garbage-no-comma",
	}
	got, err := parseStack("tbl", iterrt.ScopeMajc, "table.iterator.majc.", props)
	if err == nil {
		t.Fatal("expected parseStack error")
	}
	var headerErr *MalformedIteratorConfigError
	if !errors.As(err, &headerErr) {
		t.Fatalf("expected MalformedIteratorConfigError, got %T: %v", err, err)
	}
	if len(got.Stack) != 0 {
		t.Errorf("stack should be empty, got %+v", got.Stack)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].Name != "bad" {
		t.Errorf("skipped should contain one 'bad' entry, got %+v", got.Skipped)
	}
}

// Empty scope returns an empty stack and HasShoalCoverage()=true (no
// iterators is a valid table state).
func TestParseStack_NoIteratorsConfigured(t *testing.T) {
	got, err := parseStack("tbl", iterrt.ScopeMajc, "table.iterator.majc.", nil)
	if err != nil {
		t.Fatalf("parseStack: %v", err)
	}
	if !got.HasShoalCoverage() {
		t.Errorf("empty stack should be 'covered': skipped=%+v", got.Skipped)
	}
	if len(got.Stack) != 0 {
		t.Errorf("stack = %+v, want empty", got.Stack)
	}
}

func TestParseStackIgnoresUnrelatedNestedProperties(t *testing.T) {
	props := map[string]string{
		"table.iterator.scan.vers":                    "20,org.apache.accumulo.core.iterators.user.VersioningIterator",
		"table.iterator.scan.vers.opt.maxVersions":    "1",
		"table.iterator.scan.table.custom.constraint": "ignored",
		"table.iterator.scan.tserver.memory.maps.max": "ignored",
	}
	got, err := parseStack("tbl", iterrt.ScopeScan, "table.iterator.scan.", props)
	if err != nil {
		t.Fatalf("parseStack: %v", err)
	}
	if len(got.Stack) != 1 || got.Stack[0].Name != iterrt.IterVersioning {
		t.Fatalf("stack = %+v, want versioning only", got.Stack)
	}
}

func TestParseStack_RejectsUnsupportedContext(t *testing.T) {
	props := map[string]string{
		"table.iterator.scan.latent": "10,org.apache.accumulo.core.graph.LatentEdgeDiscoveryIterator",
	}
	got, err := parseStack("tbl", iterrt.ScopeScan, "table.iterator.scan.", props)
	if err == nil {
		t.Fatal("expected parseStack error")
	}
	var ctxErr *iterrt.UnsupportedIteratorContextError
	if !errors.As(err, &ctxErr) {
		t.Fatalf("expected UnsupportedIteratorContextError, got %T: %v", err, err)
	}
	if len(got.Stack) != 0 {
		t.Fatalf("stack = %+v, want empty", got.Stack)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].Name != "latent" {
		t.Fatalf("skipped = %+v, want one latent entry", got.Skipped)
	}
}

func TestParseStack_RejectsUnsupportedOption(t *testing.T) {
	props := map[string]string{
		"table.iterator.majc.vers":         "20,org.apache.accumulo.core.iterators.user.VersioningIterator",
		"table.iterator.majc.vers.opt.foo": "1",
	}
	got, err := parseStack("tbl", iterrt.ScopeMajc, "table.iterator.majc.", props)
	if err == nil {
		t.Fatal("expected parseStack error")
	}
	var optErr *iterrt.UnsupportedIteratorOptionError
	if !errors.As(err, &optErr) {
		t.Fatalf("expected UnsupportedIteratorOptionError, got %T: %v", err, err)
	}
	if len(got.Stack) != 0 {
		t.Fatalf("stack = %+v, want empty", got.Stack)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].Name != "vers" {
		t.Fatalf("skipped = %+v, want one vers entry", got.Skipped)
	}
}

func TestParseStack_DeletingIteratorsImplAlias(t *testing.T) {
	props := map[string]string{
		"table.iterator.majc.del": "5,org.apache.accumulo.core.iteratorsImpl.system.DeletingIterator",
	}
	got, err := parseStack("tbl", iterrt.ScopeMajc, "table.iterator.majc.", props)
	if err != nil {
		t.Fatalf("parseStack: %v", err)
	}
	if len(got.Stack) != 1 || got.Stack[0].Name != iterrt.IterDeleting {
		t.Fatalf("stack = %+v, want deleting", got.Stack)
	}
}

func TestParseStack_ReportIsMachineReadable(t *testing.T) {
	props := map[string]string{
		"table.iterator.scan.vers":                 "20,org.apache.accumulo.core.iterators.user.VersioningIterator",
		"table.iterator.scan.vers.opt.maxVersions": "2",
	}
	got, err := parseStack("tbl", iterrt.ScopeScan, "table.iterator.scan.", props)
	if err != nil {
		t.Fatalf("parseStack: %v", err)
	}
	encoded, err := json.Marshal(got.Report)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded["supported"] != true ||
		decoded["accumuloVersion"] != iterrt.AccumuloCompatibilityVersion {
		t.Fatalf("report JSON = %s", encoded)
	}
}

func TestParseStack_UsesJavaUTF16TieBreakOrdering(t *testing.T) {
	props := map[string]string{
		"table.iterator.scan.\U00010000": "20,org.apache.accumulo.core.iterators.user.VersioningIterator",
		"table.iterator.scan.\uE000":     "20,org.apache.accumulo.core.iterators.system.VisibilityFilter",
	}
	got, err := parseStack("tbl", iterrt.ScopeScan, "table.iterator.scan.", props)
	if err != nil {
		t.Fatalf("parseStack: %v", err)
	}
	if len(got.Stack) != 2 || got.Stack[0].Name != iterrt.IterVersioning ||
		got.Stack[1].Name != iterrt.IterVisibility {
		t.Fatalf("stack order = %+v, want versioning then visibility", got.Stack)
	}
}

// TestDecodePropBlob_GzipRoundtrip builds a versioned-props blob with
// the wire layout described in decodePropBlob's doc-comment and asserts
// the decoder reproduces the original map. Catches breakage in the
// version/compressed-flag/timestamp/gzip framing without needing a
// live cluster.
func TestDecodePropBlob_GzipRoundtrip(t *testing.T) {
	want := map[string]string{
		"table.iterator.majc.vers":                                        "20,org.apache.accumulo.core.iterators.user.VersioningIterator",
		"table.iterator.majc.vers.opt.maxVersions":                        "10",
		"table.iterator.majc.latentEdgeDiscovery":                         "10,org.apache.accumulo.core.graph.LatentEdgeDiscoveryIterator",
		"table.iterator.majc.latentEdgeDiscovery.opt.maxCellBuffer":       "200",
		"table.iterator.majc.latentEdgeDiscovery.opt.maxPairsPerCell":     "500",
		"table.iterator.majc.latentEdgeDiscovery.opt.similarityThreshold": "0.85",
		"table.unrelated.someprop":                                        "ignored",
	}
	blob := buildSyntheticPropBlob(t, want, true /*gzip*/)

	got, err := decodePropBlob(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("entry count: got=%d want=%d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q: got=%q want=%q", k, got[k], v)
		}
	}
}

// TestDecodePropBlob_UncompressedRoundtrip exercises the compressed=false
// path. Some Accumulo configs disable gzip on small property sets.
func TestDecodePropBlob_UncompressedRoundtrip(t *testing.T) {
	want := map[string]string{
		"table.iterator.majc.vers":                 "20,VersioningIterator",
		"table.iterator.majc.vers.opt.maxVersions": "5",
	}
	blob := buildSyntheticPropBlob(t, want, false /*no gzip*/)
	got, err := decodePropBlob(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q: got=%q want=%q", k, got[k], v)
		}
	}
}

// buildSyntheticPropBlob encodes a property map in Java's
// VersionedPropGzipCodec wire format. Identical layout to the Java code
// path so the decoder is exercised against the actual on-wire bytes.
func buildSyntheticPropBlob(t *testing.T, props map[string]string, compressed bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	// int32 version=1
	binary.Write(&buf, binary.BigEndian, uint32(1))
	// bool compressed
	if compressed {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
	// UTF timestamp
	ts := "2026-05-15T13:09:09.000000000Z"
	binary.Write(&buf, binary.BigEndian, uint16(len(ts)))
	buf.WriteString(ts)

	// Payload: int32 count + UTF pairs.
	var payload bytes.Buffer
	binary.Write(&payload, binary.BigEndian, uint32(len(props)))
	for k, v := range props {
		binary.Write(&payload, binary.BigEndian, uint16(len(k)))
		payload.WriteString(k)
		binary.Write(&payload, binary.BigEndian, uint16(len(v)))
		payload.WriteString(v)
	}

	if compressed {
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write(payload.Bytes()); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := gz.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
	} else {
		buf.Write(payload.Bytes())
	}
	return buf.Bytes()
}

func TestSplitPriorityClass(t *testing.T) {
	cases := []struct {
		in      string
		wantPri int
		wantCls string
		wantErr bool
	}{
		{"10,foo.bar.Baz", 10, "foo.bar.Baz", false},
		{" 20 , foo.Bar ", 20, "foo.Bar", false},
		{"noComma", 0, "", true},
		{"abc,foo.Bar", 0, "", true},
		{"10,", 10, "", true},
	}
	for _, c := range cases {
		pri, cls, err := splitPriorityClass(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err = %v, wantErr = %v", c.in, err, c.wantErr)
			continue
		}
		if err == nil {
			if pri != c.wantPri || cls != c.wantCls {
				t.Errorf("%q: got (%d, %q), want (%d, %q)", c.in, pri, cls, c.wantPri, c.wantCls)
			}
		}
	}
}
