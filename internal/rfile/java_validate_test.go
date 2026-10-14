// Java validation: write an RFile with our writer, then have Java's
// upstream PrintInfo / RFileScanner read it. Catches wire-format drift
// our self-roundtrip can't (we'd be drifting symmetrically and miss it).
//
// The test is gated — it only runs when SHOAL_JAVA_RFILE_VALIDATE is set
// to a command that takes one positional arg (the rfile path) and exits
// non-zero on failure. Suggested commands:
//
//	# Maven exec — works from inside the Accumulo repo:
//	export SHOAL_JAVA_RFILE_VALIDATE='cd $ACCUMULO_SRC && \
//	    mvn -q -pl core exec:java \
//	    -Dexec.mainClass=org.apache.accumulo.core.file.rfile.PrintInfo \
//	    -Dexec.args=$RFILE'
//
//	# Or the accumulo shell wrapper, if installed:
//	export SHOAL_JAVA_RFILE_VALIDATE='accumulo rfile-info $RFILE'
//
// The test substitutes $RFILE with the test fixture's local path before
// running the command via "sh -c".
package rfile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/blockmeta"
)

// writeFixtureRFile produces a small RFile at path with N rows. Returns
// the path on success.
func writeFixtureRFile(t *testing.T, path string, rows int, codec string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	w, err := NewWriter(f, WriterOptions{Codec: codec, BlockSize: 4096})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i := 0; i < rows; i++ {
		k := &Key{
			Row:             []byte(fmt.Sprintf("row%05d", i)),
			ColumnFamily:    []byte("cf"),
			ColumnQualifier: []byte("cq"),
			Timestamp:       int64(i + 1),
		}
		v := []byte(fmt.Sprintf("value-%d", i))
		if err := w.Append(k, v); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Writer.Close: %v", err)
	}
}

// runJavaValidator executes the user-supplied SHOAL_JAVA_RFILE_VALIDATE
// command with $RFILE substituted to rfilePath. Returns combined output
// + error. Times out after 5 minutes so a hung Maven download doesn't
// hang CI.
func runJavaValidator(t *testing.T, rfilePath string) ([]byte, error) {
	t.Helper()
	cmdTemplate := os.Getenv("SHOAL_JAVA_RFILE_VALIDATE")
	if cmdTemplate == "" {
		t.Skip("SHOAL_JAVA_RFILE_VALIDATE not set; skipping Java validation")
	}
	cmdStr := strings.ReplaceAll(cmdTemplate, "$RFILE", rfilePath)
	t.Logf("running validator: %s", cmdStr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

func TestJavaValidate_GzipRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shoal-java-validate.rf")
	writeFixtureRFile(t, path, 100, block.CodecGzip)

	out, err := runJavaValidator(t, path)
	if err != nil {
		t.Fatalf("Java validator failed: %v\nOutput:\n%s", err, out)
	}
	t.Logf("Java validator output (truncated to 2KB):\n%s", truncate(out, 2048))

	// Sanity: the output should mention something key-like — Java's
	// PrintInfo prints "Locality group" / "Num blocks" / "Num entries".
	for _, sentinel := range []string{"Locality group", "Num"} {
		if !bytes.Contains(out, []byte(sentinel)) {
			t.Errorf("validator output missing %q sentinel — Java may have read but produced unexpected output", sentinel)
		}
	}
}

func TestJavaValidate_NoneCodec(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shoal-none.rf")
	writeFixtureRFile(t, path, 50, block.CodecNone)

	out, err := runJavaValidator(t, path)
	if err != nil {
		t.Fatalf("Java validator failed (none codec): %v\nOutput:\n%s", err, out)
	}
	t.Logf("Java validator output (none codec, first 2KB):\n%s", truncate(out, 2048))
}

// TestJavaValidate_SnappyRoundtrip writes a snappy-compressed RFile via
// shoal and hands it to Java's PrintInfo. Confirms our Hadoop block-
// framed snappy encoder is bit-compatible with Hadoop's decoder.
func TestJavaValidate_SnappyRoundtrip(t *testing.T) {
	testJavaCodecRoundtrip(t, block.CodecSnappy)
}

func TestJavaValidate_AdditionalHadoopCodecs(t *testing.T) {
	for _, codec := range []string{block.CodecLZ4, block.CodecZstd} {
		t.Run(codec, func(t *testing.T) {
			testJavaCodecRoundtrip(t, codec)
		})
	}
}

func testJavaCodecRoundtrip(t *testing.T, codec string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "shoal-"+codec+".rf")
	writeFixtureRFile(t, path, 100, codec)

	out, err := runJavaValidator(t, path)
	if err != nil {
		t.Fatalf("Java validator failed (%s): %v\nOutput:\n%s", codec, err, out)
	}
	t.Logf("Java %s validator output (first 2KB):\n%s", codec, truncate(out, 2048))
	for _, sentinel := range []string{"Locality group", "Num"} {
		if !bytes.Contains(out, []byte(sentinel)) {
			t.Errorf("validator output missing %q sentinel", sentinel)
		}
	}
}

// TestSelfRoundtripBeforeJava runs unconditionally — it's a fast sanity
// pass that the same fixture we'd ask Java to validate also reads back
// cleanly through our own reader. Catches obvious writer regressions
// before paying the Maven cost.
func TestSelfRoundtripBeforeJava(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "self-roundtrip.rf")
	writeFixtureRFile(t, path, 200, block.CodecGzip)

	bs, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bc, err := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	count := 0
	for {
		_, _, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next at %d: %v", count, err)
		}
		count++
	}
	if count != 200 {
		t.Errorf("got %d cells, want 200", count)
	}
}

// writeFixtureRFileMultiLevel produces an RFile that's guaranteed to
// have a multi-level index (root.Level > 0). Drives BlockSize and
// IndexBlockSize tiny so the cascade fires.
func writeFixtureRFileMultiLevel(t *testing.T, path string, rows int, codec string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	w, err := NewWriter(f, WriterOptions{
		Codec:          codec,
		BlockSize:      120, // tiny → many leaves
		IndexBlockSize: 200, // tiny → forces level cascade
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i := 0; i < rows; i++ {
		k := &Key{
			Row:             []byte(fmt.Sprintf("row%06d", i)),
			ColumnFamily:    []byte("cf"),
			ColumnQualifier: []byte("cq"),
			Timestamp:       int64(i + 1),
		}
		v := []byte(fmt.Sprintf("value-%d-with-padding-%s", i, strings.Repeat("p", 16)))
		if err := w.Append(k, v); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Writer.Close: %v", err)
	}
}

// TestJavaValidate_MultiLevelGzip writes a multi-level-index RFile with
// gzip, hands it to Java, and confirms parse success. Java's PrintInfo
// prints per-level statistics (sizesByLevel) for multi-level files —
// any wire mismatch in the level-block layout would surface there.
func TestJavaValidate_MultiLevelGzip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shoal-java-multilevel.rf")
	writeFixtureRFileMultiLevel(t, path, 800, block.CodecGzip)

	// Pre-flight: confirm our writer actually produced multi-level. Avoids
	// shipping a degenerate single-level case to Java by accident.
	bs, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bc, err := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	rootLevel := r.LocalityGroup().RootIndex.Level
	r.Close()
	if rootLevel == 0 {
		t.Fatalf("fixture is single-level (root.Level=0); test would silently pass on the wrong path")
	}
	t.Logf("fixture root.Level = %d", rootLevel)

	out, err := runJavaValidator(t, path)
	if err != nil {
		t.Fatalf("Java validator failed (multi-level gzip): %v\nOutput:\n%s", err, out)
	}
	t.Logf("Java multi-level validator output (first 2KB):\n%s", truncate(out, 2048))

	for _, sentinel := range []string{"Locality group", "Num"} {
		if !bytes.Contains(out, []byte(sentinel)) {
			t.Errorf("validator output missing %q sentinel", sentinel)
		}
	}
}

// TestJavaValidate_BlockMetaIgnored writes a shoal-augmented RFile
// (with a RFile.blockmeta meta-block) and confirms Java's PrintInfo
// reads it cleanly — the unknown meta-block is ignored by name. This
// proves blockmeta is non-breaking on the Java side: stock readers
// don't choke on it, they just don't benefit from it.
func TestJavaValidate_BlockMetaIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shoal-blockmeta.rf")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zm := blockmeta.NewZoneMapBuilder()
	w, err := NewWriter(f, WriterOptions{
		Codec:             block.CodecGzip,
		BlockSize:         512,
		BlockMetaBuilders: []blockmeta.OverlayBuilder{zm},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		k := &Key{
			Row:             []byte(fmt.Sprintf("row%05d", i)),
			ColumnFamily:    []byte("cf"),
			ColumnQualifier: []byte("cq"),
			Timestamp:       int64(i + 1),
		}
		_ = w.Append(k, []byte(fmt.Sprintf("v-%d", i)))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := runJavaValidator(t, path)
	if err != nil {
		t.Fatalf("Java rejected blockmeta-augmented RFile: %v\nOutput:\n%s", err, out)
	}
	t.Logf("Java validator output (blockmeta-augmented, first 1KB):\n%s", truncate(out, 1024))

	// Sanity: Java's PrintInfo enumerates meta blocks. Our RFile.blockmeta
	// should appear in the listing — Java sees it as an unknown meta
	// block by name but doesn't try to interpret the contents.
	if !bytes.Contains(out, []byte("RFile.blockmeta")) {
		t.Errorf("PrintInfo output didn't list RFile.blockmeta — meta block may have been dropped")
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "\n... (truncated)"
}
