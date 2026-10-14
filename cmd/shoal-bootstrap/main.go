// shoal-bootstrap is a debug tool that walks the bootstrap chain
// (ZK → root tablet → metadata → user-table tablet→file map) against a
// real Accumulo cluster and prints the discovered topology.
//
// Useful for shaking out the protocol/scanclient/metadata pipeline end
// to end against a dev cluster.
//
// Example:
//
//	./shoal-bootstrap -zk zk-0:2181,zk-1:2181 -instance accumulo \
//	    -user root -password 'secret' -accumulo-version 4.0.0-SNAPSHOT
//
// Diagnostic mode:
//
//	./shoal-bootstrap ... -dump-root-scan -dump-out testdata/root.json
//
// dumps the raw (relative-key un-materialized) TKeyValue stream from the
// root tablet to stdout, and optionally writes it as JSON suitable for
// loading into the metadata package's ground-truth tests.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/cred"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/zk"
)

func main() {
	zkServers := flag.String("zk", "localhost:2181", "comma-separated ZK quorum")
	instanceName := flag.String("instance", "accumulo", "Accumulo instance name")
	user := flag.String("user", "root", "principal")
	password := flag.String("password", "", "password (PasswordToken); prefer env SHOAL_PASSWORD")
	accVersion := flag.String("accumulo-version", "4.0.0-SNAPSHOT", "server major.minor must match")
	zkTimeout := flag.Duration("zk-timeout", 30*time.Second, "ZK session timeout")
	scanTimeout := flag.Duration("scan-timeout", 30*time.Second, "scan deadline")
	verbose := flag.Bool("verbose", false, "emit slog DEBUG events (per-scan extents, durations)")
	dumpRoot := flag.Bool("dump-root-scan", false, "dump raw TKeyValue stream from the root tablet and exit (skips metadata + user-table walk)")
	dumpOut := flag.String("dump-out", "", "with -dump-root-scan: write JSON capture to this path (loadable by metadata ground-truth tests)")
	flag.Parse()

	// slog → stderr so JSON capture written to stdout (or to -dump-out) stays clean.
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if *password == "" {
		*password = os.Getenv("SHOAL_PASSWORD")
	}
	if *password == "" {
		fmt.Fprintln(os.Stderr, "shoal-bootstrap: password required (-password or SHOAL_PASSWORD env)")
		os.Exit(2)
	}

	servers := strings.Split(*zkServers, ",")
	loc, err := zk.New(servers, *instanceName, *zkTimeout)
	if err != nil {
		fail("zk.New: %v", err)
	}
	defer loc.Close()
	fmt.Fprintf(os.Stderr, "instance %q -> %s\n", *instanceName, loc.InstanceID())

	creds := cred.NewPasswordCreds(*user, *password, loc.InstanceID())
	walker := metadata.NewWalker(loc, creds, *accVersion)

	ctx, cancel := context.WithTimeout(context.Background(), *scanTimeout)
	defer cancel()

	if *dumpRoot {
		if err := runDumpRootScan(ctx, walker, *dumpOut); err != nil {
			fail("dump-root-scan: %v", err)
		}
		return
	}

	tabletsByTable, err := walker.BootstrapAll(ctx)
	if err != nil {
		fail("walker.BootstrapAll: %v", err)
	}

	for tableID, tablets := range tabletsByTable {
		fmt.Printf("\ntable %s — %d tablet(s)\n", tableID, len(tablets))
		for _, t := range tablets {
			loc := "<no location>"
			if t.Location != nil {
				loc = t.Location.HostPort
			}
			endRow := "+inf"
			if t.EndRow != nil {
				endRow = string(t.EndRow)
			}
			prevRow := "-inf"
			if t.PrevRow != nil {
				prevRow = string(t.PrevRow)
			}
			fmt.Printf("  (%s, %s] @ %s — %d file(s)\n", prevRow, endRow, loc, len(t.Files))
			for _, f := range t.Files {
				fmt.Printf("    %s  size=%d entries=%d", f.Path, f.Size, f.NumEntries)
				if f.Time >= 0 {
					fmt.Printf(" time=%d", f.Time)
				}
				fmt.Println()
			}
		}
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "shoal-bootstrap: "+format+"\n", args...)
	os.Exit(1)
}

// runDumpRootScan performs a single-shot scan of the root tablet and
// pretty-prints every TKeyValue *as it came off the wire* (i.e. before
// the AggregateRows relative-key cursor materializes empty fields).
//
// If outPath is set, it also writes a JSON capture loadable by
// metadata.LoadRootScanCapture in ground-truth tests.
func runDumpRootScan(ctx context.Context, walker *metadata.Walker, outPath string) error {
	host, kvs, more, err := walker.RawRootScan(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "root scan @ %s — %d KV(s), more=%v\n", host, len(kvs), more)
	for i, kv := range kvs {
		printRawKV(i, kv)
	}
	if outPath == "" {
		return nil
	}
	cap := buildCapture(host, kvs, more)
	buf, err := json.MarshalIndent(cap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	// 0644 — capture data is non-secret; commit-safe for golden tests.
	if err := os.WriteFile(outPath, buf, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Fprintf(os.Stderr, "wrote capture: %s (%d bytes)\n", outPath, len(buf))
	return nil
}

// printRawKV renders one TKeyValue with both a printable rendering and
// a hex one for fields that contain non-ASCII bytes. Empty fields are
// flagged "(inherit)" — Accumulo's relative-key compression overlays
// empties from the prior KV.
func printRawKV(i int, kv *data.TKeyValue) {
	if kv == nil || kv.Key == nil {
		fmt.Printf("KV[%d] <nil>\n", i)
		return
	}
	fmt.Printf("KV[%d] row=%s  cf=%s  cq=%s  cv=%s  ts=%d\n",
		i,
		fieldStr(kv.Key.Row),
		fieldStr(kv.Key.ColFamily),
		fieldStr(kv.Key.ColQualifier),
		fieldStr(kv.Key.ColVisibility),
		kv.Key.Timestamp,
	)
	fmt.Printf("       value=%s\n", fieldStr(kv.Value))
}

func fieldStr(b []byte) string {
	if b == nil {
		return "<nil>"
	}
	if len(b) == 0 {
		return "(inherit)"
	}
	pretty := metadata.PrintableBytes(b)
	if strings.HasPrefix(pretty, "0x") {
		return pretty // already hex
	}
	return fmt.Sprintf("%q [%s]", pretty, hex.EncodeToString(b))
}

// rootScanCaptureKV is the JSON shape used by ground-truth tests. All
// byte fields are hex-encoded so the file round-trips exactly.
type rootScanCaptureKV struct {
	RowHex   string `json:"rowHex"`
	CFHex    string `json:"cfHex"`
	CQHex    string `json:"cqHex"`
	CVHex    string `json:"cvHex"`
	TS       int64  `json:"ts"`
	ValueHex string `json:"valueHex"`
}

type rootScanCapture struct {
	Host       string              `json:"host"`
	More       bool                `json:"more"`
	CapturedAt string              `json:"capturedAt"`
	KVs        []rootScanCaptureKV `json:"kvs"`
}

func buildCapture(host string, kvs []*data.TKeyValue, more bool) rootScanCapture {
	out := rootScanCapture{
		Host:       host,
		More:       more,
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		KVs:        make([]rootScanCaptureKV, 0, len(kvs)),
	}
	for _, kv := range kvs {
		if kv == nil || kv.Key == nil {
			continue
		}
		out.KVs = append(out.KVs, rootScanCaptureKV{
			RowHex:   hex.EncodeToString(kv.Key.Row),
			CFHex:    hex.EncodeToString(kv.Key.ColFamily),
			CQHex:    hex.EncodeToString(kv.Key.ColQualifier),
			CVHex:    hex.EncodeToString(kv.Key.ColVisibility),
			TS:       kv.Key.Timestamp,
			ValueHex: hex.EncodeToString(kv.Value),
		})
	}
	return out
}
