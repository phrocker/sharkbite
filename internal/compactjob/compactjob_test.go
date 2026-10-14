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

package compactjob

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/manager"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

const (
	testUUID    = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	testECID    = ECIDPrefix + testUUID
	versClass   = "org.apache.accumulo.core.iterators.user.VersioningIterator"
	latentClass = "org.apache.accumulo.core.graph.LatentEdgeDiscoveryIterator"
)

// storedFile renders a whole-file StoredTabletFile metadata entry, the
// form Accumulo 3+ sends in InputFile.metadataFileEntry.
func storedFile(path string) string {
	return fmt.Sprintf(`{"path":"%s","startRow":"","endRow":""}`, path)
}

// fencedFile renders a row-fenced StoredTabletFile entry — a file whose
// referenced range is only part of the underlying RFile.
//
// The rows travel as byte arrays through ByteArrayToBase64TypeAdapter,
// which uses Base64.getUrlEncoder, and the bytes underneath are a Hadoop
// Text serialization (StoredTabletFile.encodeRow calls Text.write). So
// callers pass the logical row and the helper renders both layers.
func fencedFile(path, start, end string) string {
	return fmt.Sprintf(`{"path":"%s","startRow":"%s","endRow":"%s"}`,
		path, encodeRow(start), encodeRow(end))
}

// encodeRow mirrors StoredTabletFile.encodeRow for the short rows these
// tests use: Text.write emits a VInt length then the bytes, and a length
// below 128 is a single byte.
func encodeRow(row string) string {
	if row == "" {
		return ""
	}
	if len(row) >= 128 {
		panic("encodeRow: test rows must be shorter than 128 bytes")
	}
	return base64.URLEncoding.EncodeToString(append([]byte{byte(len(row))}, row...))
}

// validJob is a well-formed SYSTEM compaction of two whole files, the
// baseline every refusal test mutates one field of.
func validJob() *tabletserver.TExternalCompactionJob {
	job := tabletserver.NewTExternalCompactionJob()
	job.ExternalCompactionId = testECID
	job.Extent = &data.TKeyExtent{
		Table:      []byte("2"),
		PrevEndRow: []byte("c"),
		EndRow:     []byte("m"),
	}
	job.Files = []*tabletserver.InputFile{
		{MetadataFileEntry: storedFile("hdfs://nn/accumulo/tables/2/t-0001/F0001.rf"), Size: 1024, Entries: 10, Timestamp: 1700000000000},
		{MetadataFileEntry: storedFile("hdfs://nn/accumulo/tables/2/t-0001/F0002.rf"), Size: 2048, Entries: 20, Timestamp: 1700000000001},
	}
	job.OutputFile = tmpOutput(testECID)
	job.PropagateDeletes = true
	job.Kind = tabletserver.TCompactionKind_SYSTEM
	// Both containers are present and empty, which is what a coordinator
	// sends for a job with no overrides and no iterators — and what
	// Compactor.initialize requires, since it dereferences both before
	// reading a file. Thrift decodes a present empty map or list into a
	// non-nil one, so this is a different wire shape than leaving them
	// unset.
	job.Overrides = map[string]string{}
	job.IteratorSettings = iterConfig()
	return job
}

// tmpOutput renders the name a real coordinator sends: the compaction
// temp file from TabletNameGenerator.getNextDataFilenameForMajc, which
// is the allocated RFile name plus "_tmp_" and the job's own ECID. The
// manager strips the suffix when it commits the compaction.
func tmpOutput(ecid string) string {
	return "hdfs://nn/accumulo/tables/2/t-0001/C0003.rf_tmp_" + ecid
}

// iterConfig builds the iterator container a coordinator sends. The list
// is always present — Compactor.initialize iterates it unguarded — so a
// call with no settings still yields an empty, non-nil list rather than
// the nil a bare variadic would leave.
//
// Each setting's option map is filled in for the same reason:
// SystemIteratorUtil.toTIteratorSetting builds every wire setting from
// IteratorSetting.getOptions, which wraps a HashMap and is never null,
// so a real job always carries a present map. Tests that mean to send a
// missing one build the setting without this helper.
func iterConfig(settings ...*tabletserver.TIteratorSetting) *tabletserver.IteratorConfig {
	if settings == nil {
		settings = []*tabletserver.TIteratorSetting{}
	}
	for _, s := range settings {
		if s != nil && s.Properties == nil {
			s.Properties = map[string]string{}
		}
	}
	return &tabletserver.IteratorConfig{Iterators: settings}
}

// mustTranslate fails the test if the job is refused.
func mustTranslate(t *testing.T, job *tabletserver.TExternalCompactionJob, opts Options) *Plan {
	t.Helper()
	plan, err := Translate(job, opts)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	return plan
}

// assertRefused checks that the job is refused with the expected
// coordinator-facing class, and that the detail names the offending
// field (an operator reading the manager log gets both).
func assertRefused(t *testing.T, job *tabletserver.TExternalCompactionJob, opts Options, wantClass, wantField string) *Refusal {
	t.Helper()
	plan, err := Translate(job, opts)
	if err == nil {
		t.Fatalf("Translate accepted the job (plan %+v), want refusal %s", plan, wantClass)
	}
	if plan != nil {
		t.Fatalf("Translate returned a plan alongside an error: %+v", plan)
	}
	refusal := RefusalOf(err)
	if refusal == nil {
		t.Fatalf("error %v is not a *Refusal; the compactor could not report a class", err)
	}
	if refusal.Class != wantClass {
		t.Fatalf("class = %q, want %q (detail: %s)", refusal.Class, wantClass, refusal.Error())
	}
	if wantField != "" && refusal.Field != wantField {
		t.Fatalf("field = %q, want %q (detail: %s)", refusal.Field, wantField, refusal.Error())
	}
	if !strings.Contains(refusal.Error(), refusal.Class) {
		t.Fatalf("Error() = %q, want it to carry the class", refusal.Error())
	}
	return refusal
}

// TestTranslateMapsEveryJobField is the fidelity test: a fully populated
// USER job must land in the plan field-for-field, including the
// propagateDeletes → FullMajorCompaction inversion that decides whether
// tombstones may be dropped.
func TestTranslateMapsEveryJobField(t *testing.T) {
	job := validJob()
	job.Kind = tabletserver.TCompactionKind_USER
	job.FateId = &manager.TFateId{
		Type:      manager.TFateInstanceType_USER,
		TxUUIDStr: "b7f1c0de-0000-4000-8000-00000000abcd",
	}
	job.PropagateDeletes = false
	job.Overrides = map[string]string{
		propCompressType:      "gz",
		propCompressBlockSize: "128K",
	}
	job.IteratorSettings = iterConfig(
		&tabletserver.TIteratorSetting{
			Priority:      20,
			Name:          "vers",
			IteratorClass: versClass,
			Properties:    map[string]string{"maxVersions": "3"},
		},
	)

	plan := mustTranslate(t, job, Options{})

	if plan.ECID != testECID {
		t.Errorf("ECID = %q, want %q", plan.ECID, testECID)
	}
	if plan.TableID != "2" {
		t.Errorf("TableID = %q, want 2", plan.TableID)
	}
	if !bytes.Equal(plan.Extent.GetTable(), job.Extent.GetTable()) ||
		!bytes.Equal(plan.Extent.GetPrevEndRow(), job.Extent.GetPrevEndRow()) ||
		!bytes.Equal(plan.Extent.GetEndRow(), job.Extent.GetEndRow()) {
		t.Errorf("Extent = %+v, want the job's extent %+v", plan.Extent, job.Extent)
	}
	if plan.Kind != tabletserver.TCompactionKind_USER {
		t.Errorf("Kind = %v, want USER", plan.Kind)
	}
	if want := "FATE:USER:b7f1c0de-0000-4000-8000-00000000abcd"; plan.FateID != want {
		t.Errorf("FateID = %q, want %q", plan.FateID, want)
	}
	if plan.OutputFile != job.OutputFile {
		t.Errorf("OutputFile = %q, want %q", plan.OutputFile, job.OutputFile)
	}
	if plan.PropagateDeletes {
		t.Error("PropagateDeletes = true, want the job's false")
	}
	if !plan.FullMajorCompaction {
		t.Error("FullMajorCompaction = false; propagateDeletes=false means this output is the tablet's only file")
	}
	if plan.Scope != iterrt.ScopeMajc {
		t.Errorf("Scope = %v, want majc", plan.Scope)
	}
	if plan.Codec != block.CodecGzip {
		t.Errorf("Codec = %q, want %q", plan.Codec, block.CodecGzip)
	}
	if plan.BlockSize != 128*1024 {
		t.Errorf("BlockSize = %d, want %d", plan.BlockSize, 128*1024)
	}
	if plan.TotalInputBytes != 3072 || plan.TotalInputEntries != 30 {
		t.Errorf("totals = %d bytes / %d entries, want 3072 / 30",
			plan.TotalInputBytes, plan.TotalInputEntries)
	}

	if len(plan.Inputs) != 2 {
		t.Fatalf("Inputs = %d, want 2", len(plan.Inputs))
	}
	first := plan.Inputs[0]
	if first.Entry != job.Files[0].MetadataFileEntry {
		t.Errorf("Inputs[0].Entry = %q, want the metadata entry verbatim (the commit dereferences this exact string)", first.Entry)
	}
	if first.Path != "hdfs://nn/accumulo/tables/2/t-0001/F0001.rf" {
		t.Errorf("Inputs[0].Path = %q, want the decoded StoredTabletFile path", first.Path)
	}
	if first.Size != 1024 || first.Entries != 10 || first.Timestamp != 1700000000000 {
		t.Errorf("Inputs[0] DataFileValue = %+v, want size=1024 entries=10 ts=1700000000000", first)
	}

	if len(plan.Iterators) != 1 {
		t.Fatalf("Iterators = %d, want 1", len(plan.Iterators))
	}
	it := plan.Iterators[0]
	if it.Name != "vers" || it.Class != versClass || it.Priority != 20 {
		t.Errorf("Iterators[0] = %+v, want name=vers class=%s priority=20", it, versClass)
	}
	if it.Spec.Name != iterrt.IterVersioning {
		t.Errorf("Iterators[0].Spec.Name = %q, want %q", it.Spec.Name, iterrt.IterVersioning)
	}
	if it.Spec.Options["maxVersions"] != "3" {
		t.Errorf("Iterators[0] options = %v, want maxVersions=3", it.Spec.Options)
	}
	if len(plan.Stack) != 1 || plan.Stack[0].Name != iterrt.IterVersioning {
		t.Errorf("Stack = %+v, want one versioning entry", plan.Stack)
	}
}

// TestTranslateLegacyBarePathEntry covers the pre-3.0 metadata form: the
// entry is the path itself, which is whole-file by definition.
func TestTranslateLegacyBarePathEntry(t *testing.T) {
	job := validJob()
	job.Files = []*tabletserver.InputFile{
		{MetadataFileEntry: "hdfs://nn/accumulo/tables/2/t-0001/F0001.rf", Size: 1, Entries: 1},
	}

	plan := mustTranslate(t, job, Options{})
	if plan.Inputs[0].Path != "hdfs://nn/accumulo/tables/2/t-0001/F0001.rf" {
		t.Fatalf("Path = %q, want the bare entry", plan.Inputs[0].Path)
	}
}

// TestTranslateRefusesFencedInput is the ranged-input gate: a fenced
// StoredTabletFile covers only part of its RFile, and shoal's composer
// reads whole files, so merging one would pull in another tablet's cells.
func TestTranslateRefusesFencedInput(t *testing.T) {
	for _, tt := range []struct {
		name       string
		start, end string
	}{
		{"start only", "d", ""},
		{"end only", "", "k"},
		{"both bounds", "d", "k"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Files[1].MetadataFileEntry = fencedFile("hdfs://nn/accumulo/tables/2/t-0001/F0002.rf", tt.start, tt.end)

			r := assertRefused(t, job, Options{}, ClassRangedInputFile, "files[1]")
			if !strings.Contains(r.Detail, "fenced") {
				t.Fatalf("detail = %q, want it to name the fence", r.Detail)
			}
			// StoredTabletFile.deserialize rebuilds the fence with
			// new Range(startRow, true, endRow, false), so the bounds an
			// operator reads must be start-inclusive, end-exclusive.
			if want := fmt.Sprintf("[%q,%q)", tt.start, tt.end); !strings.Contains(r.Detail, want) {
				t.Fatalf("detail = %q, want it to report the fence as %s", r.Detail, want)
			}
		})
	}
}

// TestTranslateRefusesEntryWithoutBothFenceFields is the counterpart to
// the fence gate above: it pins that "no fence" is a positive statement
// the entry has to make. StoredTabletFile.deserialize calls
// Objects.requireNonNull on startRow and endRow, so an entry that omits
// one is unparseable to Java — and if shoal read it as an empty string
// it would call the file whole and merge rows the manager never put in
// this compaction's range.
func TestTranslateRefusesEntryWithoutBothFenceFields(t *testing.T) {
	for _, tt := range []struct {
		name  string
		entry string
		want  string
	}{
		{
			name:  "startRow absent",
			entry: `{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0002.rf","endRow":""}`,
			want:  "startRow",
		},
		{
			name:  "endRow absent",
			entry: `{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0002.rf","startRow":""}`,
			want:  "endRow",
		},
		{
			name:  "both absent",
			entry: `{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0002.rf"}`,
			want:  "startRow",
		},
		{
			name:  "startRow explicitly null",
			entry: `{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0002.rf","startRow":null,"endRow":""}`,
			want:  "startRow",
		},
		{
			name:  "endRow explicitly null",
			entry: `{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0002.rf","startRow":"","endRow":null}`,
			want:  "endRow",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Files[1].MetadataFileEntry = tt.entry

			r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
			if !strings.Contains(r.Detail, tt.want) {
				t.Fatalf("detail = %q, want it to name the missing %s", r.Detail, tt.want)
			}
		})
	}
}

// TestTranslateRefusesUndecodableFenceRows covers the other half of the
// fence contract: the rows are base64 (Base64.getUrlEncoder), so a value
// outside that alphabet is a parse failure in Java, not a fence.
func TestTranslateRefusesUndecodableFenceRows(t *testing.T) {
	for _, tt := range []struct {
		name  string
		entry string
	}{
		{
			name:  "startRow is not base64",
			entry: `{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0002.rf","startRow":"d","endRow":""}`,
		},
		{
			name:  "endRow uses the non-url alphabet",
			entry: `{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0002.rf","startRow":"","endRow":"a+/b"}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Files[1].MetadataFileEntry = tt.entry

			r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
			if !strings.Contains(r.Detail, "base64") {
				t.Fatalf("detail = %q, want it to name the encoding", r.Detail)
			}
		})
	}
}

// TestTranslateAcceptsUnpaddedFenceRows pins the leniency boundary:
// Base64.getUrlDecoder accepts input with or without trailing padding,
// so an unpadded row is a fence, not a malformed entry.
func TestTranslateAcceptsUnpaddedFenceRows(t *testing.T) {
	job := validJob()
	job.Files[1].MetadataFileEntry =
		`{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0002.rf","startRow":"AQID","endRow":"AQI"}`

	assertRefused(t, job, Options{}, ClassRangedInputFile, "files[1]")
}

// TestTranslateRefusesUnframedFenceRows covers the layer under base64.
// decodeRow hands the decoded bytes to Hadoop Text.readFields, which
// reads a VInt length and then readFully's that many bytes, so a row
// that decodes cleanly but does not frame a Text is an entry Java throws
// on — a malformed job, not an unsupported fence.
func TestTranslateRefusesUnframedFenceRows(t *testing.T) {
	for _, tt := range []struct {
		name string
		row  string
		want string
	}{
		{
			// [0x01]: a one-byte row with nothing after the length.
			name: "length with no payload",
			row:  "AQ==",
			want: "only 0",
		},
		{
			// [0x8f 0x05]: a two-byte VInt declaring five bytes.
			name: "multi-byte length with no payload",
			row:  base64.URLEncoding.EncodeToString([]byte{0x8f, 0x05}),
			want: "only 0",
		},
		{
			// [0x8f]: the VInt itself is cut off after its first byte.
			name: "truncated length",
			row:  base64.URLEncoding.EncodeToString([]byte{0x8f}),
			want: "needs 2 bytes",
		},
		{
			// [0x87 0x00]: decodeVIntSize 2, isNegativeVInt, so ~0 = -1.
			name: "negative length",
			row:  base64.URLEncoding.EncodeToString([]byte{0x87, 0x00}),
			want: "is negative",
		},
		{
			// [0x89 0x01 0 0 0 0 0 0]: 2^48, which readVInt rejects
			// before Text ever sees it.
			name: "length past int range",
			row:  base64.URLEncoding.EncodeToString([]byte{0x89, 0x01, 0, 0, 0, 0, 0, 0}),
			want: "does not fit in an int",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Files[1].MetadataFileEntry = fmt.Sprintf(
				`{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0002.rf","startRow":"%s","endRow":""}`,
				tt.row)

			r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
			if !strings.Contains(r.Detail, tt.want) {
				t.Fatalf("detail = %q, want it to contain %q", r.Detail, tt.want)
			}
		})
	}
}

// TestTranslateAcceptsFenceRowsWithTrailingBytes pins the other side of
// the framing rule: Text.readFields readFully's exactly the declared
// length and leaves the rest of the buffer alone, so trailing bytes are
// not a parse failure.
func TestTranslateAcceptsFenceRowsWithTrailingBytes(t *testing.T) {
	job := validJob()
	job.Files[1].MetadataFileEntry = fmt.Sprintf(
		`{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0002.rf","startRow":"%s","endRow":""}`,
		base64.URLEncoding.EncodeToString([]byte{0x01, 'a', 'b', 'c'}))

	assertRefused(t, job, Options{}, ClassRangedInputFile, "files[1]")
}

// TestTranslateRefusesUnparsableFilePaths covers the shape every path in
// a job has to have. Each of these throws inside
// ReferencedTabletFile.parsePath, which every input and the output run
// through on the Java side, so a plan built on one names a file the
// manager could never accept.
func TestTranslateRefusesUnparsableFilePaths(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
		want string
	}{
		{"no scheme", "/accumulo/tables/2/t-0001/F0002.rf", "no URI scheme"},
		{"invalid scheme", "9dfs://nn/accumulo/tables/2/t-0001/F0002.rf", "invalid URI scheme"},
		{"authority only", "hdfs://nn", "no absolute path"},
		{"too few segments", "hdfs://nn/tables/2/F0002.rf", "is not shaped"},
		{"wrong tables dir", "hdfs://nn/accumulo/tablets/2/t-0001/F0002.rf", "tables directory name"},
		// A non-normalized twin of files[0]: were it accepted, the raw
		// string would miss the duplicate check and every cell in the
		// file would be merged twice.
		{"double slash", "hdfs://nn/accumulo/tables/2//t-0001/F0001.rf", "empty segment"},
		{"dot segment", "hdfs://nn/accumulo/tables/2/./t-0001/F0002.rf", `"." segment`},
		{"parent segment", "hdfs://nn/accumulo/tables/2/t-0001/../t-0002/F0002.rf", `".." segment`},
		{"trailing slash", "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf/", "empty segment"},
		{"invalid file name", "hdfs://nn/accumulo/tables/2/t-0001/F+0002.rf", "invalid characters"},
		{"fragment", "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf#x", `"#"`},
		{"query", "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf?x=1", `"?"`},
		// URI.create is the strict parser, so these never reach
		// parsePath: deserialize throws while building the Path.
		{"unescaped space", "hdfs://nn/accumulo/tables/2/t 0001/F0002.rf", `" " at offset 29 must be percent-escaped`},
		{"control byte", "hdfs://nn/accumulo/tables/2/t-0001/\x7fF0002.rf", "must be percent-escaped"},
		{"non-ascii space", "hdfs://nn/accumulo/tables/2/t\u00a0/F0002.rf", "must be percent-escaped"},
		{"non-ascii control", "hdfs://nn/accumulo/tables/2/t\u0085/F0002.rf", "must be percent-escaped"},
		{"truncated escape", "hdfs://nn/accumulo/tables/2/t-0001%/F0002.rf", "truncated or non-hex escape at offset 34"},
		{"non-hex escape", "hdfs://nn/accumulo/tables/2/t-%ZZ01/F0002.rf", "truncated or non-hex escape at offset 30"},
		{"escape at end", "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf%2", "truncated or non-hex escape at offset 43"},
		// A well-formed escape parses, but parsePath rebuilds these four
		// segments from the decoded path and looks for the result in the
		// raw URI, so it cannot survive there.
		{"escaped tablet directory", "hdfs://nn/accumulo/tables/2/t%2D0001/F0002.rf", `escapes "%2D" inside the path`},
		{"escaped table id", "hdfs://nn/accumulo/tables/%32/t-0001/F0002.rf", `escapes "%32" inside the path`},
		// URI allows brackets only around an IPv6 host.
		{"bracket in a segment", "hdfs://nn/accumulo/tables/2/t[0]/F0002.rf", `"[" at offset 29 is legal only around an IPv6 literal host`},
		{"bracket with no authority", "hdfs:/accumulo/tables/2/t[0]/F0002.rf", "legal only around an IPv6 literal host"},
		// A bracket in the authority is not enough: parseServer runs the
		// IPv6 grammar over what it encloses, and parseAuthority cannot
		// fall back to a registry-based authority because REG_NAME has
		// no brackets, so it rethrows the literal's failure.
		{"not an ipv6 host", "hdfs://[not-ip]/accumulo/tables/2/t-0001/F0002.rf", `"not-ip" is not an IPv6 address`},
		{"unclosed ipv6 host", "hdfs://[fe80::1/accumulo/tables/2/t-0001/F0002.rf", "no closing bracket"},
		{"closing bracket alone", "hdfs://nn]/accumulo/tables/2/t-0001/F0002.rf", "does not open an IPv6 literal host"},
		{"bracket after the host", "hdfs://nn[::1]/accumulo/tables/2/t-0001/F0002.rf", "does not open an IPv6 literal host"},
		{"empty ipv6 host", "hdfs://[]/accumulo/tables/2/t-0001/F0002.rf", "IPv6 literal host is empty"},
		// The grammar admits IPv4 only as the tail of an IPv6 address,
		// so a bare one never reaches the 16 bytes the parser counts.
		{"bracketed ipv4", "hdfs://[10.0.0.1]/accumulo/tables/2/t-0001/F0002.rf", "is an IPv4 address"},
		{"junk after the literal", "hdfs://[::1]x/accumulo/tables/2/t-0001/F0002.rf", `"x" follows an IPv6 literal host`},
		{"non-numeric port", "hdfs://[::1]:80a/accumulo/tables/2/t-0001/F0002.rf", `port "80a" after an IPv6 literal host`},
		{"empty scope id", "hdfs://[fe80::1%]/accumulo/tables/2/t-0001/F0002.rf", "with no scope id"},
		{"invalid scope id", "hdfs://[fe80::1%eth-0]/accumulo/tables/2/t-0001/F0002.rf", `scope id "eth-0" contains "-"`},
		{"bad literal behind userinfo", "hdfs://u@[not-ip]/accumulo/tables/2/t-0001/F0002.rf", "is not an IPv6 address"},
		{"bracket in the userinfo", "hdfs://u[x]@nn/accumulo/tables/2/t-0001/F0002.rf", "does not open an IPv6 literal host"},
		{"bracket after the port", "hdfs://[::1]:90/accumulo/tables/2/t-0001/F[0].rf", "legal only around an IPv6 literal host"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Files[1].MetadataFileEntry = storedFile(tt.path)

			r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
			if !strings.Contains(r.Detail, tt.want) {
				t.Fatalf("detail = %q, want it to contain %q", r.Detail, tt.want)
			}
		})
	}
}

// TestTranslateRefusesUnparsableOutputPath pins that the output runs
// through the same shape check. The suffix alone is not enough: the
// manager renames this file and stores it as a StoredTabletFile, so a
// name parsePath rejects would strand the output where nothing can
// reference it.
func TestTranslateRefusesUnparsableOutputPath(t *testing.T) {
	job := validJob()
	job.OutputFile = "garbage.rf" + tmpSuffix(testECID)

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "outputFile")
	if !strings.Contains(r.Detail, "no URI scheme") {
		t.Fatalf("detail = %q, want it to name the missing scheme", r.Detail)
	}
}

// TestTranslateRefusesAnOutputThatIsNotUTF8 reaches the one path a
// StoredTabletFile entry cannot: the output comes off the wire as a
// Thrift string, which is a Go string of arbitrary bytes, while a JSON
// decode has already replaced anything invalid with U+FFFD.
func TestTranslateRefusesAnOutputThatIsNotUTF8(t *testing.T) {
	job := validJob()
	job.OutputFile = "hdfs://nn/accumulo/tables/2/t-0001/\xffC0003.rf" + tmpSuffix(testECID)

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "outputFile")
	if !strings.Contains(r.Detail, "is not valid UTF-8") {
		t.Fatalf("detail = %q, want it to name the invalid encoding", r.Detail)
	}
}

// TestTranslateRefusesAnEarlierTmpMarker pins the rename the manager
// would perform. computeCompactionFileDest truncates the whole
// normalized path at the *first* "_tmp", not at the trailing marker, so
// any earlier occurrence — a tablet directory, or the base name itself —
// makes the committed destination a truncated path that names a
// different tablet or no file at all. The temp file would be written
// correctly and the commit would still be wrong.
func TestTranslateRefusesAnEarlierTmpMarker(t *testing.T) {
	for _, tt := range []struct {
		name string
		out  string
		want string
	}{
		{
			"in the tablet directory",
			"hdfs://nn/accumulo/tables/2/t-0001_tmp/C0003.rf",
			"hdfs://nn/accumulo/tables/2/t-0001",
		},
		{
			"in the base name",
			"hdfs://nn/accumulo/tables/2/t-0001/C0003_tmp.rf",
			"hdfs://nn/accumulo/tables/2/t-0001/C0003",
		},
		{
			"in the volume",
			"hdfs://nn/accumulo_tmp/tables/2/t-0001/C0003.rf",
			"hdfs://nn/accumulo",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.OutputFile = tt.out + tmpSuffix(testECID)

			r := assertRefused(t, job, Options{}, ClassMalformedJob, "outputFile")
			if !strings.Contains(r.Detail, "would commit as "+strconv.Quote(tt.want)) {
				t.Fatalf("detail = %q, want it to name the truncated destination %q", r.Detail, tt.want)
			}
		})
	}
}

// TestTranslateAcceptsAnUnauthoritativeVolume keeps the path check from
// overreaching: parsePath takes everything before /tables as the volume
// and does not care what it is, so a job from a second configured volume
// must still translate.
func TestTranslateAcceptsAnUnauthoritativeVolume(t *testing.T) {
	const path = "hdfs://other-nn:8020/srv/vol2/accumulo/tables/2/t-0001/F0002.rf"
	job := validJob()
	job.Files[1].MetadataFileEntry = storedFile(path)

	plan := mustTranslate(t, job, Options{})
	if got := plan.Inputs[1].Path; got != path {
		t.Fatalf("input path = %q, want the second volume's path", got)
	}
}

// TestTranslateAcceptsAnEscapeInTheVolume is the other side of the
// escape rule. parsePath copies the volume out of the raw URI without
// decoding it, so an escape there round-trips and the path parses; only
// the four segments it rebuilds from the decoded path cannot carry one.
func TestTranslateAcceptsAnEscapeInTheVolume(t *testing.T) {
	const path = "hdfs://nn/vol%20two/accumulo/tables/2/t-0001/F0002.rf"
	job := validJob()
	job.Files[1].MetadataFileEntry = storedFile(path)

	plan := mustTranslate(t, job, Options{})
	if got := plan.Inputs[1].Path; got != path {
		t.Fatalf("input path = %q, want %q", got, path)
	}
}

// TestTranslateAcceptsEveryIPv6HostURIDoes keeps the literal check as
// permissive as parseIPv6Reference: an IPv4 tail, the all-zeros
// compression, a fully expanded address and an empty port are all in
// the grammar, and refusing any of them would strand a real namenode.
func TestTranslateAcceptsEveryIPv6HostURIDoes(t *testing.T) {
	for _, host := range []string{
		"[::ffff:10.0.0.1]",
		"[::]",
		"[2001:0db8:0000:0000:0000:0000:0000:0001]",
		"[::1]:",
	} {
		t.Run(host, func(t *testing.T) {
			path := "hdfs://" + host + "/accumulo/tables/2/t-0001/F0002.rf"
			job := validJob()
			job.Files[1].MetadataFileEntry = storedFile(path)

			plan := mustTranslate(t, job, Options{})
			if got := plan.Inputs[1].Path; got != path {
				t.Fatalf("input path = %q, want %q", got, path)
			}
		})
	}
}

// TestTranslateRefusesAnIPv6ScopeIDShoalCannotOpen covers the gap
// between the two parsers. URI$Parser.parseServer reads "%" inside the
// brackets as the scope-id introducer, so java.net.URI takes these
// hosts verbatim and Accumulo can name the file. net/url.Parse, which
// is what internal/storage/hdfs's Backend.resolve calls, has no such
// rule: the "%" is an escape, and a scope id is not two hex digits, so
// the parse fails. The job is refused as a capability gap rather than
// declared executable and left to die on its first read.
//
// Only the unencoded spelling is asserted. "%25" decodes to a "%" and
// leaves an empty zone, which Go 1.25 accepts and Go 1.26 rejects —
// pinning either answer here would pin a toolchain, and the refusal
// deliberately follows whichever parser the binary was built with.
func TestTranslateRefusesAnIPv6ScopeIDShoalCannotOpen(t *testing.T) {
	for _, host := range []string{
		"[fe80::1%eth0]",
		"[fe80::1%en_0.1]",
	} {
		t.Run(host, func(t *testing.T) {
			path := "hdfs://" + host + "/accumulo/tables/2/t-0001/F0002.rf"
			job := validJob()
			job.Files[1].MetadataFileEntry = storedFile(path)

			r := assertRefused(t, job, Options{}, ClassUnsupportedVolume, "files[1]")
			if !strings.Contains(r.Detail, "cannot parse this path") {
				t.Fatalf("detail = %q, want it to name the parse failure", r.Detail)
			}
		})
	}
}

// TestTranslateRefusesAnOutputVolumeShoalCannotOpen holds the same gate
// on the output. An unopenable output is worse than an unopenable
// input: the compaction would run to completion and only fail on the
// write, after every input had been read.
func TestTranslateRefusesAnOutputVolumeShoalCannotOpen(t *testing.T) {
	job := validJob()
	job.OutputFile = "hdfs://[fe80::1%eth0]/accumulo/tables/2/t-0001/C0003.rf_tmp_" + testECID

	r := assertRefused(t, job, Options{}, ClassUnsupportedVolume, "outputFile")
	if !strings.Contains(r.Detail, "cannot parse this path") {
		t.Fatalf("detail = %q, want it to name the parse failure", r.Detail)
	}
}

// TestTranslateRefusesEscapesThatNormalizeAway is the alias the raw
// segment scan cannot see. new Path(URI) is URI.normalize(), which
// resolves segments in the *decoded* path, so "%2e%2e" is a ".." that
// disappears and "%2f%2f" collapses to one separator: the paths below
// are the same Hadoop Path as the plain spelling of the same file.
// pathIdentity decodes escapes but does not resolve segments, so
// without this refusal a job could name one file twice and merge every
// cell in it twice, or aim its output at a file it was still reading.
func TestTranslateRefusesEscapesThatNormalizeAway(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
		want string
	}{
		{
			"parent segment",
			"hdfs://nn/vol/x/%2e%2e/accumulo/tables/2/t-0001/F0002.rf",
			`decodes to a ".." segment`,
		},
		{
			"current segment",
			"hdfs://nn/vol/%2E/accumulo/tables/2/t-0001/F0002.rf",
			`decodes to a "." segment`,
		},
		{
			"doubled separator",
			"hdfs://nn/vol%2f%2faccumulo/tables/2/t-0001/F0002.rf",
			"empty segment",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Files[1].MetadataFileEntry = storedFile(tt.path)

			r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
			if !strings.Contains(r.Detail, tt.want) {
				t.Fatalf("detail = %q, want it to contain %q", r.Detail, tt.want)
			}
		})
	}
}

// TestTranslateRefusesAnOutputAliasingAnInputByNormalization is why the
// escape refusal above is not merely tidiness. The output names the
// same Hadoop Path as input 1 once URI.normalize resolves the "%2e%2e",
// so running this job would have overwritten a file it was reading.
func TestTranslateRefusesAnOutputAliasingAnInputByNormalization(t *testing.T) {
	job := validJob()
	job.OutputFile = "hdfs://nn/accumulo/x/%2e%2e/tables/2/t-0001/F0002.rf_tmp_" + testECID

	assertRefused(t, job, Options{}, ClassMalformedJob, "outputFile")
}

// TestTranslateAcceptsAVisibleNonASCIIPath keeps the character check
// from refusing a volume named in a non-Latin script.
// URI$Parser.scanEscape lets an unescaped character above U+0080
// through as long as it is neither a space character nor an ISO
// control, so java.net.URI accepts these and so must shoal.
func TestTranslateAcceptsAVisibleNonASCIIPath(t *testing.T) {
	const path = "hdfs://nn/données/accumulo/tables/2/t-0001/F0002.rf"
	job := validJob()
	job.Files[1].MetadataFileEntry = storedFile(path)

	plan := mustTranslate(t, job, Options{})
	if got := plan.Inputs[1].Path; got != path {
		t.Fatalf("input path = %q, want %q", got, path)
	}
}

// TestTranslateAcceptsAnIPv6Volume keeps the URI character check from
// refusing a namenode addressed by an IPv6 literal, which is the one
// place java.net.URI allows brackets.
func TestTranslateAcceptsAnIPv6Volume(t *testing.T) {
	const path = "hdfs://[2001:db8::1]:9000/accumulo/tables/2/t-0001/F0002.rf"
	job := validJob()
	job.Files[1].MetadataFileEntry = storedFile(path)

	plan := mustTranslate(t, job, Options{})
	if got := plan.Inputs[1].Path; got != path {
		t.Fatalf("input path = %q, want %q", got, path)
	}
}

// TestTranslateRefusesNonRFileInputs pins the input side of the format
// gate. compaction.Compact opens every input through bcfile/rfile with
// no dispatch on type, so a tablet holding anything else would hand the
// composer a file it cannot open — and an .rf output does not rule that
// out, since the two are configured separately.
func TestTranslateRefusesNonRFileInputs(t *testing.T) {
	job := validJob()
	job.Files[1].MetadataFileEntry =
		storedFile("hdfs://nn/accumulo/tables/2/t-0001/F0002.parquet")

	r := assertRefused(t, job, Options{}, ClassUnsupportedProperty, "files[1]")
	if !strings.Contains(r.Detail, "parquet") {
		t.Fatalf("detail = %q, want it to name the format", r.Detail)
	}
}

// TestTranslateReportsFencesBeforeInputFormat keeps the pass-2 ordering
// stable when an input has both problems: neither is fixable by the
// operator, but the fence is the one Accumulo can be asked about.
func TestTranslateReportsFencesBeforeInputFormat(t *testing.T) {
	job := validJob()
	job.Files[1].MetadataFileEntry =
		fencedFile("hdfs://nn/accumulo/tables/2/t-0001/F0002.parquet", "d", "k")

	assertRefused(t, job, Options{}, ClassRangedInputFile, "files[1]")
}

// TestTranslateAllowsOnePathUnderDistinctFences is the counterpart to
// the duplicate check. A StoredTabletFile is its path *and* its range,
// and Accumulo deliberately references one file under several disjoint
// ranges, so such a job is a legitimate ranged job: it has to reach the
// capability pass and come back as RangedInputFileUnsupported, not as a
// malformed duplicate.
func TestTranslateAllowsOnePathUnderDistinctFences(t *testing.T) {
	const path = "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf"
	job := validJob()
	job.Files = append(job.Files,
		&tabletserver.InputFile{MetadataFileEntry: fencedFile(path, "a", "f"), Size: 8, Entries: 1},
		&tabletserver.InputFile{MetadataFileEntry: fencedFile(path, "f", "m"), Size: 8, Entries: 1},
	)

	assertRefused(t, job, Options{}, ClassRangedInputFile, "files[2]")
}

// TestTranslateRefusesOnePathUnderTheSameFence closes the other half:
// the same reference twice is still a duplicate, and equivalent
// spellings of one range must not defeat the check — Base64.getUrlDecoder
// accepts padded and unpadded input alike.
func TestTranslateRefusesOnePathUnderTheSameFence(t *testing.T) {
	const path = "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf"
	padded := base64.URLEncoding.EncodeToString([]byte{0x01, 'd'})
	unpadded := strings.TrimRight(padded, "=")
	if padded == unpadded {
		t.Fatalf("fixture error: %q needs padding for this test to mean anything", padded)
	}
	job := validJob()
	job.Files = append(job.Files,
		&tabletserver.InputFile{
			MetadataFileEntry: fmt.Sprintf(`{"path":"%s","startRow":"%s","endRow":""}`, path, padded),
		},
		&tabletserver.InputFile{
			MetadataFileEntry: fmt.Sprintf(`{"path":"%s","startRow":"%s","endRow":""}`, path, unpadded),
		},
	)

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[3]")
	if !strings.Contains(r.Detail, "duplicates files[2]") {
		t.Fatalf("detail = %q, want it to name the entry it duplicates", r.Detail)
	}
}

// TestTranslateRefusesOnePathSpelledWithADifferentCase pins that the
// duplicate check folds what Hadoop folds. java.net.URI.equals compares
// the scheme and host case-insensitively, and so does this repository's
// HDFS client, so HDFS://NN/x and hdfs://nn/x open one file — while
// Accumulo's own ReferencedTabletFile compares normalized strings and
// would not notice. Reading it twice would merge every cell in it twice.
func TestTranslateRefusesOnePathSpelledWithADifferentCase(t *testing.T) {
	job := validJob()
	job.Files = append(job.Files, &tabletserver.InputFile{
		MetadataFileEntry: storedFile("HDFS://NN/accumulo/tables/2/t-0001/F0001.rf"),
		Size:              8, Entries: 1,
	})

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[2]")
	if !strings.Contains(r.Detail, "duplicates files[0]") {
		t.Fatalf("detail = %q, want it to name the entry it duplicates", r.Detail)
	}
}

// TestTranslateRefusesAnOutputAliasingAnInputByCase closes the same hole
// on the output side, where the consequence is worse: the manager
// renames the temp file at commit, so an output whose committed name
// folds onto an input would overwrite a file the tablet still
// references.
func TestTranslateRefusesAnOutputAliasingAnInputByCase(t *testing.T) {
	for _, tt := range []struct {
		name string
		out  string
		want string
	}{
		{
			"temp name aliases an input",
			"HDFS://NN/accumulo/tables/2/t-0001/F0001.rf",
			"is also files[0]",
		},
		{
			"committed name aliases an input",
			"HDFS://NN/accumulo/tables/2/t-0001/F0001.rf" + tmpSuffix(testECID),
			"which is also files[0]",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.OutputFile = tt.out

			r := assertRefused(t, job, Options{}, ClassMalformedJob, "outputFile")
			if !strings.Contains(r.Detail, tt.want) {
				t.Fatalf("detail = %q, want it to contain %q", r.Detail, tt.want)
			}
		})
	}
}

// TestPathIdentityFoldsWhatOpensOneFile pins the folding rule directly,
// against the measure that matters: what hdfs.Backend.resolve keeps.
// It keeps u.Host, compared with EqualFold, and url.Parse's decoded
// u.Path — so the path stays case-sensitive, the port stays, and
// userinfo drops out.
//
// Each row is a pair of spellings that must share one identity. The
// assertion is on that equality rather than on the identity string
// itself, because the string is a key: pinning its shape would freeze
// an encoding the rule does not depend on.
func TestPathIdentityFoldsWhatOpensOneFile(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		same string
	}{
		{"scheme and host", "HDFS://NN/A/B.rf", "hdfs://nn/A/B.rf"},
		{"host with a port", "HDFS://NN:9000/A/B.rf", "hdfs://nn:9000/A/B.rf"},
		// Backend.resolve never looks at u.User, so a credential in the
		// path selects nothing.
		{"userinfo drops out", "hdfs://User@NN/A/B.rf", "hdfs://nn/A/B.rf"},
		{"userinfo with a password", "hdfs://u:p@NN:90/A/B.rf", "hdfs://nn:90/A/B.rf"},
		{"ipv6 literal", "HDFS://[2001:DB8::1]:9000/A/B.rf", "hdfs://[2001:db8::1]:9000/A/B.rf"},
		{"userinfo before an ipv6 literal", "hdfs://u@[2001:DB8::1]/A/B.rf", "hdfs://[2001:db8::1]/A/B.rf"},
		{"no authority", "FILE:/A/B.rf", "file:/A/B.rf"},
		// Backend.resolve keeps u.Host, which is empty for all three of
		// these, so they name one file and get one identity.
		{"empty authority", "HDFS:///A/B.rf", "hdfs:/A/B.rf"},
		{"userinfo and nothing else", "hdfs://alice@/A/B.rf", "hdfs:/A/B.rf"},
		{"empty authority and no path", "HDFS://", "hdfs:"},
		{"authority runs to the end", "HDFS://NN", "hdfs://nn"},
		{"no scheme at all", "/A/B.rf", "/A/B.rf"},
		// Escapes are decoded because that is the path shoal's HDFS
		// backend resolves: Backend.resolve hands url.Parse's decoded
		// Path to the namenode.
		{"escape in the volume", "hdfs://nn/v%6Fl/A/B.rf", "hdfs://nn/vol/A/B.rf"},
		{"escape hex case", "hdfs://nn/v%6fl/A/B.rf", "hdfs://nn/vol/A/B.rf"},
		{"escaped separator", "hdfs://nn/a%2Fb/B.rf", "hdfs://nn/a/b/B.rf"},
		// url.Parse decodes the host as well, so an escaped non-ASCII
		// host byte and the byte itself are one authority to resolve.
		// (Escaping an ASCII byte there is a URI RFC 3986 forbids and
		// url.Parse rejects; see the keeps-apart test.)
		{"escape in the host", "hdfs://%C3%89/A/B.rf", "hdfs://\u00c9/A/B.rf"},
		// EqualFold's classes are wider than ToLower's, and EqualFold is
		// what resolve compares hosts with.
		{"kelvin sign", "hdfs://%E2%84%AA/A/B.rf", "hdfs://k/A/B.rf"},
		{"greek final sigma", "hdfs://%CF%82/A/B.rf", "hdfs://%CE%A3/A/B.rf"},
		{"greek small sigma", "hdfs://%CF%83/A/B.rf", "hdfs://%CE%A3/A/B.rf"},
		// A scheme-less path is one Backend.resolve returns verbatim,
		// without decoding, so its identity is its exact spelling.
		{"escape with no scheme", "/v%6Fl/B.rf", "/v%6Fl/B.rf"},
		// A URI url.Parse rejects is one resolve cannot open either, so
		// its exact spelling is its identity and checkVolumeCapability
		// gets to name the real defect in pass 2.
		{"truncated escape", "hdfs://nn/vol%2/B.rf", "hdfs://nn/vol%2/B.rf"},
		{"non-hex escape", "hdfs://nn/vol%zz/B.rf", "hdfs://nn/vol%zz/B.rf"},
		{"escape at the very end", "hdfs://nn/vol/B.rf%", "hdfs://nn/vol/B.rf%"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if pathIdentity(tt.in) != pathIdentity(tt.same) {
				t.Fatalf("pathIdentity(%q) and pathIdentity(%q) differ, want one identity",
					tt.in, tt.same)
			}
		})
	}
}

// TestPathIdentityKeepsApartWhatOpensDifferentFiles is the other half of
// the rule: everything Backend.resolve distinguishes must stay
// distinct, or a job would be refused as a duplicate over two real
// files.
func TestPathIdentityKeepsApartWhatOpensDifferentFiles(t *testing.T) {
	for _, tt := range []struct {
		name string
		a, b string
	}{
		{"path case", "hdfs://nn/A/B.rf", "hdfs://nn/a/b.rf"},
		{"port", "hdfs://nn:9000/A/B.rf", "hdfs://nn/A/B.rf"},
		{"host", "hdfs://nn/A/B.rf", "hdfs://other/A/B.rf"},
		{"scheme", "hdfs://nn/A/B.rf", "file://nn/A/B.rf"},
		// Percent-escaping an ASCII byte in a host is invalid per RFC
		// 3986, and url.Parse rejects it, so resolve cannot open it
		// either and it does not fold into the host it spells.
		{"ascii escape in the host", "hdfs://%6Ee/A/B.rf", "hdfs://ne/A/B.rf"},
		// The escape stays in a scheme-less path, so it is not the file
		// the decoded spelling names.
		{"no scheme keeps its escapes", "/v%6Fl/B.rf", "/vol/B.rf"},
		// A URI resolve cannot open does not fold into one it can.
		{"truncated escape", "hdfs://nn/vol%2/B.rf", "hdfs://nn/vol/B.rf"},
		{"opaque", "hdfs:accumulo/tables/2/t/F.rf", "hdfs:/accumulo/tables/2/t/F.rf"},
		{"query", "hdfs://nn/A/B.rf?v=1", "hdfs://nn/A/B.rf"},
		{"fragment", "hdfs://nn/A/B.rf#f", "hdfs://nn/A/B.rf"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if pathIdentity(tt.a) == pathIdentity(tt.b) {
				t.Fatalf("pathIdentity(%q) and pathIdentity(%q) are one identity, want two",
					tt.a, tt.b)
			}
		})
	}
}

// TestPathIdentityFoldsHostsExactlyLikeEqualFold checks the host rule
// against strings.EqualFold itself, which is the comparison
// hdfs.Backend.resolve makes against its configured namenode. Every
// pair EqualFold calls equal must share an identity and every pair it
// does not must not; ToLower fails the sigma and Kelvin rows.
func TestPathIdentityFoldsHostsExactlyLikeEqualFold(t *testing.T) {
	hosts := []string{
		"nn", "NN", "Nn", "nn:9000", "NN:9000", "other", "",
		"\u03a3", "\u03c3", "\u03c2", // capital, small and final sigma
		"k", "K", "\u212a", // ASCII k and the Kelvin sign
		"stra\u00dfe", "STRASSE",
		"[2001:db8::1]", "[2001:DB8::1]",
	}
	for _, a := range hosts {
		for _, b := range hosts {
			want := strings.EqualFold(a, b)
			got := pathIdentity("hdfs://"+a+"/A/B.rf") == pathIdentity("hdfs://"+b+"/A/B.rf")
			if got != want {
				t.Fatalf("identity(%q) == identity(%q) is %v, but EqualFold says %v",
					a, b, got, want)
			}
		}
	}
}

// TestTranslateRefusesOnePathSpelledWithAnEscape is the escape twin of
// the case-folding duplicate test. checkTabletFilePath allows an escape
// in the volume, and shoal's HDFS backend resolves url.Parse's decoded
// Path, so /v%6Fl/... and /vol/... are one file on the namenode.
func TestTranslateRefusesOnePathSpelledWithAnEscape(t *testing.T) {
	job := validJob()
	job.Files = []*tabletserver.InputFile{
		{MetadataFileEntry: storedFile("hdfs://nn/vol/accumulo/tables/2/t-0001/F0001.rf"), Size: 8, Entries: 1},
		{MetadataFileEntry: storedFile("hdfs://nn/v%6Fl/accumulo/tables/2/t-0001/F0001.rf"), Size: 8, Entries: 1},
	}

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
	if !strings.Contains(r.Detail, "duplicates files[0]") {
		t.Fatalf("detail = %q, want it to name the entry it duplicates", r.Detail)
	}
}

// TestTranslateRefusesAnOutputAliasingAnInputByEscape closes the same
// hole on the output side, where the manager's rename would land on a
// file the tablet still references.
func TestTranslateRefusesAnOutputAliasingAnInputByEscape(t *testing.T) {
	job := validJob()
	job.Files = []*tabletserver.InputFile{
		{MetadataFileEntry: storedFile("hdfs://nn/vol/accumulo/tables/2/t-0001/F0001.rf"), Size: 8, Entries: 1},
	}
	job.OutputFile = "hdfs://nn/v%6Fl/accumulo/tables/2/t-0001/F0001.rf" + tmpSuffix(testECID)

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "outputFile")
	if !strings.Contains(r.Detail, "which is also files[0]") {
		t.Fatalf("detail = %q, want it to name the input it commits over", r.Detail)
	}
}

// TestTranslateRefusesOnePathSpelledWithUserinfo closes the last
// spelling that selects one file twice. hdfs.Backend.resolve never
// reads u.User, so a credential in a tablet file path picks nothing —
// two entries that differ only there open the same RFile.
func TestTranslateRefusesOnePathSpelledWithUserinfo(t *testing.T) {
	job := validJob()
	job.Files = []*tabletserver.InputFile{
		{MetadataFileEntry: storedFile("hdfs://nn/accumulo/tables/2/t-0001/F0001.rf"), Size: 8, Entries: 1},
		{MetadataFileEntry: storedFile("hdfs://alice@nn/accumulo/tables/2/t-0001/F0001.rf"), Size: 8, Entries: 1},
	}

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
	if !strings.Contains(r.Detail, "duplicates files[0]") {
		t.Fatalf("detail = %q, want it to name the entry it duplicates", r.Detail)
	}
}

// TestTranslateRefusesAnOutputAliasingAnInputByUserinfo is the same
// spelling on the output side, where the manager's rename would land on
// a file the tablet still references.
func TestTranslateRefusesAnOutputAliasingAnInputByUserinfo(t *testing.T) {
	job := validJob()
	job.Files = []*tabletserver.InputFile{
		{MetadataFileEntry: storedFile("hdfs://alice@nn/accumulo/tables/2/t-0001/F0001.rf"), Size: 8, Entries: 1},
	}
	job.OutputFile = "hdfs://nn/accumulo/tables/2/t-0001/F0001.rf" + tmpSuffix(testECID)

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "outputFile")
	if !strings.Contains(r.Detail, "which is also files[0]") {
		t.Fatalf("detail = %q, want it to name the input it commits over", r.Detail)
	}
}

// TestTranslateRefusesOnePathSpelledWithAnEmptyAuthority: hdfs:/p and
// hdfs:///p both parse to an empty host and the same path, so
// Backend.resolve opens one file for both. Two inputs spelled that way
// would have every cell in the file merged twice.
func TestTranslateRefusesOnePathSpelledWithAnEmptyAuthority(t *testing.T) {
	job := validJob()
	job.Files = []*tabletserver.InputFile{
		{MetadataFileEntry: storedFile("hdfs:/accumulo/tables/2/t-0001/F0001.rf"), Size: 8, Entries: 1},
		{MetadataFileEntry: storedFile("hdfs:///accumulo/tables/2/t-0001/F0001.rf"), Size: 8, Entries: 1},
	}

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
	if !strings.Contains(r.Detail, "duplicates files[0]") {
		t.Fatalf("detail = %q, want it to name the entry it duplicates", r.Detail)
	}
}

// TestTranslateRefusesAnOutputAliasingAnInputByEmptyAuthority is the
// same spelling on the output side, where the manager's rename would
// land on a file the tablet still references.
func TestTranslateRefusesAnOutputAliasingAnInputByEmptyAuthority(t *testing.T) {
	job := validJob()
	job.Files = []*tabletserver.InputFile{
		{MetadataFileEntry: storedFile("hdfs:///accumulo/tables/2/t-0001/F0001.rf"), Size: 8, Entries: 1},
	}
	job.OutputFile = "hdfs:/accumulo/tables/2/t-0001/F0001.rf" + tmpSuffix(testECID)

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "outputFile")
	if !strings.Contains(r.Detail, "which is also files[0]") {
		t.Fatalf("detail = %q, want it to name the input it commits over", r.Detail)
	}
}

// TestTranslateKeepsBinaryRowsApart pins that a fence identity cannot be
// forged by moving a byte across a delimiter. Every row
// KeyExtent.toDataRange stores is a followingKey(ROW) result and so ends
// in a zero byte, which is exactly the byte a delimited key would use to
// separate the bounds: ("\x00a", "\x00b") and ("\x00a\x00", "b") spell
// the same delimited key, are different ranges, and a delimited key
// would call the second a duplicate of the first and refuse a job
// Accumulo built.
//
// Both bounds start with the zero byte because Range rejects a start key
// past its end, and taking a leading zero byte off the end row is what
// would invert the pair — so this is the shape a legal collision has.
func TestTranslateKeepsBinaryRowsApart(t *testing.T) {
	const path = "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf"
	job := validJob()
	job.Files = append(job.Files,
		&tabletserver.InputFile{MetadataFileEntry: fencedFile(path, "\x00a", "\x00b"), Size: 8, Entries: 1},
		&tabletserver.InputFile{MetadataFileEntry: fencedFile(path, "\x00a\x00", "b"), Size: 8, Entries: 1},
	)

	// Both are fenced, so the job is refused for the fence — the point
	// is that it is not refused as a duplicate.
	r := assertRefused(t, job, Options{}, ClassRangedInputFile, "files[2]")
	if strings.Contains(r.Detail, "duplicates") {
		t.Fatalf("detail = %q, want the fence refusal rather than a duplicate", r.Detail)
	}
}

// TestTranslateRefusesAnInvertedFence: deserialize builds
// Range(startRow, true, endRow, false), whose Key constructor throws
// IllegalArgumentException when the end key sorts first. An entry
// spelled that way is not an input shoal has yet to port — it is one no
// Java compactor could deserialize either, so it has to read as
// malformed rather than as a capability shoal is missing, or an operator
// is told to wait for a feature that would never help.
func TestTranslateRefusesAnInvertedFence(t *testing.T) {
	const path = "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf"
	job := validJob()
	job.Files = append(job.Files,
		&tabletserver.InputFile{MetadataFileEntry: fencedFile(path, "m", "c"), Size: 8, Entries: 1},
	)

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[2]")
	if !strings.Contains(r.Detail, "sorts after") {
		t.Fatalf("detail = %q, want it to name the inversion", r.Detail)
	}
}

// TestTranslateAcceptsAnEmptyFenceRange is the boundary the inversion
// check must not cross. Key(row) is not before Key(row), so Java builds
// Range(row, true, row, false) without complaint — it simply selects
// nothing. That is a fence, so the job is still refused, but as an
// unsupported one rather than a malformed one.
func TestTranslateAcceptsAnEmptyFenceRange(t *testing.T) {
	const path = "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf"
	job := validJob()
	job.Files = append(job.Files,
		&tabletserver.InputFile{MetadataFileEntry: fencedFile(path, "m", "m"), Size: 8, Entries: 1},
	)

	assertRefused(t, job, Options{}, ClassRangedInputFile, "files[2]")
}

// TestTranslateCopiesTheExtentOutOfTheJob: a Plan outlives the RPC that
// produced it, and the extent is the transport's struct. parseIterators
// already copies each option map for this reason; an aliased extent
// would let a recycled job change a plan that was already logged and is
// about to be executed.
func TestTranslateCopiesTheExtentOutOfTheJob(t *testing.T) {
	job := validJob()
	plan := mustTranslate(t, job, Options{})

	if plan.Extent == job.Extent {
		t.Fatal("Plan.Extent is the job's own struct; a recycled job would rewrite the plan")
	}

	// Both spellings of reuse: a new struct in the job, and a mutation
	// through the byte slices the old one handed out.
	table := job.Extent.Table
	job.Extent = &data.TKeyExtent{Table: []byte("99"), PrevEndRow: []byte("x"), EndRow: []byte("z")}
	table[0] = 'Z'

	if got := string(plan.Extent.GetTable()); got != "2" {
		t.Fatalf("Extent.Table = %q after the job was reused, want the translated %q", got, "2")
	}
	if got := string(plan.Extent.GetPrevEndRow()); got != "c" {
		t.Fatalf("Extent.PrevEndRow = %q after the job was reused, want %q", got, "c")
	}
	if got := string(plan.Extent.GetEndRow()); got != "m" {
		t.Fatalf("Extent.EndRow = %q after the job was reused, want %q", got, "m")
	}
}

// TestTranslateKeepsInfiniteExtentBoundsInfinite: an extent spells an
// infinite bound as a missing row, and bytes.Clone keeps nil nil. A copy
// that turned them into empty rows would silently rename the first or
// last tablet of a table to a bounded one.
func TestTranslateKeepsInfiniteExtentBoundsInfinite(t *testing.T) {
	job := validJob()
	job.Extent = &data.TKeyExtent{Table: []byte("2")}

	plan := mustTranslate(t, job, Options{})
	if plan.Extent.GetPrevEndRow() != nil {
		t.Errorf("PrevEndRow = %q, want nil so the tablet keeps an infinite start", plan.Extent.GetPrevEndRow())
	}
	if plan.Extent.GetEndRow() != nil {
		t.Errorf("EndRow = %q, want nil so the tablet keeps an infinite end", plan.Extent.GetEndRow())
	}
}

// TestTranslateRefusesAVolumeNoBackendCanOpen: parsing proves the
// spelling, not that shoal has a reader. hdfs's Backend.resolve is the
// only shoal resolver that interprets a scheme and isHDFSScheme accepts
// exactly one; local's Open hands the string to os.Open verbatim (it
// declares local path semantics, so nothing strips the scheme) and the
// object stores take keys. So a job on any other volume would be
// declared executable and then die on its first read, with the slot
// already spent and no capability reported.
func TestTranslateRefusesAVolumeNoBackendCanOpen(t *testing.T) {
	for _, path := range []string{
		"file:/srv/vol2/accumulo/tables/2/t-0001/F0002.rf",
		"file:///srv/vol2/accumulo/tables/2/t-0001/F0002.rf",
		"s3a://bucket/accumulo/tables/2/t-0001/F0002.rf",
		"ftp://nn/accumulo/tables/2/t-0001/F0002.rf",
	} {
		t.Run(path, func(t *testing.T) {
			job := validJob()
			job.Files[1].MetadataFileEntry = storedFile(path)

			r := assertRefused(t, job, Options{}, ClassUnsupportedVolume, "files[1]")
			if !strings.Contains(r.Detail, "has no shoal backend") {
				t.Fatalf("detail = %q, want it to name the missing backend", r.Detail)
			}
		})
	}
}

// TestTranslateRefusesAnOutputVolumeNoBackendCanOpen is the same gate on
// the write side: an output shoal cannot create is not a compaction it
// can run, however well the path parses.
func TestTranslateRefusesAnOutputVolumeNoBackendCanOpen(t *testing.T) {
	job := validJob()
	job.OutputFile = "file:/srv/vol2/accumulo/tables/2/t-0001/C0003.rf" + tmpSuffix(testECID)

	assertRefused(t, job, Options{}, ClassUnsupportedVolume, "outputFile")
}

// TestTranslateRefusesAnOutputUnderAnotherTable pins that the output
// belongs to the extent's own table.
//
// TabletNameGenerator.getNextDataFilenameForMajc builds the name under
// chooseTabletDir, which is <volume>/tables/<extent.tableId()>/<dir>, so
// a job naming a different table has fields that disagree with each
// other — and executing it would write into another table's directory
// and have the manager commit the rename there.
func TestTranslateRefusesAnOutputUnderAnotherTable(t *testing.T) {
	job := validJob()
	job.OutputFile = "hdfs://nn/accumulo/tables/3/t-0001/C0003.rf" + tmpSuffix(testECID)

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "outputFile")
	if !strings.Contains(r.Detail, `under table "3"`) ||
		!strings.Contains(r.Detail, `extent assigns table "2"`) {
		t.Fatalf("detail = %q, want it to name both tables", r.Detail)
	}
}

// TestTranslateAcceptsInputsFromAnotherTable is the deliberate other
// half: a cloned table's tablets reference the *source* table's files.
// MetadataTableUtil.createCloneMutation copies the source tablet's file
// column qualifiers verbatim, so requiring inputs to sit under the
// extent's table would leave every cloned tablet permanently
// uncompactable.
func TestTranslateAcceptsInputsFromAnotherTable(t *testing.T) {
	const cloned = "hdfs://nn/accumulo/tables/7/t-0001/F0009.rf"
	job := validJob()
	job.Files[1].MetadataFileEntry = storedFile(cloned)

	plan := mustTranslate(t, job, Options{})
	if got := plan.Inputs[1].Path; got != cloned {
		t.Fatalf("input path = %q, want the source table's file %q", got, cloned)
	}
}

// TestTranslateRefusesEntriesTheTwoDecodersReadDifferently closes the
// gap between the exact-name check and the struct decode that follows
// it. Gson binds only the exact member and ignores the other spelling;
// encoding/json falls back to a case-insensitive match and takes
// whichever comes last. An entry carrying both is therefore a fenced
// file to Java and a whole one to shoal — or, with "path", a different
// file altogether.
func TestTranslateRefusesEntriesTheTwoDecodersReadDifferently(t *testing.T) {
	const path = "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf"
	for _, tt := range []struct {
		name  string
		entry string
		want  string
	}{
		{
			"a fence a struct decode would drop",
			fmt.Sprintf(`{"path":"%s","startRow":"%s","endRow":"","StartRow":""}`, path, encodeRow("d")),
			`"StartRow" alongside "startRow"`,
		},
		{
			"a path a struct decode would replace",
			fmt.Sprintf(`{"path":"%s","startRow":"","endRow":"","Path":"hdfs://nn/accumulo/tables/2/t-0001/F0009.rf"}`, path),
			`"Path" alongside "path"`,
		},
		{
			"an end bound a struct decode would drop",
			fmt.Sprintf(`{"path":"%s","startRow":"","endRow":"%s","ENDROW":""}`, path, encodeRow("k")),
			`"ENDROW" alongside "endRow"`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Files[1].MetadataFileEntry = tt.entry

			r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
			if !strings.Contains(r.Detail, tt.want) {
				t.Fatalf("detail = %q, want it to contain %q", r.Detail, tt.want)
			}
		})
	}
}

// TestTranslateRefusesEntriesGsonWouldLeaveNull pins that field names
// are matched the way Gson matches them. encoding/json falls back to a
// case-insensitive match, so a capitalised spelling decodes cleanly here
// while StoredTabletFile.deserialize leaves the field null and
// requireNonNull rejects the entry. A null literal is the same case:
// present on the wire, absent to Gson.
func TestTranslateRefusesEntriesGsonWouldLeaveNull(t *testing.T) {
	const path = "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf"
	for _, tt := range []struct {
		name  string
		entry string
		want  string
	}{
		{"capitalised path", `{"Path":"` + path + `","startRow":"","endRow":""}`, "missing path"},
		{"capitalised startRow", `{"path":"` + path + `","StartRow":"","endRow":""}`, "missing startRow"},
		{"capitalised endRow", `{"path":"` + path + `","startRow":"","EndRow":""}`, "missing endRow"},
		{"null path", `{"path":null,"startRow":"","endRow":""}`, "missing path"},
		{"row is not a string", `{"path":"` + path + `","startRow":7,"endRow":""}`, "startRow is not a string"},
		// Present and non-null, so the exact-name check passes and the
		// shared decoder is the one that rejects it.
		{"path is not a string", `{"path":7,"startRow":"","endRow":""}`, "undecodable StoredTabletFile entry"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Files[1].MetadataFileEntry = tt.entry

			r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
			if !strings.Contains(r.Detail, tt.want) {
				t.Fatalf("detail = %q, want it to contain %q", r.Detail, tt.want)
			}
		})
	}
}

// TestTranslateRefusesOneFenceSpelledWithTrailingBytes is the framing
// half of the same rule. Text.readFields readFully's the declared length
// and stops, so [1,'d'] and [1,'d','x'] deserialize to the identical
// row: they are one StoredTabletFile, and comparing the buffers rather
// than the row would let a caller list it twice and merge every cell in
// it twice.
func TestTranslateRefusesOneFenceSpelledWithTrailingBytes(t *testing.T) {
	const path = "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf"
	exact := base64.URLEncoding.EncodeToString([]byte{0x01, 'd'})
	trailing := base64.URLEncoding.EncodeToString([]byte{0x01, 'd', 'x'})
	job := validJob()
	job.Files = append(job.Files,
		&tabletserver.InputFile{
			MetadataFileEntry: fmt.Sprintf(`{"path":"%s","startRow":"%s","endRow":""}`, path, exact),
		},
		&tabletserver.InputFile{
			MetadataFileEntry: fmt.Sprintf(`{"path":"%s","startRow":"%s","endRow":""}`, path, trailing),
		},
	)

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[3]")
	if !strings.Contains(r.Detail, "duplicates files[2]") {
		t.Fatalf("detail = %q, want it to name the entry it duplicates", r.Detail)
	}
}

// TestTranslateRefusesFenceRowsWithLineBreaks pins that a row Java
// cannot decode is reported as malformed. Go's base64 decoders skip CR
// and LF; Base64.getUrlDecoder throws on them, so without the guard an
// escaped newline would decode here into a plausible fence and be
// refused as an unsupported range — the wrong class, and the wrong
// message for whoever has to fix the entry.
func TestTranslateRefusesFenceRowsWithLineBreaks(t *testing.T) {
	for _, tt := range []struct {
		name string
		row  string
	}{
		{"embedded LF", "AW\nQ="},
		{"embedded CR", "AW\rQ="},
		{"trailing LF", "AWQ=\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Files[1].MetadataFileEntry = fmt.Sprintf(
				`{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0002.rf","startRow":%q,"endRow":""}`,
				tt.row)

			r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
			if !strings.Contains(r.Detail, "line break") {
				t.Fatalf("detail = %q, want it to name the line break", r.Detail)
			}
		})
	}
}

// TestEmptyInputGuards covers the two preconditions the callers already
// satisfy — parseInputs rejects an empty entry and parseOutput an empty
// output before either helper runs, and checkFenceRow returns early on a
// zero-length row. They are checked here rather than deleted because
// neither helper should depend on its caller to stay memory-safe.
func TestEmptyInputGuards(t *testing.T) {
	if err := checkTabletFilePath("", "files[0]"); err == nil {
		t.Fatal("checkTabletFilePath(\"\") = nil, want a refusal")
	}
	if _, _, err := readVInt(nil); err == nil {
		t.Fatal("readVInt(nil) = nil error, want a truncation error")
	}
}

// TestTranslateRefusesMalformedJobs walks the structural checks. Every
// one of these would produce an uncommittable or cell-losing compaction,
// so each must come back as a malformed-job refusal naming its field.
func TestTranslateRefusesMalformedJobs(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*tabletserver.TExternalCompactionJob)
		wantField string
	}{
		{
			name:      "no ecid",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.ExternalCompactionId = "" },
			wantField: "externalCompactionId",
		},
		{
			name:      "ecid not in accumulo form",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.ExternalCompactionId = "compaction-7" },
			wantField: "externalCompactionId",
		},
		{
			name:      "ecid prefix with no uuid",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.ExternalCompactionId = ECIDPrefix },
			wantField: "externalCompactionId",
		},
		{
			// The colon spelling is the one an unrelated javadoc in
			// ExternalCompactionId still shows; PREFIX is the hyphen.
			name:      "ecid using the colon spelling",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.ExternalCompactionId = "ECID:" + testUUID },
			wantField: "externalCompactionId",
		},
		{
			name:      "ecid suffix is not a uuid",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.ExternalCompactionId = ECIDPrefix + "not-a-uuid" },
			wantField: "externalCompactionId",
		},
		{
			name: "ecid suffix is a truncated uuid",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.ExternalCompactionId = ECIDPrefix + testUUID[:len(testUUID)-1]
			},
			wantField: "externalCompactionId",
		},
		// UUID.fromString takes only the hyphenated spelling: it looks
		// for four dashes and no fifth. Every one of these parses in Go,
		// so accepting them would build a plan whose compactionFailed
		// throws in the coordinator's handler and leaves the slot
		// assigned until it times out.
		{
			name: "ecid suffix is bare hex",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.ExternalCompactionId = ECIDPrefix + strings.ReplaceAll(testUUID, "-", "")
			},
			wantField: "externalCompactionId",
		},
		{
			name: "ecid suffix is braced",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.ExternalCompactionId = ECIDPrefix + "{" + testUUID + "}"
			},
			wantField: "externalCompactionId",
		},
		{
			name: "ecid suffix is a urn",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.ExternalCompactionId = ECIDPrefix + "urn:uuid:" + testUUID
			},
			wantField: "externalCompactionId",
		},
		{
			name:      "no extent",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.Extent = nil },
			wantField: "extent",
		},
		{
			name:      "extent without table id",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.Extent.Table = nil },
			wantField: "extent.table",
		},
		{
			name: "inverted extent bounds",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Extent.PrevEndRow = []byte("z")
				j.Extent.EndRow = []byte("m")
			},
			wantField: "extent",
		},
		{
			name:      "no input files",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.Files = nil },
			wantField: "files",
		},
		{
			name:      "nil input file",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.Files[1] = nil },
			wantField: "files[1]",
		},
		{
			name:      "empty input entry",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.Files[1].MetadataFileEntry = "" },
			wantField: "files[1]",
		},
		{
			name:      "negative input size",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.Files[1].Size = -1 },
			wantField: "files[1]",
		},
		{
			name:      "negative input entries",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.Files[1].Entries = -5 },
			wantField: "files[1]",
		},
		{
			name: "duplicate input",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Files[1].MetadataFileEntry = j.Files[0].MetadataFileEntry
			},
			wantField: "files[1]",
		},
		{
			name: "undecodable stored tablet file",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Files[0].MetadataFileEntry = `{"path":`
			},
			wantField: "files[0]",
		},
		{
			name: "stored tablet file without a path",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Files[0].MetadataFileEntry = `{"startRow":"","endRow":""}`
			},
			wantField: "files[0]",
		},
		{
			name:      "no output file",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.OutputFile = "" },
			wantField: "outputFile",
		},
		{
			name: "output file is a metadata entry",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.OutputFile = storedFile("hdfs://nn/accumulo/tables/2/t-0001/C0003.rf")
			},
			wantField: "outputFile",
		},
		{
			// The committed name, not the temp name the compactor is
			// told to write; the manager renames the temp file itself.
			name: "output file is not this job's temp name",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.OutputFile = "hdfs://nn/accumulo/tables/2/t-0001/C0003.rf"
			},
			wantField: "outputFile",
		},
		{
			name: "output file carries another job's ecid",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.OutputFile = tmpOutput(ECIDPrefix + "11111111-2222-3333-4444-555555555555")
			},
			wantField: "outputFile",
		},
		{
			name: "output file has the bare pre-3.0 temp suffix",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.OutputFile = "hdfs://nn/accumulo/tables/2/t-0001/C0003.rf_tmp"
			},
			wantField: "outputFile",
		},
		{
			name: "output file aliases an input",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.OutputFile = "hdfs://nn/accumulo/tables/2/t-0001/F0001.rf"
			},
			wantField: "outputFile",
		},
		{
			name:      "unknown compaction kind",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.Kind = tabletserver.TCompactionKind(9) },
			wantField: "kind",
		},
		{
			name:      "user compaction without a fate id",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.Kind = tabletserver.TCompactionKind_USER },
			wantField: "fateId",
		},
		{
			name: "fate id without a transaction uuid",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Kind = tabletserver.TCompactionKind_USER
				j.FateId = &manager.TFateId{Type: manager.TFateInstanceType_USER}
			},
			wantField: "fateId",
		},
		{
			// FateId.fromThrift switches over TFateInstanceType with
			// only USER and META arms; an unknown ordinal throws there
			// and stringifies to "<UNSET>" here.
			name: "fate id with an unknown instance type",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Kind = tabletserver.TCompactionKind_USER
				j.FateId = &manager.TFateId{
					Type:      manager.TFateInstanceType(7),
					TxUUIDStr: "b7f1c0de-0000-4000-8000-00000000abcd",
				}
			},
			wantField: "fateId",
		},
		{
			// UuidUtil.isUUID demands the canonical 36-character form.
			name: "fate id transaction is not a uuid",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Kind = tabletserver.TCompactionKind_USER
				j.FateId = &manager.TFateId{
					Type:      manager.TFateInstanceType_USER,
					TxUUIDStr: "transaction-7",
				}
			},
			wantField: "fateId",
		},
		{
			// uuid.Parse accepts the unhyphenated form; isUUID does not.
			name: "fate id transaction uuid is unhyphenated",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Kind = tabletserver.TCompactionKind_USER
				j.FateId = &manager.TFateId{
					Type:      manager.TFateInstanceType_USER,
					TxUUIDStr: "b7f1c0de00004000800000000000abcd",
				}
			},
			wantField: "fateId",
		},
		{
			// Right length, wrong separator: isUUID demands '-' at
			// offsets 8, 13, 18 and 23.
			name: "fate id transaction uuid has a bad separator",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Kind = tabletserver.TCompactionKind_USER
				j.FateId = &manager.TFateId{
					Type:      manager.TFateInstanceType_USER,
					TxUUIDStr: "b7f1c0de_0000-4000-8000-00000000abcd",
				}
			},
			wantField: "fateId",
		},
		{
			// Right length and separators, non-hex digit.
			name: "fate id transaction uuid has a non-hex digit",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Kind = tabletserver.TCompactionKind_USER
				j.FateId = &manager.TFateId{
					Type:      manager.TFateInstanceType_USER,
					TxUUIDStr: "b7f1c0dg-0000-4000-8000-00000000abcd",
				}
			},
			wantField: "fateId",
		},
		{
			name: "nil iterator setting",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.IteratorSettings = iterConfig(nil)
			},
			wantField: "iteratorSettings[0]",
		},
		{
			name: "iterator without a name",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
					Priority:      20,
					IteratorClass: versClass,
				})
			},
			wantField: "iteratorSettings[0]",
		},
		{
			// setPriority runs first and demands a strictly positive
			// value, so a zero or negative one never reaches the name.
			name: "iterator with a zero priority",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
					Name: "vers", IteratorClass: versClass,
				})
			},
			wantField: "iteratorSettings[0]",
		},
		{
			name: "iterator with a negative priority",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
					Priority: -1, Name: "vers", IteratorClass: versClass,
				})
			},
			wantField: "iteratorSettings[0]",
		},
		{
			// setName rejects a dot outright, because Accumulo builds
			// per-iterator property keys by joining on one.
			name: "iterator name with a dot",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
					Priority: 20, Name: "vers.1", IteratorClass: versClass,
				})
			},
			wantField: "iteratorSettings[0]",
		},
		{
			name: "iterator without a class",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
					Priority: 20, Name: "vers",
				})
			},
			wantField: "iteratorSettings.vers",
		},
		{
			name: "duplicate iterator name",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.IteratorSettings = iterConfig(
					&tabletserver.TIteratorSetting{Priority: 10, Name: "vers", IteratorClass: versClass},
					&tabletserver.TIteratorSetting{Priority: 20, Name: "vers", IteratorClass: versClass},
				)
			},
			wantField: "iteratorSettings.vers",
		},
		{
			name: "non-numeric block size override",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Overrides = map[string]string{propCompressBlockSize: "big"}
			},
			wantField: "overrides[" + propCompressBlockSize + "]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			tt.mutate(job)
			assertRefused(t, job, Options{}, ClassMalformedJob, tt.wantField)
		})
	}
}

func TestTranslateRefusesNilJob(t *testing.T) {
	assertRefused(t, nil, Options{}, ClassMalformedJob, "")
}

// TestTranslateAcceptsBothFateInstanceTypes pins the enum arms Java
// accepts: FateId.fromThrift maps USER and META, and the plan records
// FateId's canonical "FATE:TYPE:uuid" form, the spelling
// FateId.from(String) round-trips.
func TestTranslateAcceptsBothFateInstanceTypes(t *testing.T) {
	const txUUID = "b7f1c0de-0000-4000-8000-00000000abcd"
	for _, tt := range []struct {
		name string
		typ  manager.TFateInstanceType
		want string
	}{
		{"user", manager.TFateInstanceType_USER, "FATE:USER:" + txUUID},
		{"meta", manager.TFateInstanceType_META, "FATE:META:" + txUUID},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Kind = tabletserver.TCompactionKind_USER
			job.FateId = &manager.TFateId{Type: tt.typ, TxUUIDStr: txUUID}

			plan := mustTranslate(t, job, Options{})
			if plan.FateID != tt.want {
				t.Fatalf("FateID = %q, want %q", plan.FateID, tt.want)
			}
		})
	}
}

// TestTranslateRefusesOutputThatCommitsOverAnInput closes the alias gate
// on the name the file actually ends up with. Shoal writes the temp
// name, but the manager renames it with computeCompactionFileDest,
// which truncates at the *first* "_tmp". An output whose committed name
// is one of the inputs therefore differs from every input while shoal
// looks at it, and destroys one of them at commit — after shoal has
// already reported the compaction done.
func TestTranslateRefusesOutputThatCommitsOverAnInput(t *testing.T) {
	const input = "hdfs://nn/accumulo/tables/2/t-0001/F0001.rf"
	for _, tt := range []struct{ name, output string }{
		{
			name:   "temp name of an input",
			output: input + tmpSuffix(testECID),
		},
		{
			// The manager stops at the first "_tmp", so everything
			// after it — including a second, well-formed temp tail —
			// is discarded.
			name:   "input whose own name contains the marker",
			output: input + "_tmp_x" + tmpSuffix(testECID),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Files[0].MetadataFileEntry = storedFile(input)
			job.OutputFile = tt.output

			r := assertRefused(t, job, Options{}, ClassMalformedJob, "outputFile")
			if !strings.Contains(r.Detail, "commits as "+input) {
				t.Fatalf("detail = %q, want it to name the committed collision", r.Detail)
			}
		})
	}
}

// TestTranslateAcceptsAnOutputThatOnlySharesAPrefix guards the other
// side: a fresh file counter shares the input's directory and most of
// its name, and that is the normal case, not a collision.
func TestTranslateAcceptsAnOutputThatOnlySharesAPrefix(t *testing.T) {
	job := validJob()
	job.Files[0].MetadataFileEntry = storedFile("hdfs://nn/accumulo/tables/2/t-0001/F0001.rf")
	job.OutputFile = "hdfs://nn/accumulo/tables/2/t-0001/F0001x.rf" + tmpSuffix(testECID)

	mustTranslate(t, job, Options{})
}

// TestTranslateKeepsTheCoordinatorsTempOutputName is the wire-shape
// guard for the output path. A real assignment names
// <dir>/<A|C><n>.rf_tmp_<ECID>; shoal must plan to write exactly that,
// not the committed name, because the manager is what renames the temp
// file once it accepts the compaction.
func TestTranslateKeepsTheCoordinatorsTempOutputName(t *testing.T) {
	job := validJob()
	want := "hdfs://nn/accumulo/tables/2/t-0001/A0007.rf_tmp_" + testECID
	job.OutputFile = want

	plan := mustTranslate(t, job, Options{})
	if plan.OutputFile != want {
		t.Fatalf("OutputFile = %q, want the temp name verbatim (%q)", plan.OutputFile, want)
	}
}

// TestTranslateRefusesUnwritableOutputFormat: the extension under the
// temp suffix comes from table.file.type, which the job never states
// directly. Anything but RFile is a table shoal's writer cannot serve.
func TestTranslateRefusesUnwritableOutputFormat(t *testing.T) {
	for _, tt := range []struct{ name, output, wantIn string }{
		{"another file format", "hdfs://nn/accumulo/tables/2/t-0001/C0003.parquet_tmp_" + testECID, "parquet"},
		{"no extension at all", "hdfs://nn/accumulo/tables/2/t-0001/C0003_tmp_" + testECID, "extensionless"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.OutputFile = tt.output

			r := assertRefused(t, job, Options{}, ClassUnsupportedProperty, "outputFile")
			if !strings.Contains(r.Detail, tt.wantIn) {
				t.Fatalf("detail = %q, want it to name %q", r.Detail, tt.wantIn)
			}
		})
	}
}

// TestTranslateRefusesUnportedIterator: an unknown class must fail the
// whole job. Dropping it (which is the right call for the shadow
// resolver, where the output is discarded) would here write an output
// missing that iterator's effect and then replace the inputs with it.
func TestTranslateRefusesUnportedIterator(t *testing.T) {
	job := validJob()
	job.IteratorSettings = iterConfig(
		&tabletserver.TIteratorSetting{
			Priority:      15,
			Name:          "ageoff",
			IteratorClass: "org.apache.accumulo.core.iterators.user.AgeOffFilter",
			Properties:    map[string]string{"ttl": "3600000"},
		},
	)

	r := assertRefused(t, job, Options{}, ClassUnsupportedIterator, "iteratorSettings.ageoff")
	if !strings.Contains(r.Detail, "AgeOffFilter") {
		t.Fatalf("detail = %q, want the unported class named", r.Detail)
	}
}

// TestTranslateRefusesSystemIterator: shoal's composer installs the
// deleting iterator itself, so a job that also lists one would suppress
// tombstones twice.
func TestTranslateRefusesSystemIterator(t *testing.T) {
	for _, tt := range []struct{ name, class string }{
		{"deleting", "org.apache.accumulo.core.iterators.system.DeletingIterator"},
		{"visibility", "org.apache.accumulo.core.iterators.system.VisibilityFilter"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
				Priority: 5, Name: "sys", IteratorClass: tt.class,
			})
			assertRefused(t, job, Options{}, ClassUnsupportedIterator, "iteratorSettings.sys")
		})
	}
}

// TestTranslateRefusesUnusableIteratorOptions proves the dry-run stack
// build is wired: a bad option is caught at translation time, not after
// the compaction has read gigabytes of input.
func TestTranslateRefusesUnusableIteratorOptions(t *testing.T) {
	for _, tt := range []struct{ name, value string }{
		{"not an integer", "many"},
		{"below the legal minimum", "0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
				Priority:      20,
				Name:          "vers",
				IteratorClass: versClass,
				Properties:    map[string]string{"maxVersions": tt.value},
			})
			assertRefused(t, job, Options{}, ClassUnsupportedIterator, "iteratorSettings")
		})
	}
}

// TestTranslateCopiesIteratorOptions guards against aliasing the thrift
// struct: the plan must not change when the job it came from is reused
// or recycled by the transport.
func TestTranslateCopiesIteratorOptions(t *testing.T) {
	job := validJob()
	props := map[string]string{"maxVersions": "3"}
	job.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
		Priority: 20, Name: "vers", IteratorClass: versClass, Properties: props,
	})

	plan := mustTranslate(t, job, Options{})
	props["maxVersions"] = "99"

	if got := plan.Stack[0].Options["maxVersions"]; got != "3" {
		t.Fatalf("plan option = %q after mutating the job, want the translated 3", got)
	}
}

// TestTranslateOrdersIteratorsLikeJava pins the ordering rule to the
// Java one. IteratorConfigUtil.parseIterConf sorts the settings by
// ITER_INFO_COMPARATOR before loadIterators builds the stack, so wire
// order is not stack order and a descending list is a job Java runs
// rather than a broken one.
func TestTranslateOrdersIteratorsLikeJava(t *testing.T) {
	job := validJob()
	job.IteratorSettings = iterConfig(
		&tabletserver.TIteratorSetting{Priority: 30, Name: "late", IteratorClass: latentClass},
		&tabletserver.TIteratorSetting{Priority: 10, Name: "early", IteratorClass: versClass},
	)

	plan := mustTranslate(t, job, Options{})

	if len(plan.Iterators) != 2 {
		t.Fatalf("Iterators = %d, want 2", len(plan.Iterators))
	}
	if plan.Iterators[0].Name != "early" || plan.Iterators[1].Name != "late" {
		t.Fatalf("order = %q then %q, want early (priority 10) then late (priority 30)",
			plan.Iterators[0].Name, plan.Iterators[1].Name)
	}
	if plan.Stack[0].Name != iterrt.IterVersioning || plan.Stack[1].Name != iterrt.IterLatentEdgeDiscovery {
		t.Fatalf("Stack = %+v, want the built stack in the same order as Iterators", plan.Stack)
	}
}

// TestTranslateBreaksIteratorPriorityTiesByName: Java's comparator falls
// back to the iterator name when priorities are equal, which makes the
// order total. Equal priorities are legal there, so shoal must order
// them the same way instead of refusing or trusting wire order.
func TestTranslateBreaksIteratorPriorityTiesByName(t *testing.T) {
	job := validJob()
	job.IteratorSettings = iterConfig(
		&tabletserver.TIteratorSetting{Priority: 10, Name: "zeta", IteratorClass: latentClass},
		&tabletserver.TIteratorSetting{Priority: 10, Name: "alpha", IteratorClass: versClass},
	)

	plan := mustTranslate(t, job, Options{})

	if plan.Iterators[0].Name != "alpha" || plan.Iterators[1].Name != "zeta" {
		t.Fatalf("order = %q then %q, want alpha then zeta",
			plan.Iterators[0].Name, plan.Iterators[1].Name)
	}
}

// TestTranslateBreaksTiesTheWayJavaComparesStrings: iterator names come
// from user configuration, so they are not guaranteed ASCII. Java
// compares by UTF-16 code unit, which puts a supplementary character
// (encoded as a surrogate pair in D800-DFFF) *below* one in E000-FFFF;
// Go's native string order puts it above. Getting this backwards
// silently swaps two equal-priority iterators, which changes the cells
// the compaction writes.
func TestTranslateBreaksTiesTheWayJavaComparesStrings(t *testing.T) {
	const (
		supplementary = "\U00010000" // surrogate pair D800 DC00 in UTF-16
		privateUse    = "\uE000"     // one code unit, numerically above D800
	)

	job := validJob()
	job.IteratorSettings = iterConfig(
		&tabletserver.TIteratorSetting{Priority: 7, Name: privateUse, IteratorClass: latentClass},
		&tabletserver.TIteratorSetting{Priority: 7, Name: supplementary, IteratorClass: versClass},
	)

	plan := mustTranslate(t, job, Options{})

	if privateUse >= supplementary {
		t.Fatal("test premise broken: Go should sort the private-use character below the supplementary one")
	}
	if plan.Iterators[0].Name != supplementary {
		t.Fatalf("first iterator = %q, want the supplementary name; Java's UTF-16 order puts its "+
			"surrogate pair below U+E000 even though Go's byte order does not", plan.Iterators[0].Name)
	}
}

// TestCompareUTF16 pins the comparator itself, including the cases the
// stack test cannot reach through Translate.
func TestCompareUTF16(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int // sign only
	}{
		{"equal", "vers", "vers", 0},
		{"ascii less", "alpha", "beta", -1},
		{"ascii greater", "beta", "alpha", 1},
		{"prefix is less", "age", "ageoff", -1},
		{"longer is greater", "ageoff", "age", 1},
		{"supplementary sorts below private use", "\U00010000", "\uE000", -1},
		{"private use sorts above supplementary", "\uE000", "\U00010000", 1},
		{"empty is least", "", "a", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareUTF16(tt.a, tt.b)
			if sign(got) != tt.want {
				t.Fatalf("compareUTF16(%q, %q) = %d, want sign %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// TestTranslateRefusesTheFirstIteratorInExecutionOrder: the refusal an
// operator reads must name the iterator the compaction would have hit
// first, which is the lowest-priority one regardless of where the
// coordinator put it in the list.
func TestTranslateRefusesTheFirstIteratorInExecutionOrder(t *testing.T) {
	job := validJob()
	job.IteratorSettings = iterConfig(
		&tabletserver.TIteratorSetting{
			Priority: 30, Name: "late", IteratorClass: "org.apache.accumulo.core.iterators.user.AgeOffFilter",
		},
		&tabletserver.TIteratorSetting{
			Priority: 10, Name: "early", IteratorClass: "org.apache.accumulo.core.iterators.user.RegExFilter",
		},
	)

	assertRefused(t, job, Options{}, ClassUnsupportedIterator, "iteratorSettings.early")
}

// TestTranslateAcceptsEmptyIteratorStack: no majc iterators configured is
// an identity compaction, not an error.
func TestTranslateAcceptsEmptyIteratorStack(t *testing.T) {
	job := validJob()
	job.IteratorSettings = iterConfig()

	plan := mustTranslate(t, job, Options{})
	if len(plan.Stack) != 0 {
		t.Fatalf("Stack = %+v, want empty", plan.Stack)
	}
}

// TestTranslateRefusesMissingRequiredContainers: Compactor.initialize
// opens with job.getOverrides().isEmpty() and then runs
// job.getIteratorSettings().getIterators().forEach(...), all unguarded,
// so a job missing any of those three containers is one the reference
// implementation throws on before it reads a single file. Reading a
// missing container as an empty one would have shoal call such a job
// translatable. Thrift decodes a present empty map or list into a
// non-nil one, so "absent" and "present and empty" really are different
// wire shapes and only the second means "nothing configured".
func TestTranslateRefusesMissingRequiredContainers(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*tabletserver.TExternalCompactionJob)
		field  string
	}{
		{
			name:   "overrides",
			mutate: func(j *tabletserver.TExternalCompactionJob) { j.Overrides = nil },
			field:  "overrides",
		},
		{
			name:   "iteratorSettings",
			mutate: func(j *tabletserver.TExternalCompactionJob) { j.IteratorSettings = nil },
			field:  "iteratorSettings",
		},
		{
			name: "iterator list inside a present config",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.IteratorSettings = &tabletserver.IteratorConfig{}
			},
			field: "iteratorSettings.iterators",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			tt.mutate(job)

			r := assertRefused(t, job, Options{}, ClassMalformedJob, tt.field)
			if !strings.Contains(r.Detail, "Compactor.initialize") {
				t.Fatalf("detail = %q, want it to name what dereferences the container", r.Detail)
			}
		})
	}
}

// TestExtentBoundsInverted pins the predicate cmd/shoal-compactor asks
// before it tries to hand a job back, because KeyExtent.fromThrift runs
// the same test inside compactionFailed: a missing bound is infinite
// rather than inverted, and equal bounds are rejected because the
// constructor's test is ">=", not ">".
func TestExtentBoundsInverted(t *testing.T) {
	for _, tt := range []struct {
		name string
		ex   *data.TKeyExtent
		want bool
	}{
		{"nil extent", nil, false},
		{"both bounds infinite", &data.TKeyExtent{Table: []byte("2")}, false},
		{"infinite start", &data.TKeyExtent{Table: []byte("2"), EndRow: []byte("m")}, false},
		{"infinite end", &data.TKeyExtent{Table: []byte("2"), PrevEndRow: []byte("c")}, false},
		{
			"ordered",
			&data.TKeyExtent{Table: []byte("2"), PrevEndRow: []byte("c"), EndRow: []byte("m")},
			false,
		},
		{
			"equal",
			&data.TKeyExtent{Table: []byte("2"), PrevEndRow: []byte("m"), EndRow: []byte("m")},
			true,
		},
		{
			"inverted",
			&data.TKeyExtent{Table: []byte("2"), PrevEndRow: []byte("m"), EndRow: []byte("c")},
			true,
		},
		{
			// Text.compareTo compares bytes unsigned, so a high byte
			// sorts after a low one rather than before it.
			"high byte",
			&data.TKeyExtent{Table: []byte("2"), PrevEndRow: []byte("\xff"), EndRow: []byte("\x01")},
			true,
		},
		{
			// A present but empty bound is an empty Text, not a missing
			// one, and every row sorts at or after it.
			"empty end row",
			&data.TKeyExtent{Table: []byte("2"), PrevEndRow: []byte("c"), EndRow: []byte{}},
			true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtentBoundsInverted(tt.ex); got != tt.want {
				t.Fatalf("ExtentBoundsInverted = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTranslateRefusesAnIteratorWithNoOptionMap covers the third
// container Compactor.initialize dereferences, one level further in.
// SystemIteratorUtil.toIteratorSetting hands the map to IteratorSetting's
// four-argument constructor, which calls addOptions, which opens with
// checkArgument(properties != null) — so a missing map is a job the
// reference implementation throws on, not an iterator with no options.
// The setting is built without iterConfig, which fills the map in the
// way a real coordinator does.
func TestTranslateRefusesAnIteratorWithNoOptionMap(t *testing.T) {
	job := validJob()
	job.IteratorSettings = &tabletserver.IteratorConfig{
		Iterators: []*tabletserver.TIteratorSetting{{
			Priority:      20,
			Name:          "vers",
			IteratorClass: versClass,
		}},
	}

	r := assertRefused(t, job, Options{}, ClassMalformedJob, "iteratorSettings.vers.properties")
	if !strings.Contains(r.Detail, "null option map") {
		t.Fatalf("detail = %q, want it to name what rejects the map", r.Detail)
	}
}

// TestTranslateAcceptsAnIteratorWithAnEmptyOptionMap is the other half:
// a present, empty map is what a coordinator sends for an iterator
// configured with no options, and it must translate.
func TestTranslateAcceptsAnIteratorWithAnEmptyOptionMap(t *testing.T) {
	job := validJob()
	job.IteratorSettings = &tabletserver.IteratorConfig{
		Iterators: []*tabletserver.TIteratorSetting{{
			Priority:      20,
			Name:          "vers",
			IteratorClass: versClass,
			Properties:    map[string]string{},
		}},
	}

	plan := mustTranslate(t, job, Options{})
	if len(plan.Iterators) != 1 {
		t.Fatalf("Iterators = %d, want 1", len(plan.Iterators))
	}
	if got := len(plan.Iterators[0].Spec.Options); got != 0 {
		t.Fatalf("Options = %d, want none", got)
	}
}

// TestTranslateNamesTheIteratorCheckItFailed pins the detail text of the
// two setter refusals, because "malformed iterator" alone does not tell
// an operator which field to fix. Both come from IteratorSetting's
// setters, which run while Compactor.initialize builds the stack.
func TestTranslateNamesTheIteratorCheckItFailed(t *testing.T) {
	for _, tt := range []struct {
		name    string
		setting *tabletserver.TIteratorSetting
		want    string
	}{
		{
			name:    "zero priority",
			setting: &tabletserver.TIteratorSetting{Name: "vers", IteratorClass: versClass},
			want:    "IteratorSetting.setPriority",
		},
		{
			name: "negative priority",
			setting: &tabletserver.TIteratorSetting{
				Priority: -1, Name: "vers", IteratorClass: versClass,
			},
			want: "IteratorSetting.setPriority",
		},
		{
			name: "dotted name",
			setting: &tabletserver.TIteratorSetting{
				Priority: 20, Name: "vers.1", IteratorClass: versClass,
			},
			want: "IteratorSetting.setName",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.IteratorSettings = iterConfig(tt.setting)

			r := assertRefused(t, job, Options{}, ClassMalformedJob, "iteratorSettings[0]")
			if !strings.Contains(r.Detail, tt.want) {
				t.Fatalf("detail = %q, want it to name %q", r.Detail, tt.want)
			}
		})
	}
}

// TestTranslateAcceptsThePriorityJavaAccepts pins the boundary: the
// check is "> 0", so 1 is the lowest priority a job can carry.
func TestTranslateAcceptsThePriorityJavaAccepts(t *testing.T) {
	job := validJob()
	job.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
		Priority: 1, Name: "vers", IteratorClass: versClass,
	})

	plan := mustTranslate(t, job, Options{})
	if len(plan.Iterators) != 1 || plan.Iterators[0].Priority != 1 {
		t.Fatalf("Iterators = %+v, want one entry at priority 1", plan.Iterators)
	}
}

// TestTranslateOverridesResolveOutputEncoding pins the property mapping
// shoal honors, including Accumulo's memory-suffix syntax.
func TestTranslateOverridesResolveOutputEncoding(t *testing.T) {
	tests := []struct {
		name          string
		overrides     map[string]string
		opts          Options
		wantCodec     string
		wantBlockSize int
	}{
		{
			name:      "no overrides falls back to the caller's table defaults",
			overrides: map[string]string{},
			opts:      Options{DefaultCodec: block.CodecSnappy, DefaultBlockSize: 64 * 1024},
			wantCodec: block.CodecSnappy, wantBlockSize: 64 * 1024,
		},
		{
			name:      "no overrides and no defaults defers to the composer",
			overrides: map[string]string{},
			wantCodec: "", wantBlockSize: 0,
		},
		{
			name:      "codec none",
			overrides: map[string]string{propCompressType: "none"},
			wantCodec: block.CodecNone,
		},
		{
			name:      "codec gz",
			overrides: map[string]string{propCompressType: "gz"},
			wantCodec: block.CodecGzip,
		},
		{
			name:      "codec snappy overrides the table default",
			overrides: map[string]string{propCompressType: "snappy"},
			opts:      Options{DefaultCodec: block.CodecNone},
			wantCodec: block.CodecSnappy,
		},
		{
			name:          "block size in bytes",
			overrides:     map[string]string{propCompressBlockSize: "100000"},
			wantBlockSize: 100000,
		},
		{
			name:          "block size with a byte suffix",
			overrides:     map[string]string{propCompressBlockSize: "4096B"},
			wantBlockSize: 4096,
		},
		{
			name:          "block size in kibibytes",
			overrides:     map[string]string{propCompressBlockSize: "100K"},
			wantBlockSize: 100 * 1024,
		},
		{
			name:          "block size in mebibytes",
			overrides:     map[string]string{propCompressBlockSize: "2m"},
			wantBlockSize: 2 * 1024 * 1024,
		},
		{
			name: "filesystem placement overrides are ignored",
			overrides: map[string]string{
				"table.file.blocksize":   "256M",
				"table.file.replication": "3",
			},
			opts:      Options{DefaultCodec: block.CodecSnappy},
			wantCodec: block.CodecSnappy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Overrides = tt.overrides
			plan := mustTranslate(t, job, tt.opts)
			if plan.Codec != tt.wantCodec {
				t.Errorf("Codec = %q, want %q", plan.Codec, tt.wantCodec)
			}
			if plan.BlockSize != tt.wantBlockSize {
				t.Errorf("BlockSize = %d, want %d", plan.BlockSize, tt.wantBlockSize)
			}
		})
	}
}

// TestTranslateRefusesUnusableOptionDefaults closes the gap between what
// a Plan claims and what compaction.Compact accepts. The defaults are
// this compactor's own configuration, so they never appear as overrides
// and were previously copied into the plan unchecked — a job would then
// be accepted, take the slot, and fail in the writer.
func TestTranslateRefusesUnusableOptionDefaults(t *testing.T) {
	for _, tt := range []struct {
		name      string
		opts      Options
		wantField string
		wantIn    string
	}{
		{
			name:      "codec shoal cannot write",
			opts:      Options{DefaultCodec: "zstd"},
			wantField: "options.defaultCodec",
			wantIn:    "zstd",
		},
		{
			// A near-miss spelling: the compressor registers exactly
			// none, gz and snappy, and a default skips the override
			// path's mapping table entirely.
			name:      "codec spelled gzip",
			opts:      Options{DefaultCodec: "gzip"},
			wantField: "options.defaultCodec",
			wantIn:    "gzip",
		},
		{
			name:      "negative block size",
			opts:      Options{DefaultBlockSize: -1},
			wantField: "options.defaultBlockSize",
			wantIn:    "-1",
		},
		{
			name:      "block size past the cap",
			opts:      Options{DefaultBlockSize: maxBlockSize + 1},
			wantField: "options.defaultBlockSize",
			wantIn:    "outside the supported range",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := assertRefused(t, validJob(), tt.opts, ClassUnsupportedProperty, tt.wantField)
			if !strings.Contains(r.Detail, tt.wantIn) {
				t.Fatalf("detail = %q, want it to name %q", r.Detail, tt.wantIn)
			}
		})
	}
}

// TestTranslateAcceptsAnOverrideOverAnUnusableDefault: the default only
// matters when the job is silent, so an override that names a codec
// shoal can write must still run.
func TestTranslateAcceptsAnOverrideOverAnUnusableDefault(t *testing.T) {
	job := validJob()
	job.Overrides = map[string]string{propCompressType: "snappy"}

	plan := mustTranslate(t, job, Options{DefaultCodec: "zstd"})
	if plan.Codec != block.CodecSnappy {
		t.Fatalf("Codec = %q, want %q", plan.Codec, block.CodecSnappy)
	}
}

// TestTranslateRefusesUnsupportedOverrides covers the capability gate on
// table properties: anything shoal cannot reproduce in the output file
// sends the job back to a Java compactor.
func TestTranslateRefusesUnsupportedOverrides(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		wantClass string
		wantField string
	}{
		{
			name:      "codec shoal cannot write",
			overrides: map[string]string{propCompressType: "zstd"},
			wantClass: ClassUnsupportedProperty,
			wantField: "overrides[" + propCompressType + "]",
		},
		{
			// Compression.getCompressionAlgorithmByName is a map
			// lookup keyed by each algorithm's getName(); Gz's is "gz",
			// so "gzip" is as unusable to Java as it is here.
			name:      "codec spelled gzip",
			overrides: map[string]string{propCompressType: "gzip"},
			wantClass: ClassUnsupportedProperty,
			wantField: "overrides[" + propCompressType + "]",
		},
		{
			// The same lookup is case-sensitive.
			name:      "codec in the wrong case",
			overrides: map[string]string{propCompressType: "GZ"},
			wantClass: ClassUnsupportedProperty,
			wantField: "overrides[" + propCompressType + "]",
		},
		{
			// ...and does no trimming.
			name:      "codec padded with whitespace",
			overrides: map[string]string{propCompressType: " gz "},
			wantClass: ClassUnsupportedProperty,
			wantField: "overrides[" + propCompressType + "]",
		},
		{
			// getFixedMemoryAsBytes hands the value straight to
			// Long.parseLong, which rejects surrounding whitespace, so
			// this is a syntax fault rather than an unsupported size.
			name:      "block size padded with whitespace",
			overrides: map[string]string{propCompressBlockSize: " 128K"},
			wantClass: ClassMalformedJob,
			wantField: "overrides[" + propCompressBlockSize + "]",
		},
		{
			name:      "block size of zero",
			overrides: map[string]string{propCompressBlockSize: "0"},
			wantClass: ClassUnsupportedProperty,
			wantField: "overrides[" + propCompressBlockSize + "]",
		},
		{
			name:      "absurd block size",
			overrides: map[string]string{propCompressBlockSize: "8G"},
			wantClass: ClassUnsupportedProperty,
			wantField: "overrides[" + propCompressBlockSize + "]",
		},
		{
			name:      "table crypto service",
			overrides: map[string]string{"table.crypto.opts.key": "kms://k1"},
			wantClass: ClassUnsupportedCrypto,
			wantField: "overrides[table.crypto.opts.key]",
		},
		{
			name:      "instance crypto service",
			overrides: map[string]string{"instance.crypto.service": "o.a.a.core.crypto.AESCryptoService"},
			wantClass: ClassUnsupportedCrypto,
			wantField: "overrides[instance.crypto.service]",
		},
		{
			name:      "unknown property",
			overrides: map[string]string{"table.sampler": "o.a.a.core.sample.RowSampler"},
			wantClass: ClassUnsupportedProperty,
			wantField: "overrides[table.sampler]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := validJob()
			job.Overrides = tt.overrides
			assertRefused(t, job, Options{}, tt.wantClass, tt.wantField)
		})
	}
}

// TestTranslateReportsTheSameOverrideRefusalEveryTime: map iteration
// order must not leak into the class an operator sees for a job with
// several unsupported overrides.
func TestTranslateReportsTheSameOverrideRefusalEveryTime(t *testing.T) {
	job := validJob()
	job.Overrides = map[string]string{
		"table.crypto.opts.key": "kms://k1",
		"table.sampler":         "o.a.a.core.sample.RowSampler",
		propCompressType:        "zstd",
	}

	first := assertRefused(t, job, Options{}, ClassUnsupportedCrypto, "overrides[table.crypto.opts.key]")
	for i := 0; i < 50; i++ {
		_, err := Translate(job, Options{})
		got := RefusalOf(err)
		if got == nil || got.Field != first.Field || got.Class != first.Class {
			t.Fatalf("iteration %d refused with %v, want a stable %s/%s", i, err, first.Class, first.Field)
		}
	}
}

// TestTranslateEnforcesInputLimits: the composer holds whole RFile images
// in memory, so an oversized job must be refused before it can OOM the
// process and take every other compaction on it down.
func TestTranslateEnforcesInputLimits(t *testing.T) {
	t.Run("too many files", func(t *testing.T) {
		job := validJob()
		r := assertRefused(t, job, Options{Limits: Limits{MaxInputFiles: 1}}, ClassResourceLimitExceeded, "files")
		if !strings.Contains(r.Detail, "limit of 1") {
			t.Fatalf("detail = %q, want the configured limit", r.Detail)
		}
	})

	t.Run("too many bytes", func(t *testing.T) {
		job := validJob()
		r := assertRefused(t, job, Options{Limits: Limits{MaxTotalInputBytes: 3071}}, ClassResourceLimitExceeded, "files")
		if !strings.Contains(r.Detail, "3072") {
			t.Fatalf("detail = %q, want the measured total", r.Detail)
		}
	})

	t.Run("exactly at the limit is accepted", func(t *testing.T) {
		job := validJob()
		mustTranslate(t, job, Options{Limits: Limits{MaxInputFiles: 2, MaxTotalInputBytes: 3072}})
	})

	t.Run("zero means unlimited", func(t *testing.T) {
		job := validJob()
		job.Files[0].Size = 1 << 62
		mustTranslate(t, job, Options{Limits: Limits{}})
	})

	t.Run("defaults admit an ordinary job", func(t *testing.T) {
		job := validJob()
		mustTranslate(t, job, Options{Limits: DefaultLimits()})
	})
}

// TestTranslateChecksStructureBeforeCapability: a job that is both
// malformed and unsupported must report the malformed field, because
// that is the one an operator can act on.
func TestTranslateChecksStructureBeforeCapability(t *testing.T) {
	job := validJob()
	job.Files = nil
	job.Overrides = map[string]string{propCompressType: "zstd"}
	job.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
		Priority: 1, Name: "ageoff", IteratorClass: "org.apache.accumulo.core.iterators.user.AgeOffFilter",
	})

	assertRefused(t, job, Options{}, ClassMalformedJob, "files")
}

// TestTranslateChecksEveryFieldForStructureFirst pins the pass split
// across fields, not just within one. Each case is a job with a
// structural fault in one field and a capability gap in another; the
// structural one must win regardless of which field it is in, because a
// job nobody can run is more actionable than one only Java can run.
//
// Checking only within a field would let the first capability gap short
// circuit the walk and hide the real fault.
func TestTranslateChecksEveryFieldForStructureFirst(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*tabletserver.TExternalCompactionJob)
		wantClass string
		wantField string
	}{
		{
			// Structural fault sits in a later field than the gap.
			name: "unported iterator does not hide a malformed input",
			mutate: func(job *tabletserver.TExternalCompactionJob) {
				job.Files = append(job.Files, &tabletserver.InputFile{
					MetadataFileEntry: storedFile("hdfs://nn/accumulo/tables/2/t-0001/F0003.rf"),
					Size:              -1,
				})
				job.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
					Priority: 1, Name: "latent", IteratorClass: latentClass,
				})
			},
			wantClass: ClassMalformedJob,
			wantField: "files[2]",
		},
		{
			// ...and in an earlier one.
			name: "unsupported codec does not hide a malformed iterator",
			mutate: func(job *tabletserver.TExternalCompactionJob) {
				job.Overrides = map[string]string{propCompressType: "zstd"}
				job.IteratorSettings = iterConfig(nil)
			},
			wantClass: ClassMalformedJob,
			wantField: "iteratorSettings[0]",
		},
		{
			// An unparsable memory value is a syntax fault, so it is
			// pass 1 even though the property itself is supported.
			name: "fenced input does not hide an unparsable block size",
			mutate: func(job *tabletserver.TExternalCompactionJob) {
				job.Files[0].MetadataFileEntry = fencedFile(
					"hdfs://nn/accumulo/tables/2/t-0001/F0001.rf", "d", "f")
				job.Overrides = map[string]string{propCompressBlockSize: "not-a-size"}
			},
			wantClass: ClassMalformedJob,
			wantField: "overrides[" + propCompressBlockSize + "]",
		},
		{
			// Inside pass 2 the order is inputs, output (file type then
			// encoding), iterators; these cases pin each boundary.
			name: "fenced input outranks an unsupported codec",
			mutate: func(job *tabletserver.TExternalCompactionJob) {
				job.Files[0].MetadataFileEntry = fencedFile(
					"hdfs://nn/accumulo/tables/2/t-0001/F0001.rf", "d", "f")
				job.Overrides = map[string]string{propCompressType: "zstd"}
			},
			wantClass: ClassRangedInputFile,
			wantField: "files[0]",
		},
		{
			name: "unwritable output format outranks an unsupported codec",
			mutate: func(job *tabletserver.TExternalCompactionJob) {
				job.OutputFile = "hdfs://nn/accumulo/tables/2/t-0001/C0003.parquet_tmp_" + testECID
				job.Overrides = map[string]string{propCompressType: "zstd"}
			},
			wantClass: ClassUnsupportedProperty,
			wantField: "outputFile",
		},
		{
			name: "unsupported codec outranks an unported iterator",
			mutate: func(job *tabletserver.TExternalCompactionJob) {
				job.Overrides = map[string]string{propCompressType: "zstd"}
				job.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
					Priority: 1, Name: "latent", IteratorClass: latentClass,
				})
			},
			wantClass: ClassUnsupportedProperty,
			wantField: "overrides[" + propCompressType + "]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job := validJob()
			tc.mutate(job)
			assertRefused(t, job, Options{}, tc.wantClass, tc.wantField)
		})
	}
}

// TestTranslateRefusesOverflowingInputTotals: the DataFileValue sizes
// come off the wire. Summing them without a guard wraps negative, which
// would make a job of impossible size compare *under* the byte budget
// and be accepted — the limit has to survive hostile inputs, not just
// large ones.
func TestTranslateRefusesOverflowingInputTotals(t *testing.T) {
	t.Run("bytes", func(t *testing.T) {
		job := validJob()
		job.Files[0].Size = math.MaxInt64
		job.Files[1].Size = math.MaxInt64

		r := assertRefused(t, job, Options{Limits: DefaultLimits()}, ClassMalformedJob, "files[1]")
		if !strings.Contains(r.Detail, "overflow") {
			t.Fatalf("detail = %q, want it to name the overflow", r.Detail)
		}
	})

	t.Run("entries", func(t *testing.T) {
		job := validJob()
		job.Files[0].Entries = math.MaxInt64
		job.Files[1].Entries = 1

		assertRefused(t, job, Options{Limits: DefaultLimits()}, ClassMalformedJob, "files[1]")
	})

	t.Run("no budget configured still refuses", func(t *testing.T) {
		// The guard is about the reported totals being truthful, not
		// only about enforcing a limit.
		job := validJob()
		job.Files[0].Size = math.MaxInt64
		job.Files[1].Size = 1

		assertRefused(t, job, Options{Limits: Limits{}}, ClassMalformedJob, "files[1]")
	})

	t.Run("summing to exactly MaxInt64 is accepted", func(t *testing.T) {
		job := validJob()
		job.Files[0].Size = math.MaxInt64 - 1
		job.Files[1].Size = 1
		job.Files[0].Entries = math.MaxInt64
		job.Files[1].Entries = 0

		plan := mustTranslate(t, job, Options{Limits: Limits{}})
		if plan.TotalInputBytes != math.MaxInt64 {
			t.Fatalf("TotalInputBytes = %d, want %d", plan.TotalInputBytes, int64(math.MaxInt64))
		}
		if plan.TotalInputEntries != math.MaxInt64 {
			t.Fatalf("TotalInputEntries = %d, want %d", plan.TotalInputEntries, int64(math.MaxInt64))
		}
	})
}

// TestTranslateDetectsDuplicatesByDecodedPath: the same RFile can be
// spelled several ways in metadataFileEntry — different JSON field
// order, or the legacy bare path. Comparing the raw entries would let
// two spellings of one file through, and merging a file with itself
// doubles every cell it holds.
func TestTranslateDetectsDuplicatesByDecodedPath(t *testing.T) {
	const dup = "hdfs://nn/accumulo/tables/2/t-0001/F0001.rf"

	t.Run("different json field order", func(t *testing.T) {
		job := validJob()
		job.Files[1].MetadataFileEntry = fmt.Sprintf(
			`{"endRow":"","startRow":"","path":"%s"}`, dup)

		r := assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
		if !strings.Contains(r.Detail, dup) {
			t.Fatalf("detail = %q, want the shared path", r.Detail)
		}
	})

	t.Run("json and legacy bare path", func(t *testing.T) {
		job := validJob()
		job.Files[1].MetadataFileEntry = dup

		assertRefused(t, job, Options{}, ClassMalformedJob, "files[1]")
	})

	t.Run("output aliasing an input spelled differently", func(t *testing.T) {
		job := validJob()
		job.OutputFile = dup

		assertRefused(t, job, Options{}, ClassMalformedJob, "outputFile")
	})

	t.Run("distinct paths are not duplicates", func(t *testing.T) {
		job := validJob()
		job.Files[1].MetadataFileEntry = "hdfs://nn/accumulo/tables/2/t-0001/F0002.rf"
		mustTranslate(t, job, Options{})
	})
}

// TestPlanIsExecutable closes the loop: the translated plan, fed real
// input images, drives internal/compaction to the output the job asked
// for — versioning applied, tombstone dropped because propagateDeletes
// was false, and the requested codec on the output blocks.
func TestPlanIsExecutable(t *testing.T) {
	fileA := buildRFile(t, []testCell{
		{key: mkKey("row1", "cf", "cq", 10), value: "old"},
		{key: mkKey("row2", "cf", "cq", 10), value: "keep"},
	})
	fileB := buildRFile(t, []testCell{
		{key: mkKey("row1", "cf", "cq", 20), value: "new"},
		{key: deleteKey("row3", "cf", "cq", 30), value: ""},
	})

	job := validJob()
	job.PropagateDeletes = false
	job.Overrides = map[string]string{propCompressType: "gz"}
	job.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
		Priority:      20,
		Name:          "vers",
		IteratorClass: versClass,
		Properties:    map[string]string{"maxVersions": "1"},
	})

	plan := mustTranslate(t, job, Options{})
	spec := plan.Spec([]compaction.Input{
		{Name: plan.Inputs[0].Path, Bytes: fileA},
		{Name: plan.Inputs[1].Path, Bytes: fileB},
	})
	if spec.Codec != block.CodecGzip || !spec.FullMajorCompaction || spec.Scope != iterrt.ScopeMajc {
		t.Fatalf("spec = %+v, want gz / full-major / majc", spec)
	}

	result, err := compaction.Compact(spec)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	got := drainRFile(t, result.Output)
	want := []string{"row1/cf:cq@20=new", "row2/cf:cq@10=keep"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("output cells = %v, want %v", got, want)
	}
	if result.EntriesWritten != 2 {
		t.Fatalf("EntriesWritten = %d, want 2", result.EntriesWritten)
	}
}

// TestPlanCarriesTheOutputBudgetIntoTheComposer is the end of the same
// loop for the resource gate. The input limits are an admission check
// Translate can make on its own, because the coordinator declares each
// file's size. Nothing declares the *output* size — an output can be
// larger than its inputs — so the only place that budget can be enforced
// is inside the composition, and the plan has to carry it there.
func TestPlanCarriesTheOutputBudgetIntoTheComposer(t *testing.T) {
	fileA := buildRFile(t, []testCell{
		{key: mkKey("row1", "cf", "cq", 10), value: "a"},
		{key: mkKey("row2", "cf", "cq", 10), value: "b"},
	})

	job := validJob()
	job.Files = job.Files[:1]
	plan := mustTranslate(t, job, Options{Limits: Limits{MaxOutputBytes: 16}})
	if plan.MaxOutputBytes != 16 {
		t.Fatalf("plan.MaxOutputBytes = %d, want 16", plan.MaxOutputBytes)
	}

	spec := plan.Spec([]compaction.Input{{Name: plan.Inputs[0].Path, Bytes: fileA}})
	if spec.MaxOutputBytes != 16 {
		t.Fatalf("spec.MaxOutputBytes = %d, want 16", spec.MaxOutputBytes)
	}
	if _, err := compaction.Compact(spec); !errors.Is(err, compaction.ErrOutputTooLarge) {
		t.Fatalf("Compact err = %v, want ErrOutputTooLarge", err)
	}
}

// TestDefaultLimitsBudgetBothSides pins the defaults an operator gets
// when they set no flags: a job is admitted on its declared inputs and
// abandoned if its output runs away.
func TestDefaultLimitsBudgetBothSides(t *testing.T) {
	limits := DefaultLimits()
	if limits.MaxInputFiles != DefaultMaxInputFiles {
		t.Errorf("MaxInputFiles = %d, want %d", limits.MaxInputFiles, DefaultMaxInputFiles)
	}
	if limits.MaxTotalInputBytes != DefaultMaxTotalInputBytes {
		t.Errorf("MaxTotalInputBytes = %d, want %d", limits.MaxTotalInputBytes, DefaultMaxTotalInputBytes)
	}
	if limits.MaxOutputBytes != DefaultMaxOutputBytes {
		t.Errorf("MaxOutputBytes = %d, want %d", limits.MaxOutputBytes, DefaultMaxOutputBytes)
	}

	job := validJob()
	plan := mustTranslate(t, job, Options{Limits: limits})
	if plan.MaxOutputBytes != DefaultMaxOutputBytes {
		t.Fatalf("plan.MaxOutputBytes = %d, want the configured default %d",
			plan.MaxOutputBytes, DefaultMaxOutputBytes)
	}

	// Zero has to keep meaning "unlimited" all the way through, since
	// that is how a caller opts out.
	unlimited := mustTranslate(t, validJob(), Options{})
	if got := unlimited.Spec(nil).MaxOutputBytes; got != 0 {
		t.Fatalf("unset budget reached the spec as %d, want 0 (unlimited)", got)
	}
}

// TestPlanLogValueCarriesTheJobIdentity: the refusal/acceptance log line
// is the only operator-visible record of what shoal decided, so the
// fields an operator correlates on must be present.
func TestPlanLogValueCarriesTheJobIdentity(t *testing.T) {
	job := validJob()
	job.Kind = tabletserver.TCompactionKind_USER
	job.FateId = &manager.TFateId{
		Type:      manager.TFateInstanceType_USER,
		TxUUIDStr: "b7f1c0de-0000-4000-8000-00000000abcd",
	}
	job.IteratorSettings = iterConfig(&tabletserver.TIteratorSetting{
		Priority: 20, Name: "vers", IteratorClass: versClass,
	})

	plan := mustTranslate(t, job, Options{})
	rendered := plan.LogValue().String()
	want := []string{
		testECID,
		"table=2",
		"hdfs://nn/accumulo/tables/2/t-0001/C0003.rf",
		"USER",
		"b7f1c0de-0000-4000-8000-00000000abcd",
		"vers=" + iterrt.IterVersioning,
	}
	for _, want := range want {
		if !strings.Contains(rendered, want) {
			t.Fatalf("log value %q missing %q", rendered, want)
		}
	}
}

func TestExtentStringHandlesPartialExtents(t *testing.T) {
	if got := ExtentString(nil); got != "<no-extent>" {
		t.Fatalf("nil extent = %q", got)
	}
	got := ExtentString(&data.TKeyExtent{Table: []byte("2")})
	if !strings.Contains(got, "prev=-inf") || !strings.Contains(got, "end=+inf") {
		t.Fatalf("open extent = %q, want infinite bounds", got)
	}
}

func TestRefusalOfIgnoresOtherErrors(t *testing.T) {
	if got := RefusalOf(errors.New("boom")); got != nil {
		t.Fatalf("RefusalOf(non-refusal) = %+v, want nil", got)
	}
	if got := RefusalOf(nil); got != nil {
		t.Fatalf("RefusalOf(nil) = %+v, want nil", got)
	}
	wrapped := fmt.Errorf("context: %w", refuse(ClassMalformedJob, "files", "boom"))
	if got := RefusalOf(wrapped); got == nil || got.Class != ClassMalformedJob {
		t.Fatalf("RefusalOf(wrapped) = %+v, want the wrapped refusal", got)
	}
}

func TestParseMemoryBytes(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "1024", want: 1024},
		{in: "512b", want: 512},
		{in: "1K", want: 1024},
		{in: "1k", want: 1024},
		{in: "1M", want: 1024 * 1024},
		{in: "1G", want: 1024 * 1024 * 1024},
		{in: "-1", want: -1},
		{in: "", wantErr: true},
		{in: "K", wantErr: true},
		{in: "1.5M", wantErr: true},
		{in: "9223372036854775807G", wantErr: true},
		// getFixedMemoryAsBytes reads the raw string: the last
		// character decides the multiplier and Long.parseLong takes the
		// rest, and neither tolerates surrounding whitespace.
		{in: " 2048", wantErr: true},
		{in: "2048 ", wantErr: true},
		{in: " 2048 ", wantErr: true},
		{in: "128 K", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseMemoryBytes(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseMemoryBytes(%q) = %d, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMemoryBytes(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("parseMemoryBytes(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// --- RFile helpers -------------------------------------------------

type testCell struct {
	key   *wire.Key
	value string
}

func mkKey(row, cf, cq string, ts int64) *wire.Key {
	return &wire.Key{
		Row:             []byte(row),
		ColumnFamily:    []byte(cf),
		ColumnQualifier: []byte(cq),
		Timestamp:       ts,
	}
}

func deleteKey(row, cf, cq string, ts int64) *wire.Key {
	k := mkKey(row, cf, cq, ts)
	k.Deleted = true
	return k
}

func buildRFile(t *testing.T, cells []testCell) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := rfile.NewWriter(&buf, rfile.WriterOptions{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, c := range cells {
		if err := w.Append(c.key, []byte(c.value)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// drainRFile renders every cell of an RFile image as
// "row/cf:cq@ts=value" so assertions read like the shell's scan output.
func drainRFile(t *testing.T, image []byte) []string {
	t.Helper()
	bc, err := bcfile.NewReader(bytes.NewReader(image), int64(len(image)))
	if err != nil {
		t.Fatalf("bcfile.NewReader: %v", err)
	}
	r, err := rfile.Open(bc, block.Default())
	if err != nil {
		t.Fatalf("rfile.Open: %v", err)
	}
	defer r.Close()

	var out []string
	for {
		k, v, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, fmt.Sprintf("%s/%s:%s@%d=%s",
			k.Row, k.ColumnFamily, k.ColumnQualifier, k.Timestamp, v))
	}
}
