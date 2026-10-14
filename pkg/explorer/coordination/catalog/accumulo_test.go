/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 */

package catalog

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
)

type recordingScanner struct {
	scanRange *accumulo.Range
	err       error
}

func (s *recordingScanner) Stream(
	_ context.Context,
	scanRange *accumulo.Range,
) (*accumulo.ResultStream, error) {
	s.scanRange = scanRange
	return nil, s.err
}

func TestAccumuloPrefixScanUsesExactExclusiveSuccessor(t *testing.T) {
	sentinel := errors.New("stop after mapping")
	scanner := &recordingScanner{err: sentinel}
	store := &AccumuloStore{scanner: scanner}
	prefix := []byte{1, 2, 0xff}
	_, err := store.ScanPrefix(
		context.Background(), prefix, []byte("a"), []byte("active"), []byte("svc"), 4,
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("scan error = %v", err)
	}

	if scanner.scanRange == nil {
		t.Fatal("scanner did not receive a range")
	}
	if !bytes.Equal(scanner.scanRange.StartRow(), prefix) {
		t.Fatalf("start row = %x, want %x", scanner.scanRange.StartRow(), prefix)
	}
	if !bytes.Equal(scanner.scanRange.EndRow(), []byte{1, 3}) {
		t.Fatalf("end row = %x, want 0103", scanner.scanRange.EndRow())
	}
}

func TestAccumuloPrefixSeekUsesTrustedStartAndPrefixEnd(t *testing.T) {
	policyPrefix, _ := coordination.PolicyCopyMapPrefix([]byte("domain"), []byte("part"))
	policyStart, _ := coordination.PolicyCopyMapSeek([]byte("domain"), []byte("part"), 17)
	indexPrefix, _ := coordination.IndexActivationPrefix([]byte("domain"), []byte("lexical"))
	indexStart, _ := coordination.IndexActivationSeek([]byte("domain"), []byte("lexical"), 23)
	for name, test := range map[string]struct {
		prefix []byte
		start  []byte
	}{
		"policy": {prefix: policyPrefix, start: policyStart},
		"index":  {prefix: indexPrefix, start: indexStart},
	} {
		t.Run(name, func(t *testing.T) {
			sentinel := errors.New("stop after mapping")
			scanner := &recordingScanner{err: sentinel}
			store := &AccumuloStore{scanner: scanner}
			_, err := store.ScanPrefixFrom(
				context.Background(), test.prefix, test.start,
				[]byte("m"), []byte("active"), []byte("svc"), 4,
			)
			if !errors.Is(err, sentinel) {
				t.Fatalf("scan error = %v", err)
			}
			if !bytes.Equal(scanner.scanRange.StartRow(), test.start) {
				t.Fatalf("start row = %x, want %x", scanner.scanRange.StartRow(), test.start)
			}
			end, _ := prefixSuccessor(test.prefix)
			if !bytes.Equal(scanner.scanRange.EndRow(), end) {
				t.Fatalf("end row = %x, want %x", scanner.scanRange.EndRow(), end)
			}
		})
	}
	store := &AccumuloStore{}
	if _, err := store.ScanPrefixFrom(
		context.Background(), policyPrefix, indexStart,
		[]byte("m"), []byte("active"), []byte("svc"), 4,
	); !errors.Is(err, ErrBounds) {
		t.Fatalf("out-of-prefix start error = %v", err)
	}
}

func TestPrefixSuccessorBoundaries(t *testing.T) {
	if value, ok := prefixSuccessor([]byte{0x01, 0xff, 0xff}); !ok || !bytes.Equal(value, []byte{0x02}) {
		t.Fatalf("successor = %x, %v", value, ok)
	}
	if _, ok := prefixSuccessor([]byte{0xff, 0xff}); ok {
		t.Fatal("all-ff prefix unexpectedly has a successor")
	}
}

func TestCleanupErrorMeansScanResultsRemainUsable(t *testing.T) {
	cleanup := &accumulo.CleanupError{ScanID: 19, Err: errors.New("close failed")}
	if err := usableScanCloseError(cleanup); err != nil {
		t.Fatalf("usableScanCloseError = %v, want nil", err)
	}
	sentinel := errors.New("stream failed")
	if err := usableScanCloseError(sentinel); !errors.Is(err, sentinel) {
		t.Fatalf("usableScanCloseError = %v, want stream failure", err)
	}
}
