// shoal-scan-client is a debug/smoke-test tool that issues a single
// StartScan request to a shoal listener (or any tserver speaking the
// same Thrift surface) and dumps the cells.
//
// Used for:
//   - Wire-path validation against a fresh shoal daemon
//   - A/B comparison: same scan vs shoal vs real tserver, diff outputs
//   - Quick scans without booting a full Java client
//
// Example:
//
//	./shoal-scan-client -addr 127.0.0.1:9801 \
//	    -instance accumulo -accumulo-version 4.0.0-SNAPSHOT \
//	    -user root -table 1 \
//	    -prev-end-row "concept:triviaqa" \
//	    -auths system,read
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/cred"
	"github.com/phrocker/shoal-oss/internal/scanclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/zk"
)

func main() {
	addr := flag.String("addr", "", "shoal/tserver host:port")
	zkServers := flag.String("zk", "", "ZK quorum (used to resolve instance UUID)")
	instanceName := flag.String("instance", "accumulo", "Accumulo instance name")
	accVersion := flag.String("accumulo-version", "4.0.0-SNAPSHOT", "server major.minor must match")
	user := flag.String("user", "root", "principal")
	password := flag.String("password", "", "password (NOT recommended; prefer SHOAL_PASSWORD env)")
	tableID := flag.String("table", "", "table ID (e.g. \"1\" or \"!0\")")
	prevEndRow := flag.String("prev-end-row", "", "tablet PrevEndRow (empty = absent / first tablet)")
	endRow := flag.String("end-row", "", "tablet EndRow (empty = absent / last tablet)")
	startRow := flag.String("start-row", "", "scan range start row (empty = -inf)")
	stopRow := flag.String("stop-row", "", "scan range stop row (empty = +inf)")
	authsArg := flag.String("auths", "", "comma-separated authorizations")
	maxCells := flag.Int("max-cells", 50, "stop after printing this many cells")
	dialTimeout := flag.Duration("dial-timeout", 10*time.Second, "Thrift connect timeout")
	scanTimeout := flag.Duration("scan-timeout", 30*time.Second, "scan deadline")
	flag.Parse()

	if *addr == "" || *zkServers == "" || *tableID == "" {
		fmt.Fprintln(os.Stderr, "shoal-scan-client: -addr, -zk, and -table are required")
		os.Exit(2)
	}
	if *password == "" {
		*password = os.Getenv("SHOAL_PASSWORD")
	}
	if *password == "" {
		fmt.Fprintln(os.Stderr, "shoal-scan-client: password required (-password or SHOAL_PASSWORD env)")
		os.Exit(2)
	}

	servers := strings.Split(*zkServers, ",")
	loc, err := zk.New(servers, *instanceName, 30*time.Second)
	if err != nil {
		fail("zk.New: %v", err)
	}
	instanceID := loc.InstanceID()
	loc.Close()

	creds := cred.NewPasswordCreds(*user, *password, instanceID)
	auths := splitAuths(*authsArg)

	ctx, cancel := context.WithTimeout(context.Background(), *scanTimeout)
	defer cancel()

	c, err := scanclient.DialContext(ctx, *addr, instanceID, *accVersion, *dialTimeout)
	if err != nil {
		fail("scanclient.DialContext: %v", err)
	}
	defer c.Close()

	extent := &data.TKeyExtent{
		Table:      []byte(*tableID),
		EndRow:     bytesOrNil(*endRow),
		PrevEndRow: bytesOrNil(*prevEndRow),
	}
	rangeArg := &data.TRange{}
	if *startRow == "" {
		rangeArg.InfiniteStartKey = true
	} else {
		rangeArg.Start = &data.TKey{Row: []byte(*startRow)}
		rangeArg.StartKeyInclusive = true
	}
	if *stopRow == "" {
		rangeArg.InfiniteStopKey = true
	} else {
		rangeArg.Stop = &data.TKey{Row: []byte(*stopRow)}
		rangeArg.StopKeyInclusive = false
	}

	t0 := time.Now()
	resp, err := c.Start(ctx, scanclient.StartRequest{
		Credentials:    creds,
		Extent:         extent,
		Range:          rangeArg,
		BatchSize:      1024,
		Authorizations: auths,
	})
	dur := time.Since(t0)
	if err != nil {
		fail("StartScan: %v", err)
	}
	if resp.Result_ == nil {
		fail("nil ScanResult")
	}

	fmt.Fprintf(os.Stderr, "received %d cells in %v (more=%v)\n",
		len(resp.Result_.Results), dur, resp.Result_.More)

	for i, kv := range resp.Result_.Results {
		if i >= *maxCells {
			fmt.Fprintf(os.Stderr, "(truncated; %d more)\n", len(resp.Result_.Results)-i)
			break
		}
		fmt.Printf("[%4d] row=%q cf=%q cq=%q cv=%q ts=%d val=%q\n",
			i, printable(kv.Key.Row), printable(kv.Key.ColFamily),
			printable(kv.Key.ColQualifier), printable(kv.Key.ColVisibility),
			kv.Key.Timestamp, printable(kv.Value))
	}
}

func splitAuths(s string) [][]byte {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([][]byte, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, []byte(p))
		}
	}
	return out
}

func bytesOrNil(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

func printable(b []byte) string {
	if b == nil {
		return ""
	}
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c >= 0x20 && c < 0x7F {
			out = append(out, c)
		} else {
			out = append(out, '.')
		}
	}
	return string(out)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "shoal-scan-client: "+format+"\n", args...)
	os.Exit(1)
}
