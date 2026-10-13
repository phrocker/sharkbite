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
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package authorized

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Application edge IDs live in a namespace of their writer (#699).
//
// Edge IDs are one space shared by every edge in the corpus: intrinsic
// "contains" edges (derived from a document's URI), extraction and
// derivation edges, and every principal's application edges. If Connect
// accepted any caller-chosen ID, whether it succeeded would depend on
// whether some edge the caller cannot read already holds that ID, and,
// through intrinsic IDs, on whether a hidden document exists at a guessed
// URI. Refusing only known server-derived prefixes would still leave other
// principals' hidden application edges as an oracle.
//
// So Connect accepts only IDs in the caller's own namespace: the prefix
// applicationEdgeIDPrefix, a digest of the caller's identity, then "/" and
// the caller's local ID. No server derivation produces that prefix, and
// another identity's namespace differs by a 128-bit digest, so the only
// registrations an accepted ID can collide with were connected under the
// same identity. Any other ID is refused with the same InvalidArgument
// whatever the catalog holds: the refusal depends on the caller's identity
// and the ID only.
//
// The namespace binds the identity, not its current authority: a narrowed
// session of the same identity (workspace narrowing, a down-scoped token, a
// revoked label) shares it, and can still tell whether a local ID that
// identity's broader sessions connected exists. Connect has no HTTP or MCP
// route, so this is reachable by library callers only.
const applicationEdgeIDPrefix = "app-edge_"

// applicationEdgeNamespaceVersion separates the digest from any other use
// of the same components.
const applicationEdgeNamespaceVersion = "shoal/authorized/application-edge-namespace/v1"

// applicationEdgeNamespace returns the ID prefix of every application edge
// the decision's identity may connect. It covers every component that
// identifies who is writing: the authorization domain, the subject, the
// acting application, the client, the delegation chain and the service
// identity. It leaves out what changes per request (request and correlation
// IDs) and what changes per grant (policies, generation), so a writer's
// retry lands in the same namespace.
func applicationEdgeNamespace(decision auth.Decision) string {
	digest := sha256.New()
	var length [8]byte
	write := func(value []byte) {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write(value)
	}
	write([]byte(applicationEdgeNamespaceVersion))
	write(decision.AuthorizationDomain())
	write([]byte(decision.Subject()))
	write([]byte(decision.Actor()))
	write([]byte(decision.ClientID()))
	onBehalfOf := decision.OnBehalfOf()
	// Explicit framing. Every component is length-delimited and all but the
	// chain have a fixed count, so the chain's length is already implied;
	// the count keeps the encoding self-describing if that ever changes.
	binary.BigEndian.PutUint64(length[:], uint64(len(onBehalfOf)))
	_, _ = digest.Write(length[:])
	for _, principal := range onBehalfOf {
		write([]byte(principal))
	}
	write([]byte(decision.ServiceRole()))
	write([]byte(decision.ServiceCeilingIdentity()))
	return applicationEdgeIDPrefix +
		hex.EncodeToString(digest.Sum(nil)[:16]) + "/"
}

// applicationEdgeID places localID in the decision's namespace.
func applicationEdgeID(decision auth.Decision, localID shoal.ID) (shoal.ID, error) {
	if err := shoal.ValidateRequiredID("local graph edge ID", localID); err != nil {
		return "", err
	}
	id := shoal.ID(applicationEdgeNamespace(decision) + string(localID))
	if err := shoal.ValidateRequiredID("graph edge ID", id); err != nil {
		return "", err
	}
	return id, nil
}

// requireApplicationEdgeNamespace refuses an edge ID outside the caller's
// namespace. The refusal is the same whatever the catalog or corpus holds.
func requireApplicationEdgeNamespace(decision auth.Decision, edgeID shoal.ID) error {
	namespace := applicationEdgeNamespace(decision)
	if len(edgeID) > len(namespace) &&
		strings.HasPrefix(string(edgeID), namespace) {
		return nil
	}
	return shoal.NewError(shoal.ErrorInvalidArgument,
		"graph edge ID is outside the caller's application edge namespace; "+
			"derive it with ApplicationEdgeID")
}

// ApplicationEdgeLocalID returns the local ID of an application edge ID in
// the decision's namespace, and false for any other ID. A trusted
// EdgePolicySelector that selects by edge ID can use it to key on the ID its
// caller chose.
func ApplicationEdgeLocalID(decision auth.Decision, edgeID shoal.ID) (shoal.ID, bool) {
	if requireApplicationEdgeNamespace(decision, edgeID) != nil {
		return "", false
	}
	return edgeID[len(applicationEdgeNamespace(decision)):], true
}

// ApplicationEdgeID returns the edge ID under which the authenticated caller
// connects an application edge it calls localID. Connect accepts no other
// edge IDs (#699). The result depends only on the caller's identity and
// localID, is the same on every call and across restarts, and is the ID
// reads report for the edge. Two different identities never share an ID, so
// one identity's edges never collide with another's; sessions of the same
// identity share its namespace whatever their grants.
func (c *Client) ApplicationEdgeID(
	ctx context.Context,
	localID shoal.ID,
) (shoal.ID, error) {
	if err := shoal.ValidateRequiredID("local graph edge ID", localID); err != nil {
		return "", err
	}
	decision, guard, _, err := c.begin(ctx, auth.OperationConnect)
	if err != nil {
		return "", err
	}
	id, err := applicationEdgeID(decision, localID)
	if err != nil {
		return "", err
	}
	if err := guard.Check(ctx); err != nil {
		return "", err
	}
	return id, nil
}
