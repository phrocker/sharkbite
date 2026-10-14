// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// registryDigestVersions lets a property every version must hold (a changed
// field changes the digest) be checked against each.
var registryDigestVersions = []struct {
	name   string
	digest func(Mutation) [sha256.Size]byte
}{
	{"v1", registryMutationDigestV1},
	{"v2", registryMutationDigest},
}

// Every v1 value in this file was computed by registryMutationDigest on
// origin/main at 6cf88b0c, before v2 existed, by a throwaway test run over the
// same mutation. They are statements of the old encoding, independent of the
// legacy reader under test; recomputing them here would let a reader that
// drifted from the old bytes, or a version switch wired backwards, still pass.
const (
	// approvalDigestMutation(true); approvalDigestMutation(false) is pinned
	// in approval_test.go.
	goldenV1ApprovalHeld = "6511742dce638575a692d99ecc50cabe" +
		"b917943271a7a76c928f794c23232f9d"
	// Both halves of v1CollidingActions, which old builds hashed identically.
	goldenV1ActionCollision = "189f93b7362a1f0315bbab22e6b7323a" +
		"89a81542bc7c0e8fef0e2bb115b2f507"
	// Both halves of v1CollidingScopes.
	goldenV1ScopeCollision = "c9ff15c954263230c2071f03784a049a" +
		"e77ba78743a46c83a10185f479b39868"
)

// v1CollidingActions returns two canonical descriptors that differ in what
// they require and declare but whose v1 byte streams are identical (#521).
//
// v1 appended the approval tag only when set, and the effects field only when
// non-empty, with no count ahead of a capability's actions. So
//
//	a: [a {approval}], [external {}]  ->  a, I, O, TAG, external, I, O
//	b: [a {}], [TAG {effects: external}] ->  a, I, O, TAG, external, I, O
//
// where TAG is "shoal.fleet.requires-approval.v1", a legal action name, and
// "external" is both a legal action name and EffectMutatesExternal's wire
// value. The first requires approval for "a"; the second does not, and
// instead declares an external effect on an action the first does not have.
func v1CollidingActions() (Mutation, Mutation) {
	schema := json.RawMessage(`{"type":"object"}`)
	mutation := func(actions []Action) Mutation {
		return Mutation{RegistrationKey: "key", Descriptor: Descriptor{
			ID: "agent", Generation: 1, Subject: "owner", Actor: "operator",
			AuthorizationDomain: []byte("domain"),
			Scopes: []Scope{{
				SourceID: []byte("s"), PolicyID: []byte("p"),
			}},
			ExecutorRef:    "local",
			Capabilities:   []Capability{{Name: "ops", Actions: actions}},
			LeaseExpiresAt: time.Unix(1_800_000_000, 0).UTC(),
		}}
	}
	return mutation([]Action{
			{Name: "a", InputSchema: schema, OutputSchema: schema,
				RequiresApproval: true},
			{Name: "external", InputSchema: schema, OutputSchema: schema},
		}), mutation([]Action{
			{Name: "a", InputSchema: schema, OutputSchema: schema},
			{Name: "shoal.fleet.requires-approval.v1",
				Effects:     Effects{EffectMutatesExternal},
				InputSchema: schema, OutputSchema: schema},
		})
}

// v1CollidingScopes moves fields across the scopes, executor and capability
// boundaries:
//
//	a: scopes [(X,Y),(P,Q),(E,S)], executor S, no capabilities
//	b: scopes [(X,Y)], executor P, capability Q, action E, schemas S, S
//
// Both write X, Y, P, Q, E, S, S. The first is not a registrable descriptor
// (it has no capabilities); it shows that the v1 function itself is not
// injective at every list boundary, not only where per-action fields are
// optional.
func v1CollidingScopes() (Mutation, Mutation) {
	schema := json.RawMessage(`{"type":"object"}`)
	descriptor := Descriptor{
		ID: "agent", Generation: 1, Subject: "owner", Actor: "operator",
		AuthorizationDomain: []byte("domain"),
		LeaseExpiresAt:      time.Unix(1_800_000_000, 0).UTC(),
	}
	first, second := descriptor, descriptor
	first.Scopes = []Scope{
		{SourceID: []byte("X"), PolicyID: []byte("Y")},
		{SourceID: []byte("P"), PolicyID: []byte("Q")},
		{SourceID: []byte("E"), PolicyID: schema},
	}
	first.ExecutorRef = string(schema)
	second.Scopes = []Scope{{SourceID: []byte("X"), PolicyID: []byte("Y")}}
	second.ExecutorRef = "P"
	second.Capabilities = []Capability{{Name: "Q", Actions: []Action{{
		Name: "E", InputSchema: schema, OutputSchema: schema,
	}}}}
	return Mutation{RegistrationKey: "key", Descriptor: first},
		Mutation{RegistrationKey: "key", Descriptor: second}
}

// TestRegistryDigestV1CollidesAcrossAListBoundary is the collision #521 said
// had not been constructed. Two registrable descriptors, one requiring
// approval and one declaring an external effect instead, had the same v1
// digest; the literal was computed by the old build over each. v2 separates
// them.
func TestRegistryDigestV1CollidesAcrossAListBoundary(t *testing.T) {
	first, second := v1CollidingActions()
	if reflect.DeepEqual(first, second) {
		t.Fatal("the colliding descriptors are the same descriptor")
	}
	// Both are what a registration would store: canonicalisation accepts
	// them and leaves them as they are.
	for _, mutation := range []Mutation{first, second} {
		canonical, err := canonicalCapabilities(mutation.Descriptor.Capabilities)
		if err != nil {
			t.Fatalf("colliding descriptor is not registrable: %v", err)
		}
		if !reflect.DeepEqual(canonical, mutation.Descriptor.Capabilities) {
			t.Fatalf("colliding descriptor is not canonical: %#v", canonical)
		}
	}
	for _, mutation := range []Mutation{first, second} {
		digest := registryMutationDigestV1(mutation)
		if got := hex.EncodeToString(digest[:]); got != goldenV1ActionCollision {
			t.Fatalf("v1 digest = %s, want the old build's %s",
				got, goldenV1ActionCollision)
		}
	}
	if registryMutationDigest(first) == registryMutationDigest(second) {
		t.Fatal("v2 digests of the v1-colliding descriptors are equal")
	}
}

func TestRegistryDigestV1CollidesAcrossTheScopeBoundary(t *testing.T) {
	first, second := v1CollidingScopes()
	for _, mutation := range []Mutation{first, second} {
		digest := registryMutationDigestV1(mutation)
		if got := hex.EncodeToString(digest[:]); got != goldenV1ScopeCollision {
			t.Fatalf("v1 digest = %s, want the old build's %s",
				got, goldenV1ScopeCollision)
		}
	}
	if registryMutationDigest(first) == registryMutationDigest(second) {
		t.Fatal("v2 digests of the v1-colliding descriptors are equal")
	}
}

// TestRegistryDigestVersionsDiffer checks that the legacy reader reproduces
// the old build's literals and that v2 never yields the v1 value for the same
// mutation, so a v1 digest can never be mistaken for a v2 one.
func TestRegistryDigestVersionsDiffer(t *testing.T) {
	first, second := v1CollidingActions()
	scopeFirst, scopeSecond := v1CollidingScopes()
	for _, fixture := range []struct {
		name     string
		mutation Mutation
		v1       string
	}{
		{"plain", approvalDigestMutation(false),
			"54af49ed8009c6c7c8d4862c19d5875176dfa8dbe51dd5188521a0c8b672138a"},
		{"approval", approvalDigestMutation(true), goldenV1ApprovalHeld},
		{"action collision a", first, goldenV1ActionCollision},
		{"action collision b", second, goldenV1ActionCollision},
		{"scope collision a", scopeFirst, goldenV1ScopeCollision},
		{"scope collision b", scopeSecond, goldenV1ScopeCollision},
	} {
		v1 := registryMutationDigestV1(fixture.mutation)
		if got := hex.EncodeToString(v1[:]); got != fixture.v1 {
			t.Fatalf("%s: v1 = %s, want the old build's %s",
				fixture.name, got, fixture.v1)
		}
		v2 := registryMutationDigest(fixture.mutation)
		if v2 == v1 {
			t.Fatalf("%s: v1 and v2 digests are equal", fixture.name)
		}
		digests := registryMutationDigests(fixture.mutation)
		if digests.current != v2 || digests.v1 != v1 {
			t.Fatalf("%s: receipt digests are not (v2, v1): %#v",
				fixture.name, digests)
		}
	}
}

// recordingHash captures the bytes an encoding writes.
type recordingHash struct{ bytes.Buffer }

func (*recordingHash) Sum(b []byte) []byte { return b }
func (*recordingHash) Size() int           { return 0 }
func (*recordingHash) BlockSize() int      { return 1 }

func layoutField(value []byte) []byte {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	return append(size[:], value...)
}

func layoutUint(value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return layoutField(encoded[:])
}

// TestRegistryDigestV2Layout pins the v2 encoding field by field, written out
// independently of the encoder: every list is preceded by its count, every
// flag is a one-byte field whether set or not, and an effect set is a counted
// list. Dropping a count or a flag, or reordering fields, fails here.
//
// A v2 digest is stored in receipts from the moment this ships, so this
// layout, and the golden below, can never change. A new encoding is v3.
func TestRegistryDigestV2Layout(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	lease := time.Unix(1_800_000_000, 0).UTC()
	revoked := time.Unix(1_800_000_100, 0).UTC()
	mutation := Mutation{
		RegistrationKey: "key", ExpectedGeneration: 3,
		Descriptor: Descriptor{
			ID: "agent", Generation: 4, Subject: "owner", Actor: "operator",
			ParentID: "parent", AuthorizationDomain: []byte("domain"),
			Scopes: []Scope{
				{SourceID: []byte("s1"), PolicyID: []byte("p1")},
				{SourceID: []byte("s2"), PolicyID: []byte("p2")},
			},
			ExecutorRef: "local",
			Capabilities: []Capability{
				{Name: "ops", Actions: []Action{
					{Name: "deploy", InputSchema: schema, OutputSchema: schema,
						// Out of order and duplicated: hashed sorted, once.
						Effects: Effects{EffectMutatesExternal,
							EffectEgressesContent, EffectMutatesExternal},
						RequiresApproval: true, RequiresAttestation: true},
					{Name: "read", InputSchema: schema, OutputSchema: schema},
				}},
				{Name: "watch", Actions: []Action{
					{Name: "tail", InputSchema: schema, OutputSchema: schema,
						RequiresApproval: true},
				}},
			},
			LeaseExpiresAt: lease, RevokedAt: revoked,
			// Not part of the mutation: server acceptance time.
			UpdatedAt: time.Unix(1, 0),
		},
	}
	var want []byte
	for _, part := range [][]byte{
		layoutField([]byte("shoal.fleet.registry-mutation.v2")),
		layoutField([]byte("key")), layoutUint(3),
		layoutField([]byte("agent")), layoutUint(4),
		layoutField([]byte("owner")), layoutField([]byte("operator")),
		layoutField([]byte("parent")), layoutField([]byte("domain")),
		layoutUint(2),
		layoutField([]byte("s1")), layoutField([]byte("p1")),
		layoutField([]byte("s2")), layoutField([]byte("p2")),
		layoutField([]byte("local")),
		layoutUint(2),
		layoutField([]byte("ops")), layoutUint(2),
		layoutField([]byte("deploy")),
		layoutUint(2),
		layoutField([]byte("egresses-content")), layoutField([]byte("external")),
		layoutField(schema), layoutField(schema),
		layoutField([]byte{1}), layoutField([]byte{1}),
		layoutField([]byte("read")),
		layoutUint(0),
		layoutField(schema), layoutField(schema),
		layoutField([]byte{0}), layoutField([]byte{0}),
		layoutField([]byte("watch")), layoutUint(1),
		layoutField([]byte("tail")),
		layoutUint(0),
		layoutField(schema), layoutField(schema),
		layoutField([]byte{1}), layoutField([]byte{0}),
		layoutUint(uint64(lease.UnixNano())),
		layoutUint(uint64(revoked.UnixNano())),
	} {
		want = append(want, part...)
	}
	var recorded recordingHash
	writeRegistryMutationV2(&recorded, mutation)
	if !bytes.Equal(recorded.Bytes(), want) {
		t.Fatalf("v2 layout drifted\n got  %x\n want %x", recorded.Bytes(), want)
	}
	if registryMutationDigest(mutation) != sha256.Sum256(want) {
		t.Fatal("v2 digest is not the SHA-256 of its layout")
	}
	// Frozen: the v2 digest of the fixture as first shipped.
	const goldenV2 = "49bd0b31c3151036a670126daf8bc3e4" +
		"5c458b4cbb7527eb70b966b6ed0c4516"
	digest := registryMutationDigest(mutation)
	if got := hex.EncodeToString(digest[:]); got != goldenV2 {
		t.Fatalf("v2 digest = %s, want %s", got, goldenV2)
	}
}

// TestRegistryDigestV2SeparatesEveryListBoundary moves one element across
// each list boundary in turn, the shape #521 describes, and requires v2 to
// tell the two apart. Each pair keeps every other field identical.
func TestRegistryDigestV2SeparatesEveryListBoundary(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	action := func(name string) Action {
		return Action{Name: name, InputSchema: schema, OutputSchema: schema}
	}
	base := func() Descriptor {
		return Descriptor{
			ID: "agent", Generation: 1, Subject: "owner", Actor: "operator",
			AuthorizationDomain: []byte("domain"),
			Scopes:              []Scope{{SourceID: []byte("s"), PolicyID: []byte("p")}},
			ExecutorRef:         "local",
			LeaseExpiresAt:      time.Unix(1_800_000_000, 0).UTC(),
		}
	}
	pairs := []struct {
		name          string
		first, second func(*Descriptor)
	}{
		{"actions across capabilities",
			func(d *Descriptor) {
				d.Capabilities = []Capability{
					{Name: "c1", Actions: []Action{action("a1"), action("a2")}},
					{Name: "c2", Actions: []Action{action("a3")}},
				}
			},
			func(d *Descriptor) {
				d.Capabilities = []Capability{
					{Name: "c1", Actions: []Action{action("a1")}},
					{Name: "c2", Actions: []Action{action("a2"), action("a3")}},
				}
			}},
		{"effect versus action",
			func(d *Descriptor) {
				held := action("a")
				held.Effects = Effects{EffectMutatesExternal}
				d.Capabilities = []Capability{{Name: "c", Actions: []Action{held}}}
			},
			func(d *Descriptor) {
				d.Capabilities = []Capability{{Name: "c", Actions: []Action{
					action("a"), action(string(EffectMutatesExternal)),
				}}}
			}},
	}
	for _, pair := range pairs {
		first, second := base(), base()
		pair.first(&first)
		pair.second(&second)
		if registryMutationDigest(Mutation{Descriptor: first}) ==
			registryMutationDigest(Mutation{Descriptor: second}) {
			t.Fatalf("%s: v2 digests are equal", pair.name)
		}
	}
}
