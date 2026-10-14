// shoal-rfile-pull copies a single object (typically a small RFile) from
// GCS or HDFS to local disk, so we can drop it into a testdata/ directory and
// exercise the rfile reader against real cluster bytes offline.
//
// Example:
//
//	./shoal-rfile-pull -src gs://your-bucket/accumulo/tables/2k/A0001.rf \
//	                   -dst internal/rfile/testdata/captured.rf
//
// Auth uses standard GCS Application Default Credentials or Hadoop
// configuration from HADOOP_CONF_DIR/HADOOP_HOME.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/gcs"
	"github.com/phrocker/shoal-oss/internal/storage/hdfs"
	"github.com/phrocker/shoal-oss/internal/storage/local"
)

func main() {
	src := flag.String("src", "", "source path (gs://bucket/object, hdfs://namenode/path, or GCS bucket/object)")
	dst := flag.String("dst", "", "destination local filesystem path")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall fetch timeout")
	flag.Parse()

	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "shoal-rfile-pull: -src and -dst are required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var srcBE storage.Backend
	var cleanup func()
	if strings.HasPrefix(*src, "hdfs:") {
		address := os.Getenv("SHOAL_HDFS_NAMENODE")
		if address == "" {
			var err error
			address, err = hdfs.AddressFromPath(*src)
			if err != nil {
				fail("HDFS source: %v", err)
			}
		}
		be, err := hdfs.NewContext(ctx, address)
		if err != nil {
			fail("hdfs.New: %v", err)
		}
		srcBE = be
		cleanup = func() { _ = be.Close() }
	} else {
		be, err := gcs.New(ctx)
		if err != nil {
			fail("gcs.New: %v", err)
		}
		srcBE = be
		cleanup = func() { _ = be.Close() }
	}
	defer cleanup()

	dstBE := local.New()

	start := time.Now()
	n, err := storage.Copy(ctx, srcBE, *src, dstBE, *dst)
	if err != nil {
		fail("copy: %v", err)
	}
	dur := time.Since(start)
	rate := float64(n) / dur.Seconds() / (1 << 20)
	fmt.Fprintf(os.Stderr, "copied %d bytes in %v (%.1f MiB/s) → %s\n", n, dur, rate, *dst)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "shoal-rfile-pull: "+format+"\n", args...)
	os.Exit(1)
}
