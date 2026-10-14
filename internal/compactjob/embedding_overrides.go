// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.

package compactjob

import (
	"fmt"
	"strings"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

// EmbeddingOverrides builds the override entries that carry a
// compaction's embedding-space context to an external compactor.
//
// # Who is expected to call this
//
// Nothing inside this repository constructs a TExternalCompactionJob
// today. External compaction jobs are assembled by Accumulo's Java
// manager and arrive here over Thrift; shoal is the compactor, not the
// coordinator. So this function is the *producer* half of a wire format
// whose only in-tree consumer is Translate, and it is unreferenced by
// any shoal binary on purpose rather than by omission.
//
// It is exported anyway because the format needs exactly one definition.
// A Shoal-side coordinator, a test harness, or the Java patch that
// eventually populates these keys must all agree with Translate on
// encoding, key names and size limits, and the way to guarantee that is
// to have them call this rather than re-derive it. Round-tripping this
// function's output through Translate is also what keeps the encoder and
// the decoder honest about each other.
//
// # What silence means
//
// Until some coordinator calls this, external compaction jobs arrive
// carrying no embedding column at all. That is a supported state, not a
// broken one: every such input is *absent*, its space is read from its
// own file footer, and the compaction proceeds unconverged. What it is
// not is a claim — see InputFile.Embedding.
//
// states is keyed by the exact metadata file entry each input is named
// by in the job, because that string is the only identifier the
// coordinator and the compactor provably share. A key that names a file
// the job does not compact makes Translate refuse the job, so callers
// must pass the states for that job's inputs and no others.
//
// target and epoch may be empty, in which case their keys are omitted:
// an absent target means the table is not converging, and an absent
// epoch means the compaction is not part of a migration.
func EmbeddingOverrides(
	target, epoch string, states map[string]embeddingspace.FileState,
) (map[string]string, error) {
	out := make(map[string]string, 3)

	normalized, err := embeddingspace.ParseTarget(target)
	if err != nil {
		return nil, fmt.Errorf("compactjob: invalid embedding target: %w", err)
	}
	if normalized != "" {
		out[embeddingspace.TableTargetProperty] = normalized
	}

	if trimmed := strings.TrimSpace(epoch); trimmed != "" {
		if len(trimmed) > embeddingspace.MaxJobEpochBytes {
			return nil, fmt.Errorf("compactjob: embedding epoch is %d bytes, limit %d",
				len(trimmed), embeddingspace.MaxJobEpochBytes)
		}
		out[embeddingspace.JobEpochProperty] = trimmed
	}

	if len(states) > 0 {
		encoded, err := embeddingspace.EncodeFileStates(states)
		if err != nil {
			return nil, fmt.Errorf("compactjob: cannot encode per-file embedding column: %w", err)
		}
		out[embeddingspace.JobFileStatesProperty] = encoded
	}
	return out, nil
}

// ApplyEmbeddingOverrides merges EmbeddingOverrides into a job's
// override map.
//
// Merging rather than replacing matters: the override map already
// carries the output codec and block size, and dropping those would
// change the file the compactor writes. Existing embedding keys are
// replaced, because the caller of this function is the authority on
// them.
//
// See EmbeddingOverrides for why no shoal binary calls this yet.
func ApplyEmbeddingOverrides(
	job *tabletserver.TExternalCompactionJob,
	target, epoch string,
	states map[string]embeddingspace.FileState,
) error {
	if job == nil {
		return fmt.Errorf("compactjob: cannot apply embedding overrides to a nil job")
	}
	extra, err := EmbeddingOverrides(target, epoch, states)
	if err != nil {
		return err
	}
	if job.Overrides == nil {
		job.Overrides = make(map[string]string, len(extra))
	}
	// Clear first so a caller that drops a key actually drops it rather
	// than leaving a stale claim from a previous assignment behind.
	for _, key := range []string{
		embeddingspace.TableTargetProperty,
		embeddingspace.JobEpochProperty,
		embeddingspace.JobFileStatesProperty,
	} {
		delete(job.Overrides, key)
	}
	for key, value := range extra {
		job.Overrides[key] = value
	}
	return nil
}
