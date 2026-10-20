// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
)

// RecoveryClaim identifies one live prepared claim. Its fence, claim ID and
// authenticated caller must all still match when the server answers.
type RecoveryClaim struct {
	Context    Context
	ClaimID    []byte
	ClaimFence uint64
}

// Material is executable input bound to a live claim, but not a send grant.
type Material struct {
	Input       json.RawMessage
	ExecutorKey []byte
}

func (c *Client) ClaimMaterial(ctx context.Context, actionID []byte, request RecoveryClaim) (Material, error) {
	const op = "material"
	if err := checkActionID(op, actionID); err != nil {
		return Material{}, err
	}
	if len(request.ClaimID) == 0 || len(request.ClaimID) > MaxActionIDBytes || request.ClaimFence == 0 {
		return Material{}, refused(op, "claim identity and fence are required")
	}
	correlation, err := correlationHeader(op, request.Context.CorrelationID)
	if err != nil {
		return Material{}, err
	}
	contextValue, err := request.Context.wire(op, c.clock())
	if err != nil {
		return Material{}, err
	}
	var response ClaimMaterial
	if _, _, err := c.post(ctx, op, ActionRoute(actionID, VerbMaterial), correlation,
		RecoveryClaimRequest{Context: contextValue, ClaimID: EncodeID(request.ClaimID), ClaimFence: request.ClaimFence}, &response); err != nil {
		return Material{}, err
	}
	id, err := DecodeID(response.ID)
	if err != nil || !bytes.Equal(id, actionID) {
		return Material{}, protocolError(op, "material response names a different action")
	}
	claimID, err := DecodeID(response.ClaimID)
	if err != nil || !bytes.Equal(claimID, request.ClaimID) || response.ClaimFence != request.ClaimFence {
		return Material{}, protocolError(op, "material response names a different claim")
	}
	key, err := ParseExecutorKey(response.ExecutorKey)
	if err != nil || len(response.Input) == 0 {
		return Material{}, protocolError(op, "material response lacks executable fields")
	}
	return Material{Input: append(json.RawMessage(nil), response.Input...), ExecutorKey: key}, nil
}

// RecoveryPrepare is the exact request and account-scope commitment submitted
// before a trusted gateway sends to its target.
type RecoveryPrepare struct {
	RecoveryClaim
	Mode              string
	RequestDigest     [32]byte
	TargetScopeDigest [32]byte
}

// Prepare confirms a durable commitment only after reauthorizing the live
// claimant. A lost or malformed answer grants nothing; the same request may
// be retried while the lease remains live.
func (c *Client) Prepare(ctx context.Context, actionID []byte, request RecoveryPrepare) (uint64, error) {
	const op = "prepare"
	if err := checkActionID(op, actionID); err != nil {
		return 0, err
	}
	if len(request.ClaimID) == 0 || len(request.ClaimID) > MaxActionIDBytes || request.ClaimFence == 0 ||
		request.RequestDigest == ([32]byte{}) || request.TargetScopeDigest == ([32]byte{}) {
		return 0, refused(op, "claim identity, fence and digests are required")
	}
	switch request.Mode {
	case "key", "natural", "unprotected":
	default:
		return 0, refused(op, "recovery mode is invalid")
	}
	correlation, err := correlationHeader(op, request.Context.CorrelationID)
	if err != nil {
		return 0, err
	}
	contextValue, err := request.Context.wire(op, c.clock())
	if err != nil {
		return 0, err
	}
	var response PrepareReceipt
	if _, _, err := c.post(ctx, op, ActionRoute(actionID, VerbPrepare), correlation, PrepareRequest{
		Context: contextValue, ClaimID: EncodeID(request.ClaimID), ClaimFence: request.ClaimFence,
		Mode: request.Mode, RequestDigest: base64.RawURLEncoding.EncodeToString(request.RequestDigest[:]),
		TargetScopeDigest: base64.RawURLEncoding.EncodeToString(request.TargetScopeDigest[:]),
	}, &response); err != nil {
		return 0, err
	}
	id, err := DecodeID(response.ID)
	if err != nil || !bytes.Equal(id, actionID) {
		return 0, protocolError(op, "prepare receipt names a different action")
	}
	claimID, err := DecodeID(response.ClaimID)
	if err != nil || !bytes.Equal(claimID, request.ClaimID) ||
		response.ClaimFence != request.ClaimFence || !response.Committed || response.Version == 0 {
		return 0, protocolError(op, "prepare receipt does not confirm the current claim")
	}
	requestDigest, requestError := base64.RawURLEncoding.Strict().DecodeString(response.RequestDigest)
	scopeDigest, scopeError := base64.RawURLEncoding.Strict().DecodeString(response.TargetScopeDigest)
	if response.Mode != request.Mode || requestError != nil || scopeError != nil ||
		len(requestDigest) != 32 || len(scopeDigest) != 32 ||
		subtle.ConstantTimeCompare(requestDigest, request.RequestDigest[:]) != 1 ||
		subtle.ConstantTimeCompare(scopeDigest, request.TargetScopeDigest[:]) != 1 {
		return 0, protocolError(op, "prepare receipt confirms a different bound request")
	}
	return response.Version, nil
}
