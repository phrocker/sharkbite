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
	"sort"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
)

var qualifierRetired = []byte("retired")

func (c *Client) ReserveIndexGeneration(
	ctx context.Context,
	family coordination.Family,
	reservationID []byte,
) (coordination.IGEN, error) {
	row, err := coordination.IndexGenerationHeadRow(c.domain, family)
	if err != nil {
		return nil, err
	}
	generation, err := c.reserveGeneration(ctx, row, reservationID)
	if err != nil {
		return nil, err
	}
	return append(coordination.IGEN(nil), copyGenerationIGEN(generation)...), nil
}

func (c *Client) CreateIndexGeneration(ctx context.Context, build IndexBuild) (IndexBuild, error) {
	build = cloneBuild(build)
	if build.Manifest.State != coordination.IndexGenerationBuilding {
		return IndexBuild{}, ErrConflict
	}
	if _, err := generationFromIGEN(build.Manifest.IGEN); err != nil {
		return IndexBuild{}, err
	}
	authority, err := c.currentAuthority(ctx)
	if err != nil {
		return IndexBuild{}, err
	}
	if err := requireAuthority(build.AuthorityGeneration, build.RetentionGeneration, authority); err != nil {
		return IndexBuild{}, err
	}
	if build.RecordGeneration == 0 {
		build.RecordGeneration = 1
	}
	if build.UpdatedAt.IsZero() {
		build.UpdatedAt = c.clock().UTC()
	}
	if existing, _, _, readErr := c.readBuild(ctx, build.Manifest.Family, build.Manifest.IGEN); readErr == nil {
		if sameBuildIdentity(existing, build) {
			return cloneBuild(existing), nil
		}
		return IndexBuild{}, ErrCorruption
	} else if !errors.Is(readErr, ErrNotFound) {
		return IndexBuild{}, readErr
	}
	value, err := marshalBuild(build)
	if err != nil {
		return IndexBuild{}, err
	}
	row, err := coordination.IndexGenerationRow(c.domain, build.Manifest.Family, build.Manifest.IGEN)
	if err != nil {
		return IndexBuild{}, err
	}
	if err := c.absentOrIdentical(
		ctx, c.coordinate(row, familyManifest, qualifierBuild), value, int64(build.RecordGeneration),
	); err != nil {
		return IndexBuild{}, err
	}
	return cloneBuild(build), nil
}

func (c *Client) readBuild(
	ctx context.Context,
	family coordination.Family,
	igen coordination.IGEN,
) (IndexBuild, []byte, int64, error) {
	row, err := coordination.IndexGenerationRow(c.domain, family, igen)
	if err != nil {
		return IndexBuild{}, nil, 0, err
	}
	cell, found, err := c.readOne(ctx, c.coordinate(row, familyManifest, qualifierBuild))
	if err != nil {
		return IndexBuild{}, nil, 0, err
	}
	if !found {
		return IndexBuild{}, nil, 0, ErrNotFound
	}
	build, err := unmarshalBuild(cell.Value)
	if err != nil || cell.Timestamp != int64(build.RecordGeneration) ||
		!bytes.Equal(build.Manifest.Family, family) || !bytes.Equal(build.Manifest.IGEN, igen) {
		return IndexBuild{}, nil, 0, ErrCorruption
	}
	return build, cell.Value, cell.Timestamp, nil
}

func (c *Client) AppendIndexDelta(ctx context.Context, delta coordination.IndexDeltaV1) error {
	if delta.State != coordination.LifecycleVerified {
		return ErrConflict
	}
	build, _, _, err := c.readBuild(ctx, delta.Family, delta.IGEN)
	if err != nil {
		return err
	}
	if build.Manifest.State != coordination.IndexGenerationBuilding {
		return ErrConflict
	}
	if delta.Epoch <= build.Manifest.SourceEpoch || delta.ManifestDigest != build.Manifest.ManifestDigest {
		return ErrConflict
	}
	authority, err := c.currentAuthority(ctx)
	if err != nil {
		return err
	}
	if err := requireAuthority(build.AuthorityGeneration, build.RetentionGeneration, authority); err != nil {
		return err
	}
	if delta.Epoch < authority.HistoryFloor {
		return ErrStaleRetention
	}
	if err := c.indexVerifier.VerifyDelta(ctx, c.domain, delta, authority); err != nil {
		return classifyVerifier(err)
	}
	value, err := coordination.MarshalIndexDeltaV1(delta)
	if err != nil {
		return err
	}
	row, err := coordination.IndexDeltaRow(c.domain, delta.Family, delta.IGEN, delta.Epoch, delta.TXN)
	if err != nil {
		return err
	}
	return c.absentOrIdentical(ctx, c.coordinate(row, familyDelta, qualifierDelta), value, int64(delta.Epoch))
}

func (c *Client) scanIndexDeltas(
	ctx context.Context,
	family coordination.Family,
	igen coordination.IGEN,
) ([]coordination.IndexDeltaV1, error) {
	prefix, err := coordination.IndexDeltaPrefix(c.domain, family, igen)
	if err != nil {
		return nil, err
	}
	cells, err := c.store.ScanPrefix(ctx, prefix, familyDelta, qualifierDelta, c.visibility, c.maxScan)
	if err != nil {
		return nil, classifyUnavailable(err)
	}
	if len(cells) == c.maxScan {
		return nil, ErrBounds
	}
	sort.Slice(cells, func(i, j int) bool { return bytes.Compare(cells[i].Coordinate.Row, cells[j].Coordinate.Row) < 0 })
	result := make([]coordination.IndexDeltaV1, 0, len(cells))
	var previous *coordination.IndexDeltaKey
	for _, cell := range cells {
		key, parseErr := coordination.ParseIndexDeltaRow(cell.Coordinate.Row)
		if parseErr != nil || !bytes.Equal(key.Domain, c.domain) ||
			!bytes.Equal(key.Family, family) || !bytes.Equal(key.IGEN, igen) {
			return nil, ErrCorruption
		}
		value, decodeErr := coordination.UnmarshalIndexDeltaV1(cell.Value)
		if decodeErr != nil || value.Epoch != key.Epoch || !bytes.Equal(value.TXN, key.TXN) ||
			cell.Timestamp != int64(value.Epoch) {
			return nil, ErrCorruption
		}
		if previous != nil && previous.Epoch == key.Epoch && bytes.Equal(previous.TXN, key.TXN) {
			return nil, ErrCorruption
		}
		copy := key
		previous = &copy
		result = append(result, value)
	}
	return result, nil
}

func (c *Client) SealIndexGeneration(
	ctx context.Context,
	owner coordination.OwnerID,
	operationID []byte,
	fence coordination.Fence,
	sealed coordination.IndexGenerationV2,
) (IndexBuild, error) {
	if sealed.State != coordination.IndexGenerationSealed {
		return IndexBuild{}, ErrConflict
	}
	build, before, timestamp, err := c.readBuild(ctx, sealed.Family, sealed.IGEN)
	if err != nil {
		return IndexBuild{}, err
	}
	if !bytes.Equal(build.Owner, owner) || !bytes.Equal(build.OperationID, operationID) || build.Fence != fence {
		return IndexBuild{}, ErrStaleOwner
	}
	if build.Manifest.State == coordination.IndexGenerationSealed {
		if build.Manifest.ManifestDigest == sealed.ManifestDigest {
			return cloneBuild(build), nil
		}
		return IndexBuild{}, ErrCorruption
	}
	authority, err := c.currentAuthority(ctx)
	if err != nil {
		return IndexBuild{}, err
	}
	if err := requireAuthority(build.AuthorityGeneration, build.RetentionGeneration, authority); err != nil {
		return IndexBuild{}, err
	}
	if sealed.BuildThrough < build.Manifest.SourceEpoch || sealed.DeltaThrough < sealed.BuildThrough {
		return IndexBuild{}, ErrConflict
	}
	deltas, err := c.scanIndexDeltas(ctx, sealed.Family, sealed.IGEN)
	if err != nil {
		return IndexBuild{}, err
	}
	filtered := make([]coordination.IndexDeltaV1, 0, len(deltas))
	seen := make(map[string]struct{}, len(deltas))
	var digestPartsInput [][]byte
	digestPartsInput = append(digestPartsInput, []byte("index-delta-set-v1"))
	for _, delta := range deltas {
		if delta.Epoch <= build.Manifest.SourceEpoch || delta.Epoch > sealed.DeltaThrough {
			return IndexBuild{}, ErrCorruption
		}
		if delta.ManifestDigest != build.Manifest.ManifestDigest || delta.State != coordination.LifecycleVerified {
			return IndexBuild{}, ErrCorruption
		}
		if err := c.indexVerifier.VerifyDelta(ctx, c.domain, delta, authority); err != nil {
			return IndexBuild{}, classifyVerifier(err)
		}
		if delta.Epoch <= sealed.BuildThrough {
			continue
		}
		key := outcomeKey(delta.Epoch, delta.TXN)
		if _, exists := seen[key]; exists {
			return IndexBuild{}, ErrCorruption
		}
		seen[key] = struct{}{}
		encoded, _ := coordination.MarshalIndexDeltaV1(delta)
		digestPartsInput = append(digestPartsInput, encoded)
		filtered = append(filtered, delta)
	}
	outcomes, err := c.indexVerifier.CommittedOutcomes(
		ctx, c.domain, sealed.BuildThrough, sealed.DeltaThrough, c.maxScan,
	)
	if err != nil {
		return IndexBuild{}, classifyVerifier(err)
	}
	if len(outcomes) >= c.maxScan {
		return IndexBuild{}, ErrBounds
	}
	outcomeSeen := make(map[string]struct{}, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.Epoch <= sealed.BuildThrough || outcome.Epoch > sealed.DeltaThrough ||
			outcome.Epoch.Validate() != nil || outcome.TXN.Validate() != nil {
			return IndexBuild{}, ErrCorruption
		}
		key := outcomeKey(outcome.Epoch, outcome.TXN)
		if _, duplicate := outcomeSeen[key]; duplicate {
			return IndexBuild{}, ErrCorruption
		}
		outcomeSeen[key] = struct{}{}
		if _, present := seen[key]; !present {
			return IndexBuild{}, ErrCorruption
		}
	}
	if len(outcomeSeen) != len(seen) {
		return IndexBuild{}, ErrCorruption
	}
	computedDeltaDigest := digestParts(digestPartsInput...)
	if sealed.DeltaDigest != computedDeltaDigest {
		return IndexBuild{}, ErrConflict
	}
	if err := c.indexVerifier.VerifyBase(ctx, c.domain, sealed, sealed.BuildThrough); err != nil {
		return IndexBuild{}, classifyVerifier(err)
	}
	if err := c.indexVerifier.VerifySealing(ctx, c.domain, sealed, filtered); err != nil {
		return IndexBuild{}, classifyVerifier(err)
	}
	if err := coordination.ValidateIndexGenerationTransition(build.Manifest, sealed); err != nil {
		return IndexBuild{}, ErrConflict
	}
	build.Manifest = sealed
	build.RecordGeneration++
	build.UpdatedAt = c.clock().UTC()
	after, err := marshalBuild(build)
	if err != nil {
		return IndexBuild{}, err
	}
	row, _ := coordination.IndexGenerationRow(c.domain, sealed.Family, sealed.IGEN)
	if err := c.transition(
		ctx, c.coordinate(row, familyManifest, qualifierBuild), before, timestamp, after, int64(build.RecordGeneration),
	); err != nil {
		return IndexBuild{}, err
	}
	return cloneBuild(build), nil
}

func (c *Client) PublishIndexActivation(
	ctx context.Context,
	activation coordination.IndexActivationV2,
) error {
	if activation.State != coordination.LifecycleActive {
		return ErrConflict
	}
	build, _, _, err := c.readBuild(ctx, activation.Family, activation.IGEN)
	if err != nil {
		return err
	}
	if build.Manifest.State != coordination.IndexGenerationSealed ||
		build.Manifest.ManifestDigest != activation.ManifestDigest ||
		build.Manifest.DeltaDigest != activation.DeltaDigest {
		return ErrCorruption
	}
	authority, err := c.currentAuthority(ctx)
	if err != nil {
		return err
	}
	if err := requireAuthority(activation.AuthorityGeneration, build.RetentionGeneration, authority); err != nil {
		return err
	}
	if activation.Fence != build.Fence {
		return ErrStaleOwner
	}
	if err := c.indexVerifier.VerifyActivation(ctx, c.domain, activation, build.Manifest); err != nil {
		return classifyVerifier(err)
	}
	value, err := coordination.MarshalIndexActivationV2(activation)
	if err != nil {
		return err
	}
	row, err := coordination.IndexActivationRow(c.domain, activation.Family, activation.ActivationEpoch, activation.IGEN)
	if err != nil {
		return err
	}
	return c.absentOrIdentical(
		ctx, c.coordinate(row, familyActivation, qualifierActive), value, int64(activation.ActivationEpoch),
	)
}

func (c *Client) LookupIndexGeneration(
	ctx context.Context,
	family coordination.Family,
	snapshot coordination.Epoch,
) (IndexPin, error) {
	if err := snapshot.Validate(); err != nil {
		return IndexPin{}, err
	}
	prefix, err := coordination.IndexActivationPrefix(c.domain, family)
	if err != nil {
		return IndexPin{}, err
	}
	start, err := coordination.IndexActivationSeek(c.domain, family, snapshot)
	if err != nil {
		return IndexPin{}, err
	}
	cells, err := c.store.ScanPrefixFrom(
		ctx, prefix, start, familyActivation, qualifierActive, c.visibility, c.maxScan,
	)
	if err != nil {
		return IndexPin{}, classifyUnavailable(err)
	}
	if len(cells) == c.maxScan {
		return IndexPin{}, ErrBounds
	}
	sort.Slice(cells, func(i, j int) bool { return bytes.Compare(cells[i].Coordinate.Row, cells[j].Coordinate.Row) < 0 })
	var candidate *coordination.IndexActivationV2
	var observedEpoch coordination.Epoch
	for _, cell := range cells {
		key, parseErr := coordination.ParseIndexActivationRow(cell.Coordinate.Row)
		if parseErr != nil || !bytes.Equal(key.Domain, c.domain) || !bytes.Equal(key.Family, family) {
			return IndexPin{}, ErrCorruption
		}
		if key.ActivationEpoch > snapshot {
			return IndexPin{}, ErrCorruption
		}
		if observedEpoch == 0 {
			observedEpoch = key.ActivationEpoch
		} else if key.ActivationEpoch == observedEpoch {
			return IndexPin{}, ErrCorruption
		} else if candidate != nil {
			break
		}
		activation, decodeErr := coordination.UnmarshalIndexActivationV2(cell.Value)
		if decodeErr != nil || activation.ActivationEpoch != key.ActivationEpoch ||
			!bytes.Equal(activation.Family, family) || cell.Timestamp != int64(activation.ActivationEpoch) {
			return IndexPin{}, ErrCorruption
		}
		if activation.State != coordination.LifecycleActive {
			continue
		}
		if candidate == nil {
			copy := activation
			candidate = &copy
			continue
		}
		break
	}
	if candidate != nil {
		activation := *candidate
		build, _, _, readErr := c.readBuild(ctx, family, activation.IGEN)
		if readErr != nil {
			return IndexPin{}, readErr
		}
		if build.Manifest.State != coordination.IndexGenerationSealed ||
			build.Manifest.ManifestDigest != activation.ManifestDigest ||
			build.Manifest.DeltaDigest != activation.DeltaDigest {
			return IndexPin{}, ErrCorruption
		}
		retired, retireErr := c.indexRetired(ctx, family, activation.IGEN)
		if retireErr != nil {
			return IndexPin{}, retireErr
		}
		if retired {
			return IndexPin{}, ErrNotFound
		}
		authority, authorityErr := c.currentAuthority(ctx)
		if authorityErr != nil {
			return IndexPin{}, authorityErr
		}
		if err := requireAuthority(build.AuthorityGeneration, build.RetentionGeneration, authority); err != nil {
			return IndexPin{}, err
		}
		if snapshot < authority.HistoryFloor {
			return IndexPin{}, ErrStaleRetention
		}
		if err := c.indexVerifier.VerifyLookup(ctx, c.domain, activation, build.Manifest); err != nil {
			return IndexPin{}, classifyVerifier(err)
		}
		activationBytes, _ := coordination.MarshalIndexActivationV2(activation)
		manifestBytes, _ := coordination.MarshalIndexGenerationV2(build.Manifest)
		return IndexPin{
			Activation: cloneActivation(activation), Manifest: cloneGeneration(build.Manifest),
			PinDigest: digestParts([]byte("index-pin-v1"), activationBytes, manifestBytes),
		}, nil
	}
	return IndexPin{}, ErrNotFound
}

func (c *Client) RetireIndexGeneration(
	ctx context.Context,
	family coordination.Family,
	igen coordination.IGEN,
) error {
	build, _, _, err := c.readBuild(ctx, family, igen)
	if err != nil {
		return err
	}
	if build.Manifest.State != coordination.IndexGenerationSealed {
		return ErrConflict
	}
	selected, err := c.leases.SelectsIndexGeneration(ctx, c.domain, family, igen)
	if err != nil {
		return classifyUnavailable(err)
	}
	if selected {
		return ErrLeaseActive
	}
	authority, err := c.currentAuthority(ctx)
	if err != nil {
		return err
	}
	if err := requireAuthority(build.AuthorityGeneration, build.RetentionGeneration, authority); err != nil {
		return err
	}
	if err := c.indexVerifier.AllowIndexRetirement(ctx, c.domain, build.Manifest); err != nil {
		return classifyVerifier(err)
	}
	row, _ := coordination.IndexGenerationRow(c.domain, family, igen)
	value := digestParts([]byte("index-retired-v1"), build.Manifest.ManifestDigest[:])
	return c.absentOrIdentical(
		ctx, c.coordinate(row, familyManifest, qualifierRetired), value[:], int64(build.RecordGeneration),
	)
}

func (c *Client) indexRetired(
	ctx context.Context,
	family coordination.Family,
	igen coordination.IGEN,
) (bool, error) {
	row, err := coordination.IndexGenerationRow(c.domain, family, igen)
	if err != nil {
		return false, err
	}
	_, found, err := c.readOne(ctx, c.coordinate(row, familyManifest, qualifierRetired))
	return found, err
}

func outcomeKey(epoch coordination.Epoch, txn coordination.TXN) string {
	return string(coordination.U64(uint64(epoch))) + string(txn)
}

func cloneBuild(value IndexBuild) IndexBuild {
	value.Manifest = cloneGeneration(value.Manifest)
	value.Owner = append(coordination.OwnerID(nil), value.Owner...)
	value.OperationID = append([]byte(nil), value.OperationID...)
	return value
}

func sameBuildIdentity(a, b IndexBuild) bool {
	left, leftErr := coordination.MarshalIndexGenerationV2(a.Manifest)
	right, rightErr := coordination.MarshalIndexGenerationV2(b.Manifest)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right) &&
		bytes.Equal(a.Owner, b.Owner) && bytes.Equal(a.OperationID, b.OperationID) &&
		a.Fence == b.Fence && a.AuthorityGeneration == b.AuthorityGeneration &&
		a.RetentionGeneration == b.RetentionGeneration
}

func cloneGeneration(value coordination.IndexGenerationV2) coordination.IndexGenerationV2 {
	value.Family = append(coordination.Family(nil), value.Family...)
	value.IGEN = append(coordination.IGEN(nil), value.IGEN...)
	value.Schema = append([]byte(nil), value.Schema...)
	return value
}

func cloneActivation(value coordination.IndexActivationV2) coordination.IndexActivationV2 {
	value.Family = append(coordination.Family(nil), value.Family...)
	value.IGEN = append(coordination.IGEN(nil), value.IGEN...)
	value.TXN = append(coordination.TXN(nil), value.TXN...)
	return value
}
