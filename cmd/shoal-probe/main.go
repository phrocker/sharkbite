package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
)

func main() {
	cap := flag.Int("cap", 0, "stop after N cells (0 = walk to EOF)")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: shoal-probe [-cap N] <path>")
		os.Exit(2)
	}
	path := flag.Arg(0)

	tReadFile := time.Now()
	bs, err := os.ReadFile(path)
	must(err)
	dReadFile := time.Since(tReadFile)

	tOpen := time.Now()
	bc, err := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
	must(err)
	r, err := rfile.Open(bc, block.Default())
	must(err)
	defer r.Close()
	dOpen := time.Since(tOpen)

	fmt.Printf("BCFile version=%s\n", bc.Footer().Version)
	fmt.Printf("file size=%d bytes (read in %v)\n", len(bs), dReadFile)
	fmt.Printf("Open(): %v\n", dOpen)
	fmt.Printf("MetaIndex:\n")
	for name, e := range bc.MetaIndex().Entries {
		fmt.Printf("  %q codec=%s region=%+v\n", name, e.CompressionAlgo, e.Region)
	}
	lg := r.LocalityGroup()
	fmt.Printf("default LG: name=%q firstKey-row=%q cfCount=%d numTotalEntries=%d\n",
		lg.Name, lg.FirstKey.Row, len(lg.ColumnFamilies), lg.NumTotalEntries)
	fmt.Printf("RootIndex.Level=%d entries=%d\n", lg.RootIndex.Level, lg.RootIndex.NumEntries())

	tWalk := time.Now()
	count := 0
	bytesValues := int64(0)
	bytesKeys := int64(0)
	var lastKey *rfile.Key
	for *cap == 0 || count < *cap {
		k, v, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		must(err)
		if lastKey != nil && k.Compare(lastKey) < 0 {
			fmt.Printf("non-monotonic at cell %d: prev=%v curr=%v\n", count, lastKey, k)
			os.Exit(3)
		}
		bytesKeys += int64(len(k.Row) + len(k.ColumnFamily) + len(k.ColumnQualifier) + len(k.ColumnVisibility) + 9)
		bytesValues += int64(len(v))
		lastKey = k.Clone()
		count++
	}
	dWalk := time.Since(tWalk)

	fmt.Printf("\nwalked %d cells in %v\n", count, dWalk)
	if count > 0 {
		cellsPerSec := float64(count) / dWalk.Seconds()
		mibPerSec := float64(bytesKeys+bytesValues) / dWalk.Seconds() / (1 << 20)
		fmt.Printf("  %.0f cells/s   %.1f MiB/s (logical bytes; ignores compression)\n", cellsPerSec, mibPerSec)
		fmt.Printf("  %.1f MiB/s (file bytes; %d compressed bytes / %v)\n",
			float64(len(bs))/dWalk.Seconds()/(1<<20), len(bs), dWalk)
		fmt.Printf("  bytesKeys=%d bytesValues=%d\n", bytesKeys, bytesValues)
	}
	if lastKey != nil {
		fmt.Printf("last row=%q cf=%q ts=%d\n", lastKey.Row, lastKey.ColumnFamily, lastKey.Timestamp)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
