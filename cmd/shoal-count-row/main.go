package main

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"io"
	"os"
	"strings"
)

func main() {
	bs, _ := os.ReadFile(os.Args[1])
	bc, _ := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
	readers, err := rfile.OpenAll(bc, block.Default())
	if err != nil {
		fmt.Println("openAll:", err)
		os.Exit(1)
	}
	target := os.Args[2]
	n := 0
	for lgIdx, r := range readers {
		defer r.Close()
		lg := r.LocalityGroup()
		lgName := lg.Name
		if lg.IsDefault {
			lgName = "<DEFAULT>"
		}
		fmt.Printf("=== LG %d %q (cfs=%d entries=%d) ===\n", lgIdx, lgName, len(lg.ColumnFamilies), lg.NumTotalEntries)
		if err := r.SeekRow([]byte(target)); err != nil {
			fmt.Println("seek:", err)
			continue
		}
		for {
			k, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fmt.Println("err:", err)
				break
			}
			if !strings.HasPrefix(string(k.Row), target) {
				break
			}
			fmt.Printf("%4d  row=%q cf=%q cq=%q cv=%q ts=%d\n", n, k.Row, k.ColumnFamily, k.ColumnQualifier, k.ColumnVisibility, k.Timestamp)
			n++
			if n > 50 {
				break
			}
		}
	}
	fmt.Printf("--- total %d cells (prefix=%q) across %d LGs ---\n", n, target, len(readers))
}
