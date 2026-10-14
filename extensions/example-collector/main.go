// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Command example-collector is a synthetic non-session collector. It tails a
// text file and reports each complete new line to Shoal as a raw artifact
// reference plus one extractor-versioned observation.
//
// It exists to prove the extension boundary: it imports from Shoal only
// pkg/sdk and the public contracts it exposes (pkg/collector), never
// internals. The collector must first be provisioned by an operator; it can
// request only authority that provisioning granted.
//
//	SHOAL_TOKEN=... example-collector -base-url https://shoal.example \
//	    -collector collector:tail -authority authority:logs -file /var/log/app.log
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/sdk"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Extractor identifies this program's line parser. Bump Version when the
// payload it produces changes; earlier observations stay as they were.
var Extractor = collector.ExtractorRef{ID: "example:line-fields", Version: "1"}

// maxLine bounds one reported line; longer lines are reported unextractable.
const maxLine = 16 * 1024

// tailer reads complete lines appended to a file since the last call.
type tailer struct {
	path    string
	offset  int64
	partial []byte
}

func (t *tailer) next() ([][]byte, error) {
	f, err := os.Open(t.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() < t.offset {
		t.offset, t.partial = 0, nil // Truncated or rotated: start over.
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, err
	}
	chunk, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, err
	}
	t.offset += int64(len(chunk))
	data := append(t.partial, chunk...)
	var lines [][]byte
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, bytes.Clone(data[:i]))
		data = data[i+1:]
	}
	t.partial = bytes.Clone(data)
	return lines, nil
}

// artifactFor names a line by its content digest and read time, so the same
// text read again later is a distinct artifact. Identical lines read in one
// poll collapse into one artifact, which is acceptable for this example.
func artifactFor(line []byte, now time.Time) collector.ArtifactRef {
	sum := sha256.Sum256(line)
	digest := hex.EncodeToString(sum[:])
	return collector.ArtifactRef{ID: shoal.ID("line:" + digest + "@" + now.Format(time.RFC3339Nano)), Digest: digest, Size: int64(len(line)), MediaType: "text/plain", ObservedAt: now}
}

// observationFor is the extractor: it reports the line's length and field
// count. Overlong or empty lines are unextractable rather than guessed at.
func observationFor(collectorID shoal.ID, artifact collector.ArtifactRef, line []byte, now time.Time) (collector.Observation, error) {
	c := collector.ObservationConfig{CollectorID: collectorID, ArtifactID: artifact.ID, Extractor: Extractor, Kind: "log_line", ObservedAt: now}
	if len(line) == 0 || len(line) > maxLine {
		c.Confidence = collector.Confidence{Disposition: collector.Unextractable}
		return collector.NewObservation(c)
	}
	payload, err := json.Marshal(struct {
		Length int `json:"length"`
		Fields int `json:"fields"`
	}{len(line), len(strings.Fields(string(line)))})
	if err != nil {
		return collector.Observation{}, err
	}
	c.Payload = payload
	c.Confidence = collector.Confidence{Disposition: collector.Extracted}
	return collector.NewObservation(c)
}

// report submits one line. Every call is idempotent, so a write reported as
// indeterminate is simply retried on the next poll by the caller.
func report(ctx context.Context, client *sdk.Client, collectorID shoal.ID, line []byte, now time.Time) error {
	artifact := artifactFor(line, now)
	if _, err := client.Collectors().SubmitArtifact(ctx, collectorID, artifact); err != nil {
		return fmt.Errorf("artifact: %w", err)
	}
	o, err := observationFor(collectorID, artifact, line, now)
	if err != nil {
		return err
	}
	if _, err := client.Collectors().SubmitObservation(ctx, o); err != nil {
		return fmt.Errorf("observation: %w", err)
	}
	return nil
}

func enroll(ctx context.Context, client *sdk.Client, collectorID shoal.ID, authority []shoal.ID) error {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	receipt, err := client.Collectors().Enroll(ctx, key, collector.EnrollRequest{CollectorID: collectorID, RequestedAuthorityPolicyIDs: authority, Extractors: []collector.ExtractorRef{Extractor}})
	if err != nil {
		return err
	}
	log.Printf("enrolled %s at generation %d", collectorID, receipt.Generation)
	return nil
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("example-collector", flag.ContinueOnError)
	base := flags.String("base-url", "", "Shoal base URL")
	id := flags.String("collector", "", "provisioned collector ID")
	authority := flags.String("authority", "", "comma-separated authority policy IDs to request")
	file := flags.String("file", "", "file to tail")
	poll := flags.Duration("poll", time.Second, "poll interval")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *base == "" || *id == "" || *authority == "" || *file == "" {
		return errors.New("base-url, collector, authority and file are required")
	}
	token := os.Getenv("SHOAL_TOKEN")
	client, err := sdk.New(sdk.Config{BaseURL: *base, Token: func(context.Context) (string, error) { return token, nil }})
	if err != nil {
		return err
	}
	var requested []shoal.ID
	for _, a := range strings.Split(*authority, ",") {
		requested = append(requested, shoal.ID(strings.TrimSpace(a)))
	}
	collectorID := shoal.ID(*id)
	if err := enroll(ctx, client, collectorID, requested); err != nil {
		return fmt.Errorf("enroll: %w", err)
	}
	t := &tailer{path: *file}
	ticker := time.NewTicker(*poll)
	defer ticker.Stop()
	// A line keeps the time it was first read, so a retried report is the
	// identical request and the server's idempotency applies.
	type line struct {
		body []byte
		at   time.Time
	}
	var pending []line
	for {
		lines, err := t.next()
		if err != nil {
			log.Printf("read: %v", err)
		}
		now := time.Now().UTC().Round(0)
		for _, l := range lines {
			pending = append(pending, line{l, now})
		}
		for len(pending) > 0 {
			if err := report(ctx, client, collectorID, pending[0].body, pending[0].at); err != nil {
				log.Printf("report: %v", err)
				break
			}
			pending = pending[1:]
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
