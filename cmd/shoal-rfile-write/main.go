// shoal-rfile-write produces a small synthetic RFile so we can validate
// that upstream Java tools (RFileScanner, PrintInfo) accept it.
//
// Two ways to use:
//
//  1. Manual validation:
//     go run ./cmd/shoal-rfile-write -out /tmp/shoal-test.rf
//     cd $ACCUMULO_SRC
//     mvn -pl core -am compile
//     mvn -pl core exec:java \
//     -Dexec.mainClass=org.apache.accumulo.core.file.rfile.PrintInfo \
//     -Dexec.args=/tmp/shoal-test.rf
//
//  2. Test-suite validation: see internal/rfile/java_validate_test.go.
//     The test invokes this binary and a configured PrintInfo command,
//     gated on env vars.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/local"
)

func main() {
	if err := run(); err != nil {
		fail("%v", err)
	}
}

func run() (err error) {
	out := flag.String("out", "", "output path (local filesystem)")
	codec := flag.String("codec", "gz", "data-block codec: none | gz")
	rows := flag.Int("rows", 100, "number of cells to write")
	blockSize := flag.Int("block-size", 4096, "data-block size threshold (bytes)")
	rowFmt := flag.String("row-format", "row%05d", "fmt.Sprintf format for row keys")
	cf := flag.String("cf", "cf", "column family for every cell")
	cq := flag.String("cq", "cq", "column qualifier for every cell")
	flag.Parse()

	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	if *rows < 1 {
		return fmt.Errorf("-rows must be >= 1")
	}
	if !strings.Contains(*rowFmt, "%") {
		return fmt.Errorf("-row-format must contain a %% placeholder for the row index")
	}

	be := local.New()
	w, err := be.Create(nil, *out)
	if err != nil {
		return fmt.Errorf("create %s: %w", *out, err)
	}
	var cleanupState storage.WriteCleanupState
	defer func() { storage.AbortOnError(&err, w, &cleanupState) }()

	rw, err := rfile.NewWriter(w, rfile.WriterOptions{
		Codec:     *codec,
		BlockSize: *blockSize,
	})
	if err != nil {
		return fmt.Errorf("rfile.NewWriter: %w", err)
	}

	// Codecs we don't support yet — re-check here because NewWriter only
	// catches the case where the codec isn't registered, and the caller
	// might have built a non-default Compressor.
	if !block.DefaultCompressor().Has(*codec) {
		return fmt.Errorf("codec %q is not registered in the default compressor", *codec)
	}

	for i := 0; i < *rows; i++ {
		k := &rfile.Key{
			Row:              []byte(fmt.Sprintf(*rowFmt, i)),
			ColumnFamily:     []byte(*cf),
			ColumnQualifier:  []byte(*cq),
			ColumnVisibility: []byte(""),
			Timestamp:        int64(i + 1),
		}
		val := []byte(fmt.Sprintf("value-%d-%s", i, strings.Repeat("x", 20)))
		if err = rw.Append(k, val); err != nil {
			return fmt.Errorf("Append %d: %w", i, err)
		}
	}
	if err = rw.Close(); err != nil {
		return fmt.Errorf("rfile close: %w", err)
	}
	cleanupState.MarkCloseAttempted()
	if err = w.Close(); err != nil {
		return fmt.Errorf("storage close %s: %w", *out, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %d cells to %s (codec=%s, block-size=%d)\n",
		*rows, *out, *codec, *blockSize)
	return nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "shoal-rfile-write: "+format+"\n", args...)
	os.Exit(1)
}
