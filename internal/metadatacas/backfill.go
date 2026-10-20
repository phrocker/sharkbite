// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package metadatacas

import (
	"context"
	"errors"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/metadata"
)

// ErrRootBackfillUnsupported reports an attempt to backfill root metadata.
var ErrRootBackfillUnsupported = errors.New(
	"metadatacas: the root tablet's file entries cannot be backfilled from outside its owner")

// BackfillTarget names one existing metadata file entry. These fields remain
// available so callers can construct a target while the sidecar is designed.
type BackfillTarget struct {
	TableID           string
	PrevEndRow        []byte
	EndRow            []byte
	FileQualifier     []byte
	FileValue         []byte
	ExistingEmbedding []byte
}

// BackfillWriter cannot write a Shoal-only family into Accumulo metadata.
// Accumulo 4 rejects unknown families when it parses tablet metadata, so a
// compatible external sidecar is required before this API can be enabled.
type BackfillWriter struct{}

func NewBackfillWriter(
	reader TabletReader, locator RootLocator, writer ConditionalWriter,
) (*BackfillWriter, error) {
	if reader == nil || locator == nil || writer == nil {
		return nil, ErrInvalidConfig
	}
	return nil, ErrEmbeddingSidecarRequired
}

func (w *BackfillWriter) WriteFileEmbedding(
	_ context.Context, target BackfillTarget, _ embeddingspace.FileState,
) (bool, error) {
	if w == nil {
		return false, ErrInvalidConfig
	}
	if target.TableID == metadata.RootTableID {
		return false, ErrRootBackfillUnsupported
	}
	return false, ErrEmbeddingSidecarRequired
}
