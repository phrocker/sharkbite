// Parity-harness tests. Two tiers:
//
//   - Pure-Go (always run): shoal-write → shoal-read roundtrip, writer
//     determinism, metadata self-consistency, cell-stream codec
//     roundtrip. No JVM.
//
//   - Java-gated (run only when SHOAL_JAVA_PARITY is set): write the
//     SAME generated cell stream through both shoal and the Java
//     RFile.Writer (via ShoalParityWrite), then assert semantic
//     equivalence — identical all-row scan + identical point-lookup
//     results — plus metadata-readable-by-Java via PrintInfo.
//
// SHOAL_JAVA_PARITY is a command template with two placeholders:
//
//	$CELLSTREAM  — path to the shoal-emitted cell-stream input
//	$RFILE       — path the Java side must write its RFile to
//
// The harness also substitutes $CODEC, $BLOCKSIZE, $INDEXBLOCKSIZE. The
// command must exit non-zero on failure. Suggested value (run from
// inside the Accumulo repo):
//
//	export SHOAL_JAVA_PARITY='cd /mnt/ExtraDrive/repos/accumulo && \
//	  mvn -q -pl core exec:java -Dexec.classpathScope=test \
//	  -Dexec.mainClass=org.apache.accumulo.core.file.rfile.ShoalParityWrite \
//	  -Dexec.args="$CELLSTREAM $RFILE $CODEC $BLOCKSIZE $INDEXBLOCKSIZE"'
//
// For assertion (c) — metadata read by the Java RFile reader — the
// existing SHOAL_JAVA_RFILE_VALIDATE PrintInfo hook is reused if set.
package rfile

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
)

// defaultParityConfig is the standard C0 identity-compaction scenario.
func defaultParityConfig(codec string) ParityConfig {
	return ParityConfig{
		Seed:           0xC0FFEE,
		Cells:          5000,
		Codec:          codec,
		BlockSize:      4096,
		IndexBlockSize: 256, // small → exercises the multi-level index
		Lookups:        10000,
	}
}

// --- Pure-Go tier (unconditional) ----------------------------------------

// TestParity_CellStreamCodecRoundtrip locks the shared cell-stream wire
// format. ShoalParityWrite.java decodes exactly these bytes — if this
// drifts, the Java side silently reads garbage, so pin it.
func TestParity_CellStreamCodecRoundtrip(t *testing.T) {
	cfg := defaultParityConfig(block.CodecNone)
	cfg.Cells = 200
	cells := genCells(cfg)

	var buf bytes.Buffer
	if err := writeCellStream(&buf, cells); err != nil {
		t.Fatalf("writeCellStream: %v", err)
	}
	// Re-decode with the same big-endian layout ShoalParityWrite uses and
	// confirm every field survives.
	got := decodeCellStream(t, buf.Bytes())
	if d := cellSeqDiff(cells, got); d != "" {
		t.Fatalf("cell-stream codec roundtrip diverged: %s", d)
	}
}

// TestParity_ShoalRoundtrip is the core pure-Go assertion: a shoal-
// written RFile reads back as the identical key+value sequence. This is
// assertion (d) restricted to the shoal side — it can't catch symmetric
// drift (that needs the Java tier) but it catches every asymmetric
// writer/reader regression for free on `go test`.
func TestParity_ShoalRoundtrip(t *testing.T) {
	for _, codec := range []string{block.CodecNone, block.CodecGzip, block.CodecSnappy} {
		t.Run(codec, func(t *testing.T) {
			cfg := defaultParityConfig(codec)
			cells := genCells(cfg)
			dir := t.TempDir()
			path := filepath.Join(dir, "shoal.rf")
			if err := shoalWriteRFile(path, cfg, cells); err != nil {
				t.Fatalf("shoalWriteRFile: %v", err)
			}
			r, err := openRFile(path)
			if err != nil {
				t.Fatalf("openRFile: %v", err)
			}
			defer r.Close()
			got, err := scanAll(r)
			if err != nil {
				t.Fatalf("scanAll: %v", err)
			}
			if d := cellSeqDiff(cells, got); d != "" {
				t.Fatalf("shoal write→read diverged: %s", d)
			}
		})
	}
}

// TestParity_WriterDeterminism: writing the same input twice must
// produce byte-identical RFiles. Without this, the Java-tier byte-
// identity assertion (a) would be untrustworthy — divergence could be
// shoal-internal nondeterminism rather than a real Java mismatch.
func TestParity_WriterDeterminism(t *testing.T) {
	cfg := defaultParityConfig(block.CodecGzip)
	cells := genCells(cfg)
	dir := t.TempDir()

	p1 := filepath.Join(dir, "a.rf")
	p2 := filepath.Join(dir, "b.rf")
	if err := shoalWriteRFile(p1, cfg, cells); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := shoalWriteRFile(p2, cfg, cells); err != nil {
		t.Fatalf("write b: %v", err)
	}
	b1, _ := os.ReadFile(p1)
	b2, _ := os.ReadFile(p2)
	if !bytes.Equal(b1, b2) {
		t.Fatalf("shoal writer is non-deterministic: %d vs %d bytes, differ", len(b1), len(b2))
	}

	// Belt-and-suspenders: scan fingerprints match too.
	r1, _ := openRFile(p1)
	defer r1.Close()
	r2, _ := openRFile(p2)
	defer r2.Close()
	c1, err := scanAll(r1)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := scanAll(r2)
	if err != nil {
		t.Fatal(err)
	}
	if scanFingerprint(c1) != scanFingerprint(c2) {
		t.Fatal("scan fingerprints differ despite identical bytes — impossible; reader bug")
	}
}

// TestParity_MetadataSelfConsistency exercises assertion (c) on the
// shoal side only: the metadata the writer emits is the metadata the
// reader sees. The Java-tier test extends this by cross-checking
// against PrintInfo.
func TestParity_MetadataSelfConsistency(t *testing.T) {
	cfg := defaultParityConfig(block.CodecGzip)
	cells := genCells(cfg)
	dir := t.TempDir()
	path := filepath.Join(dir, "shoal.rf")
	if err := shoalWriteRFile(path, cfg, cells); err != nil {
		t.Fatalf("shoalWriteRFile: %v", err)
	}
	r, err := openRFile(path)
	if err != nil {
		t.Fatalf("openRFile: %v", err)
	}
	defer r.Close()

	ms := readMetadataSummary(r)
	if ms.NumLocalityGroups != 1 {
		t.Errorf("NumLocalityGroups = %d, want 1 (single default LG)", ms.NumLocalityGroups)
	}
	if ms.DefaultLGEntries <= 0 {
		t.Errorf("DefaultLGEntries = %d, want > 0", ms.DefaultLGEntries)
	}
	if ms.RootIndexLevel == 0 {
		t.Errorf("RootIndexLevel = 0; IndexBlockSize=%d should have forced multi-level",
			cfg.IndexBlockSize)
	}
	if ms.FirstRow != "row00000000" {
		t.Errorf("FirstRow = %q, want row00000000", ms.FirstRow)
	}
	if ms.HasBlockMeta {
		t.Errorf("HasBlockMeta = true; identity-compaction harness writes no blockmeta")
	}
}

// TestParity_PointLookupsAgainstScan verifies the point-lookup
// primitive itself: every sampled probe's lookup result must equal what
// a linear scan would yield for the same probe. This validates the
// assertion-(b) machinery on the shoal side before the Java tier trusts
// it to compare two files.
func TestParity_PointLookupsAgainstScan(t *testing.T) {
	cfg := defaultParityConfig(block.CodecNone)
	cfg.Cells = 2000
	cfg.Lookups = 1500
	cells := genCells(cfg)
	dir := t.TempDir()
	path := filepath.Join(dir, "shoal.rf")
	if err := shoalWriteRFile(path, cfg, cells); err != nil {
		t.Fatalf("shoalWriteRFile: %v", err)
	}
	r, err := openRFile(path)
	if err != nil {
		t.Fatalf("openRFile: %v", err)
	}
	defer r.Close()

	probes := sampleProbeKeys(cells, cfg.Lookups, cfg.Seed^0x5A5A)
	for i, p := range probes {
		got, err := pointLookup(r, p)
		if err != nil {
			t.Fatalf("pointLookup %d: %v", i, err)
		}
		want := linearLookup(cells, p)
		if d := got.diff(want); d != "" {
			t.Fatalf("probe %d (%+v): lookup != linear-scan reference: %s", i, p, d)
		}
	}
}

// --- Java-gated tier -----------------------------------------------------

// TestParity_ShoalVsJava is the full harness: same cell stream → shoal
// RFile + Java RFile → assert semantic equivalence. Gated on
// SHOAL_JAVA_PARITY; skips cleanly without a JVM.
func TestParity_ShoalVsJava(t *testing.T) {
	tmpl := os.Getenv("SHOAL_JAVA_PARITY")
	if tmpl == "" {
		t.Skip("SHOAL_JAVA_PARITY not set; skipping shoal-vs-Java parity")
	}

	for _, codec := range []string{block.CodecNone, block.CodecGzip} {
		t.Run(codec, func(t *testing.T) {
			cfg := defaultParityConfig(codec)
			cells := genCells(cfg)
			dir := t.TempDir()

			// Shared input: one cell stream, both writers consume it.
			csPath := filepath.Join(dir, "cells.bin")
			cs, err := os.Create(csPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeCellStream(cs, cells); err != nil {
				cs.Close()
				t.Fatalf("writeCellStream: %v", err)
			}
			cs.Close()

			// shoal_rfile.
			shoalPath := filepath.Join(dir, "shoal.rf")
			if err := shoalWriteRFile(shoalPath, cfg, cells); err != nil {
				t.Fatalf("shoalWriteRFile: %v", err)
			}

			// java_rfile — via the configured command template.
			javaPath := filepath.Join(dir, "java.rf")
			runJavaParityWriter(t, tmpl, cfg, csPath, javaPath)

			// Assertion (a): byte-identity is the strong form; the design
			// doc explicitly permits block-boundary divergence, so a byte
			// mismatch is logged, not failed. (b)+(d) are the binding
			// semantic assertions.
			sb, _ := os.ReadFile(shoalPath)
			jb, _ := os.ReadFile(javaPath)
			if bytes.Equal(sb, jb) {
				t.Logf("(a) byte-identical RFiles (%d bytes)", len(sb))
			} else {
				t.Logf("(a) RFiles differ at byte level (shoal=%d java=%d) — "+
					"permitted; relying on semantic assertions (b)+(d)", len(sb), len(jb))
			}

			shoalR, err := openRFile(shoalPath)
			if err != nil {
				t.Fatalf("open shoal rfile: %v", err)
			}
			defer shoalR.Close()
			javaR, err := openRFile(javaPath)
			if err != nil {
				t.Fatalf("open java rfile: %v", err)
			}
			defer javaR.Close()

			// (b) + (d): point lookups + all-row scan.
			probes := sampleProbeKeys(cells, cfg.Lookups, cfg.Seed^0x5A5A)
			diff, err := compareReaders(shoalR, javaR, probes)
			if err != nil {
				t.Fatalf("compareReaders: %v", err)
			}
			if diff != "" {
				t.Fatalf("shoal vs java parity FAILED: %s", diff)
			}
			t.Logf("(b)+(d) parity OK: all-row scan + %d point lookups identical", len(probes))

			// (c): metadata as the shoal reader sees it for both files
			// must agree on shape (LG count, first row). Entry counts and
			// block levels may differ — different block boundaries.
			sms := readMetadataSummary(shoalR)
			jms := readMetadataSummary(javaR)
			if sms.NumLocalityGroups != jms.NumLocalityGroups {
				t.Errorf("(c) LG count mismatch: shoal=%d java=%d",
					sms.NumLocalityGroups, jms.NumLocalityGroups)
			}
			if sms.FirstRow != jms.FirstRow {
				t.Errorf("(c) first-row mismatch: shoal=%q java=%q", sms.FirstRow, jms.FirstRow)
			}
			t.Logf("(c) metadata shape agrees: %d LG(s), firstRow=%q", sms.NumLocalityGroups,
				sms.FirstRow)
		})
	}
}

// TestParity_JavaReadsShoalMetadata is assertion (c) in its strict form:
// hand the shoal RFile to the actual Java RFile reader (PrintInfo) and
// confirm it parses the summaries / locality-group / block-index
// metadata without error. Reuses the existing SHOAL_JAVA_RFILE_VALIDATE
// hook.
func TestParity_JavaReadsShoalMetadata(t *testing.T) {
	if os.Getenv("SHOAL_JAVA_RFILE_VALIDATE") == "" {
		t.Skip("SHOAL_JAVA_RFILE_VALIDATE not set; skipping Java metadata read")
	}
	cfg := defaultParityConfig(block.CodecGzip)
	cells := genCells(cfg)
	dir := t.TempDir()
	path := filepath.Join(dir, "shoal-meta.rf")
	if err := shoalWriteRFile(path, cfg, cells); err != nil {
		t.Fatalf("shoalWriteRFile: %v", err)
	}
	out, err := runJavaValidator(t, path)
	if err != nil {
		t.Fatalf("Java reader rejected shoal RFile: %v\nOutput:\n%s", err, out)
	}
	for _, sentinel := range []string{"Locality group", "Num"} {
		if !bytes.Contains(out, []byte(sentinel)) {
			t.Errorf("PrintInfo output missing %q — metadata may not have parsed", sentinel)
		}
	}
	t.Logf("Java PrintInfo read shoal metadata OK:\n%s", truncate(out, 1024))
}

// runJavaParityWriter invokes the SHOAL_JAVA_PARITY command template
// with all placeholders substituted, producing javaPath from csPath.
func runJavaParityWriter(t *testing.T, tmpl string, cfg ParityConfig, csPath, javaPath string) {
	t.Helper()
	rep := strings.NewReplacer(
		"$CELLSTREAM", csPath,
		"$RFILE", javaPath,
		"$CODEC", cfg.Codec,
		"$BLOCKSIZE", strconv.Itoa(cfg.BlockSize),
		"$INDEXBLOCKSIZE", strconv.Itoa(cfg.IndexBlockSize),
	)
	cmdStr := rep.Replace(tmpl)
	t.Logf("running Java parity writer: %s", cmdStr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("Java parity writer failed: %v\nOutput:\n%s", err, truncate(out.Bytes(), 4096))
	}
	if _, err := os.Stat(javaPath); err != nil {
		t.Fatalf("Java parity writer exited 0 but produced no RFile at %s: %v", javaPath, err)
	}
}

// --- test-only helpers ---------------------------------------------------

// decodeCellStream parses the shared cell-stream binary back into cells.
// Mirror of ShoalParityWrite.java's reader — kept here so the codec
// roundtrip test doesn't need the JVM.
func decodeCellStream(t *testing.T, b []byte) []cell {
	t.Helper()
	r := bytes.NewReader(b)
	var hdr [16]byte
	if _, err := r.Read(hdr[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	if be32(hdr[0:4]) != cellStreamMagic {
		t.Fatalf("bad magic 0x%08x", be32(hdr[0:4]))
	}
	if be32(hdr[4:8]) != cellStreamVersion {
		t.Fatalf("bad version %d", be32(hdr[4:8]))
	}
	n := be64(hdr[8:16])
	field := func() []byte {
		var l [4]byte
		if _, err := r.Read(l[:]); err != nil {
			t.Fatalf("read field len: %v", err)
		}
		buf := make([]byte, be32(l[:]))
		if _, err := r.Read(buf); err != nil {
			t.Fatalf("read field body: %v", err)
		}
		return buf
	}
	out := make([]cell, 0, n)
	for i := uint64(0); i < n; i++ {
		row := field()
		cf := field()
		cq := field()
		cv := field()
		var ts [8]byte
		if _, err := r.Read(ts[:]); err != nil {
			t.Fatalf("read ts: %v", err)
		}
		var del [1]byte
		if _, err := r.Read(del[:]); err != nil {
			t.Fatalf("read deleted: %v", err)
		}
		val := field()
		out = append(out, cell{
			K: &Key{
				Row:              row,
				ColumnFamily:     cf,
				ColumnQualifier:  cq,
				ColumnVisibility: nilIfEmpty(cv),
				Timestamp:        int64(be64(ts[:])),
				Deleted:          del[0] != 0,
			},
			V: val,
		})
	}
	return out
}

// nilIfEmpty normalizes a zero-length visibility to nil — genCells emits
// nil for the no-visibility case, and Key.Equal treats nil==empty, but
// keeping the codec roundtrip exact avoids a confusing diff.
func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func be64(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// linearLookup is the brute-force reference for pointLookup: the first
// cell in (already-sorted) cells whose Key >= target.
func linearLookup(cells []cell, target *Key) lookupResult {
	for _, c := range cells {
		if c.K.Compare(target) >= 0 {
			return lookupResult{found: true, key: c.K.Clone(), val: append([]byte(nil), c.V...)}
		}
	}
	return lookupResult{found: false}
}
