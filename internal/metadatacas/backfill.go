// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package metadatacas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/ingestclient"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// ErrRootBackfillUnsupported reports an attempt to backfill the root
// tablet's own file entries.
//
// The root tablet's metadata lives in ZooKeeper rather than in a
// metadata row, so it is mutated through a completely different path
// that is owned by whichever tablet server holds the root tablet. A
// backfill tool that reached in there would be writing behind that
// owner's back. The root tablet holds a handful of files describing the
// metadata table, not user data with vectors, so refusing is a real
// answer rather than a gap.
var ErrRootBackfillUnsupported = errors.New(
	"metadatacas: the root tablet's file entries cannot be backfilled from outside its owner")

// BackfillTarget names one existing metadata file entry.
type BackfillTarget struct {
	// TableID is the table whose tablet holds the entry.
	TableID string

	// PrevEndRow and EndRow bound the tablet. EndRow selects the
	// metadata row; PrevEndRow is a precondition, so a tablet that split
	// underneath the run is refused rather than written to.
	PrevEndRow []byte
	EndRow     []byte

	// FileQualifier is the exact file: column qualifier bytes.
	FileQualifier []byte

	// FileValue is the exact DataFileValue bytes currently stored. It is
	// a precondition, not a payload: if the entry has changed, the file
	// being labelled is not the file that was examined.
	FileValue []byte

	// ExistingEmbedding is the exact file.embedding column bytes
	// currently stored, or nil when the column is absent.
	//
	// It is the precondition on the thing being replaced. A nil value
	// conditions on the column not existing; a non-nil value conditions
	// on it still holding exactly these bytes. Either way a concurrent
	// writer that established a state first wins and this run reports a
	// race rather than overwriting it. Only a column that decodes to
	// unknown may be replaced — see WriteFileEmbedding.
	ExistingEmbedding []byte
}

// BackfillWriter writes explicit file.embedding columns for file entries
// that carry none.
//
// It deliberately does not claim tablet ownership. The embedding column
// is a pure annotation: it neither adds nor removes a file, and its only
// legal value is one the file's own footer already asserts. What it does
// require is that the thing being annotated is still there and still the
// same, which is what the conditions below establish:
//
//   - the tablet's ~tab:~pr is unchanged, so the row still describes the
//     tablet that was examined and has not split;
//   - the file: entry still holds exactly the bytes that were read, so
//     the file has not been replaced by a compaction; and
//   - the file.embedding column still holds exactly what it held when
//     the file was examined — absent, or an explicit unknown — so a
//     concurrent writer that established the state first wins and this
//     run reports a race instead of overwriting it.
//
// Together those make the write idempotent: a second run finds a
// definite column, skips the file before reaching here, and nothing is
// written.
type BackfillWriter struct {
	reader  TabletReader
	locator RootLocator
	writer  ConditionalWriter
	next    atomic.Int64
	routes  *metadataRouteCache
}

// metadataRouteCache memoizes the metadata table's tablet routing for
// the duration of a backfill pass.
//
// locateMetadataTarget re-derives routing from a LocateTable call, which
// is one root-tablet scan. The tablet authority pays that once per
// commit, but a backfill visits every file in a table, so the same code
// would issue a routing scan per file and make a large migration cost
// millions of avoidable metadata RPCs — enough that an operator would
// reasonably decline to run it.
//
// The cache is only ever an optimisation: it is dropped whenever a write
// fails or is rejected, so a split or a moved tablet is re-resolved on
// the next attempt, and the conditional write's own preconditions remain
// the thing that decides whether a mutation is safe to apply.
type metadataRouteCache struct {
	reader  TabletReader
	mu      sync.Mutex
	tablets []metadata.TabletInfo
}

func (c *metadataRouteCache) LocateTable(
	ctx context.Context, tableID string,
) ([]metadata.TabletInfo, error) {
	if tableID != metadata.MetadataTableID {
		return c.reader.LocateTable(ctx, tableID)
	}
	c.mu.Lock()
	cached := c.tablets
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	tablets, err := c.reader.LocateTable(ctx, tableID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.tablets = tablets
	c.mu.Unlock()
	return tablets, nil
}

func (c *metadataRouteCache) invalidate() {
	c.mu.Lock()
	c.tablets = nil
	c.mu.Unlock()
}

// NewBackfillWriter builds a writer over the same seams the tablet
// authority uses.
func NewBackfillWriter(
	reader TabletReader, locator RootLocator, writer ConditionalWriter,
) (*BackfillWriter, error) {
	if reader == nil || locator == nil || writer == nil {
		return nil, ErrInvalidConfig
	}
	routes := &metadataRouteCache{reader: reader}
	w := &BackfillWriter{reader: routes, locator: locator, writer: writer, routes: routes}
	w.next.Store(1)
	return w, nil
}

// WriteFileEmbedding records state for one file entry.
//
// It reports applied=false with a nil error when the conditional write
// was rejected — the entry changed, the tablet split, or the column was
// written by someone else. That is an expected outcome of running a
// migration against a live cluster, not a failure, and re-running the
// backfill resolves whatever replaced the entry.
func (w *BackfillWriter) WriteFileEmbedding(
	ctx context.Context, target BackfillTarget, state embeddingspace.FileState,
) (bool, error) {
	if w == nil {
		return false, ErrInvalidConfig
	}
	if target.TableID == metadata.RootTableID {
		return false, ErrRootBackfillUnsupported
	}
	if target.TableID == "" || len(target.FileQualifier) == 0 || len(target.FileValue) == 0 {
		return false, fmt.Errorf("%w: incomplete backfill target", ErrInvalidConfig)
	}
	if !state.Known() {
		// The backfill only ever writes what a footer positively
		// asserted. Recording unknown would replace an absent column
		// with an explicit non-claim, which is no more informative and
		// which a later integrity check would have to reconcile against
		// the footer that could not produce it.
		return false, fmt.Errorf("%w: refusing to record %s", ErrInvalidConfig, state.String())
	}
	if target.ExistingEmbedding != nil {
		// Nilness, not length: nil means the column is absent, so a
		// present-but-empty value is a malformed column, not a missing
		// one. Testing length would skip decoding it and let the write
		// replace it, when the contract here is that only a column
		// decoding to unknown may be replaced.
		//
		// Replacing a column is only ever an upgrade from a non-claim.
		// Anything that already decodes to a definite state was
		// established by a writer with better evidence than a migration
		// tool, and the file's own footer is not grounds to overrule it
		// — that disagreement is an integrity condition for an operator
		// to look at, not something to silently paper over.
		existing, err := embeddingspace.Decode(target.ExistingEmbedding)
		if err != nil {
			return false, fmt.Errorf("%w: existing file.embedding column: %w", ErrInvalidConfig, err)
		}
		if existing.Known() {
			return false, fmt.Errorf(
				"%w: refusing to replace an established %s column", ErrInvalidConfig, existing.String())
		}
	}
	encoded, err := encodeFileEmbedding(state)
	if err != nil {
		return false, err
	}

	row, err := metadata.EncodeTabletRow(target.TableID, target.EndRow)
	if err != nil {
		return false, err
	}
	address, tableID, extent, err := locateMetadataTarget(ctx, w.reader, w.locator, target.TableID, row)
	if err != nil {
		// A cached route that no longer contains the row surfaces here.
		// Drop it and re-resolve once before giving up, so a split that
		// happened during the pass costs one extra scan rather than
		// stalling the whole migration.
		w.routes.invalidate()
		address, tableID, extent, err = locateMetadataTarget(ctx, w.reader, w.locator, target.TableID, row)
		if err != nil {
			return false, err
		}
	}
	mutation, err := cclient.NewMutation(row)
	if err != nil {
		return false, err
	}
	mutation.PutLatest([]byte(metadata.CFFileEmbedding), target.FileQualifier, nil, encoded)
	wireMutation, err := mutation.ToThrift()
	if err != nil {
		return false, err
	}
	conditions := []*data.TCondition{
		{
			Cf:  []byte(metadata.CFTabletSection),
			Cq:  []byte(metadata.CQPrevRow),
			Cv:  []byte{},
			Val: metadata.EncodePrevEndRow(target.PrevEndRow),
		},
		{
			Cf:  []byte(metadata.CFFile),
			Cq:  append([]byte(nil), target.FileQualifier...),
			Cv:  []byte{},
			Val: append([]byte(nil), target.FileValue...),
		},
		{
			// A nil Val means "this column must be absent"; a non-nil
			// one means "it must still hold exactly these bytes".
			Cf:  []byte(metadata.CFFileEmbedding),
			Cq:  append([]byte(nil), target.FileQualifier...),
			Cv:  []byte{},
			Val: bytes.Clone(target.ExistingEmbedding),
		},
	}
	status, err := w.writer.ConditionalWrite(ctx, address, tableID, extent,
		&data.TConditionalMutation{
			Conditions: conditions, Mutation: wireMutation, ID: w.next.Add(1),
		})
	if err != nil {
		// The route the write was sent to may be why it failed, so it
		// is not trusted again.
		w.routes.invalidate()
		return false, err
	}
	switch status {
	case ingestclient.ConditionalAccepted:
		return true, nil
	case ingestclient.ConditionalRejected:
		// A rejection is usually a changed entry, but it is also what a
		// stale route produces. Re-resolving costs one scan and only on
		// the rare path, which is the right trade against a cache that
		// could otherwise keep sending every remaining file to a tablet
		// that moved.
		w.routes.invalidate()
		return false, nil
	default:
		// An unknown outcome must not be reported as a clean skip: the
		// write may have landed. Reporting it surfaces the file to the
		// operator, and the next run's absent-column condition settles
		// which way it went.
		w.routes.invalidate()
		return false, fmt.Errorf("%w: conditional write outcome is unknown", ErrRejected)
	}
}
