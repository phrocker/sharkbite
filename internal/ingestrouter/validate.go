// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package ingestrouter

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"math"
)

// Limits bounds a fully validated request before any tablet is contacted.
type Limits struct {
	MaxRequestBytes    int
	MaxSessionRequests int
	MaxBatches         int
	MaxMutations       int
	MaxUpdates         int
	MaxTableIDBytes    int
	MaxExtentRowBytes  int
	MaxRowBytes        int
	MaxColumnBytes     int
	MaxVisibilityBytes int
	MaxValueBytes      int
	MaxRequestIDBytes  int
	MaxVisibilityDepth int
	MinTimestamp       int64
	MaxTimestamp       int64
}

// DefaultLimits returns conservative limits suitable for an RPC boundary.
func DefaultLimits() Limits {
	return Limits{
		MaxRequestBytes:    16 << 20,
		MaxSessionRequests: 4_096,
		MaxBatches:         1_024,
		MaxMutations:       100_000,
		MaxUpdates:         1_000_000,
		MaxTableIDBytes:    1_024,
		MaxExtentRowBytes:  1 << 20,
		MaxRowBytes:        1 << 20,
		MaxColumnBytes:     1 << 20,
		MaxVisibilityBytes: 1 << 16,
		MaxValueBytes:      16 << 20,
		MaxRequestIDBytes:  256,
		MaxVisibilityDepth: 64,
		MinTimestamp:       math.MinInt64,
		MaxTimestamp:       math.MaxInt64,
	}
}

func (l Limits) validate() error {
	if l.MaxRequestBytes <= 0 || l.MaxSessionRequests <= 0 ||
		l.MaxBatches <= 0 || l.MaxMutations <= 0 ||
		l.MaxUpdates <= 0 || l.MaxTableIDBytes <= 0 || l.MaxExtentRowBytes <= 0 ||
		l.MaxRowBytes <= 0 || l.MaxColumnBytes <= 0 ||
		l.MaxVisibilityBytes <= 0 || l.MaxValueBytes <= 0 ||
		l.MaxRequestIDBytes <= 0 || l.MaxVisibilityDepth <= 0 ||
		l.MinTimestamp > l.MaxTimestamp {
		return fmt.Errorf("%w: invalid limits", ErrInvalidBatch)
	}
	return nil
}

type validatedRequest struct {
	request Request
	digest  [sha256.Size]byte
}

func validateRequest(tableID string, limits Limits, request Request) (validatedRequest, error) {
	if request.ID == "" || len(request.ID) > limits.MaxRequestIDBytes {
		return validatedRequest{}, fmt.Errorf("%w: invalid request id", ErrInvalidBatch)
	}
	if len(request.Batches) == 0 || len(request.Batches) > limits.MaxBatches {
		return validatedRequest{}, fmt.Errorf("%w: invalid batch count", ErrInvalidBatch)
	}

	cloned := Request{ID: request.ID, Batches: make([]Batch, len(request.Batches))}
	seen := make(map[string]struct{}, len(request.Batches))
	totalBytes := len(request.ID)
	mutations := 0
	updates := 0
	for batchIndex, batch := range request.Batches {
		if err := batch.Extent.Validate(); err != nil {
			return validatedRequest{}, fmt.Errorf("batch %d: %w", batchIndex, err)
		}
		if batch.Extent.TableID != tableID {
			return validatedRequest{}, fmt.Errorf("%w: batch %d table %q does not match session table %q",
				ErrInvalidBatch, batchIndex, batch.Extent.TableID, tableID)
		}
		if len(batch.Extent.TableID) > limits.MaxTableIDBytes ||
			len(batch.Extent.PrevEndRow) > limits.MaxExtentRowBytes ||
			len(batch.Extent.EndRow) > limits.MaxExtentRowBytes {
			return validatedRequest{}, fmt.Errorf("%w: batch %d extent size limit exceeded",
				ErrInvalidBatch, batchIndex)
		}
		extentBytes := len(batch.Extent.TableID) + len(batch.Extent.PrevEndRow) + len(batch.Extent.EndRow)
		if extentBytes > limits.MaxRequestBytes-totalBytes {
			return validatedRequest{}, fmt.Errorf("%w: request byte limit exceeded", ErrInvalidBatch)
		}
		totalBytes += extentBytes
		key := batch.Extent.Key()
		if _, duplicate := seen[key]; duplicate {
			return validatedRequest{}, fmt.Errorf("%w: duplicate extent %s", ErrInvalidBatch, key)
		}
		seen[key] = struct{}{}
		if len(batch.Mutations) == 0 {
			return validatedRequest{}, fmt.Errorf("%w: batch %d has no mutations", ErrInvalidBatch, batchIndex)
		}
		mutations += len(batch.Mutations)
		if mutations > limits.MaxMutations {
			return validatedRequest{}, fmt.Errorf("%w: mutation limit exceeded", ErrInvalidBatch)
		}

		outBatch := Batch{
			Extent:    batch.Extent.clone(),
			Mutations: make([]Mutation, len(batch.Mutations)),
		}
		for mutationIndex, mutation := range batch.Mutations {
			if len(mutation.Row) == 0 || len(mutation.Row) > limits.MaxRowBytes {
				return validatedRequest{}, fmt.Errorf("%w: batch %d mutation %d has invalid row",
					ErrInvalidBatch, batchIndex, mutationIndex)
			}
			if !batch.Extent.Contains(mutation.Row) {
				return validatedRequest{}, fmt.Errorf("%w: row %q is outside extent %s",
					ErrInvalidBatch, mutation.Row, key)
			}
			if len(mutation.Updates) == 0 {
				return validatedRequest{}, fmt.Errorf("%w: batch %d mutation %d has no updates",
					ErrInvalidBatch, batchIndex, mutationIndex)
			}
			updates += len(mutation.Updates)
			if updates > limits.MaxUpdates {
				return validatedRequest{}, fmt.Errorf("%w: update limit exceeded", ErrInvalidBatch)
			}
			outMutation := Mutation{
				Row:     append([]byte(nil), mutation.Row...),
				Updates: make([]Update, len(mutation.Updates)),
			}
			if len(mutation.Row) > limits.MaxRequestBytes-totalBytes {
				return validatedRequest{}, fmt.Errorf("%w: request byte limit exceeded", ErrInvalidBatch)
			}
			totalBytes += len(mutation.Row)
			for updateIndex, update := range mutation.Updates {
				if len(update.ColumnFamily) > limits.MaxColumnBytes ||
					len(update.ColumnQualifier) > limits.MaxColumnBytes {
					return validatedRequest{}, fmt.Errorf("%w: batch %d mutation %d update %d column limit exceeded",
						ErrInvalidBatch, batchIndex, mutationIndex, updateIndex)
				}
				if len(update.ColumnVisibility) > limits.MaxVisibilityBytes {
					return validatedRequest{}, fmt.Errorf("%w: visibility limit exceeded", ErrInvalidBatch)
				}
				if err := validateVisibility(update.ColumnVisibility, limits.MaxVisibilityDepth); err != nil {
					return validatedRequest{}, fmt.Errorf("%w: batch %d mutation %d update %d: %v",
						ErrInvalidBatch, batchIndex, mutationIndex, updateIndex, err)
				}
				if len(update.Value) > limits.MaxValueBytes {
					return validatedRequest{}, fmt.Errorf("%w: value limit exceeded", ErrInvalidBatch)
				}
				if update.Delete && len(update.Value) != 0 {
					return validatedRequest{}, fmt.Errorf("%w: delete carries a value", ErrInvalidBatch)
				}
				if update.Timestamp.Set &&
					(update.Timestamp.Value < limits.MinTimestamp ||
						update.Timestamp.Value > limits.MaxTimestamp) {
					return validatedRequest{}, fmt.Errorf("%w: timestamp outside configured range", ErrInvalidBatch)
				}
				if !update.Timestamp.Set && update.Timestamp.Value != 0 {
					return validatedRequest{}, fmt.Errorf("%w: server-assigned timestamp carries a value", ErrInvalidBatch)
				}
				updateBytes := len(update.ColumnFamily) + len(update.ColumnQualifier) +
					len(update.ColumnVisibility) + len(update.Value) + 16
				if updateBytes > limits.MaxRequestBytes-totalBytes {
					return validatedRequest{}, fmt.Errorf("%w: request byte limit exceeded", ErrInvalidBatch)
				}
				totalBytes += updateBytes
				outMutation.Updates[updateIndex] = Update{
					ColumnFamily:     append([]byte(nil), update.ColumnFamily...),
					ColumnQualifier:  append([]byte(nil), update.ColumnQualifier...),
					ColumnVisibility: append([]byte(nil), update.ColumnVisibility...),
					Timestamp:        update.Timestamp,
					Value:            append([]byte(nil), update.Value...),
					Delete:           update.Delete,
				}
			}
			outBatch.Mutations[mutationIndex] = outMutation
		}
		cloned.Batches[batchIndex] = outBatch
	}

	return validatedRequest{request: cloned, digest: digestRequest(cloned)}, nil
}

func digestRequest(request Request) [sha256.Size]byte {
	h := sha256.New()
	writeHashBytes(h, []byte(request.ID))
	writeHashInt(h, len(request.Batches))
	for _, batch := range request.Batches {
		writeHashBytes(h, []byte(batch.Extent.TableID))
		writeHashBytes(h, batch.Extent.PrevEndRow)
		writeHashBytes(h, batch.Extent.EndRow)
		writeHashInt(h, len(batch.Mutations))
		for _, mutation := range batch.Mutations {
			writeHashBytes(h, mutation.Row)
			writeHashInt(h, len(mutation.Updates))
			for _, update := range mutation.Updates {
				writeHashBytes(h, update.ColumnFamily)
				writeHashBytes(h, update.ColumnQualifier)
				writeHashBytes(h, update.ColumnVisibility)
				var fields [10]byte
				if update.Timestamp.Set {
					fields[0] = 1
				}
				if update.Delete {
					fields[1] = 1
				}
				binary.BigEndian.PutUint64(fields[2:], uint64(update.Timestamp.Value))
				_, _ = h.Write(fields[:])
				writeHashBytes(h, update.Value)
			}
		}
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func writeHashInt(h hash.Hash, value int) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = h.Write(encoded[:])
}

func writeHashBytes(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}

type visibilityParser struct {
	value    []byte
	position int
	depth    int
	maxDepth int
}

func validateVisibility(value []byte, maxDepth int) error {
	if len(value) == 0 {
		return nil
	}
	p := visibilityParser{value: value, maxDepth: maxDepth}
	if err := p.expression(); err != nil {
		return fmt.Errorf("invalid visibility: %w", err)
	}
	if p.position != len(value) {
		return fmt.Errorf("invalid visibility: unexpected byte at %d", p.position)
	}
	return nil
}

func (p *visibilityParser) expression() error {
	if err := p.term(); err != nil {
		return err
	}
	var operator byte
	for p.position < len(p.value) && (p.value[p.position] == '&' || p.value[p.position] == '|') {
		next := p.value[p.position]
		if operator != 0 && operator != next {
			return fmt.Errorf("mixed operators require parentheses")
		}
		operator = next
		p.position++
		if err := p.term(); err != nil {
			return err
		}
	}
	return nil
}

func (p *visibilityParser) term() error {
	if p.position >= len(p.value) {
		return fmt.Errorf("missing term")
	}
	if p.value[p.position] == '(' {
		p.depth++
		if p.depth > p.maxDepth {
			return fmt.Errorf("nesting limit exceeded")
		}
		p.position++
		if err := p.expression(); err != nil {
			return err
		}
		if p.position >= len(p.value) || p.value[p.position] != ')' {
			return fmt.Errorf("missing closing parenthesis")
		}
		p.position++
		p.depth--
		return nil
	}
	if p.value[p.position] == '"' {
		return p.quoted()
	}
	start := p.position
	for p.position < len(p.value) && visibilityLabelByte(p.value[p.position]) {
		p.position++
	}
	if p.position == start {
		return fmt.Errorf("invalid label byte at %d", p.position)
	}
	return nil
}

func (p *visibilityParser) quoted() error {
	p.position++
	content := 0
	for p.position < len(p.value) {
		switch p.value[p.position] {
		case '"':
			if content == 0 {
				return fmt.Errorf("empty quoted label")
			}
			p.position++
			return nil
		case '\\':
			p.position++
			if p.position >= len(p.value) ||
				(p.value[p.position] != '\\' && p.value[p.position] != '"') {
				return fmt.Errorf("invalid quoted-label escape")
			}
		}
		content++
		p.position++
	}
	return fmt.Errorf("unterminated quoted label")
}

func visibilityLabelByte(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' ||
		value == '_' || value == ':' || value == '.' ||
		value == '/' || value == '-'
}
