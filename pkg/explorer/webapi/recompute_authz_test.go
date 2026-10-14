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

package webapi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// recomputeAuthzFixture builds two mutually unauthorized identities over one
// corpus. Identity A ingests both documents backing a latent link cell, so only
// A can observe the derived assertion the cell produces; identity B is
// authorized for neither document.
type recomputeAuthzFixture struct {
	serviceA    *webapi.EmbeddedService
	serviceB    *webapi.EmbeddedService
	ctxA        context.Context
	ctxB        context.Context
	sourceID    shoal.ID
	assertionID shoal.ID
}

func newRecomputeAuthzFixture(t *testing.T) *recomputeAuthzFixture {
	t.Helper()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })

	authority := auth.NewAuthority()
	store := authorized.NewMemoryPolicyStore()
	scorer, _ := any(corpus).(authorized.VectorScorer)

	newClient := func(source, policy []byte) *authorized.Client {
		selector, err := authorized.NewStaticPolicySelector(source, policy)
		if err != nil {
			t.Fatal(err)
		}
		client, err := authorized.NewClient(authorized.Config{
			Base:                   corpus,
			VectorScorer:           scorer,
			DerivedAssertionReader: corpus,
			Resolver:               authority.Resolver(),
			PolicySelector:         selector,
			PolicyStore:            store,
			GenerationReader:       authnGenerationReader{},
			Clock:                  time.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}

	clientA := newClient(authnSourceGranted, authnPolicyGranted)
	clientB := newClient(authnSourceOther, authnPolicyOther)

	ctxA, err := authority.Binder().Bind(
		context.Background(), authnPrincipal(t, "granted"))
	if err != nil {
		t.Fatal(err)
	}
	ctxB, err := authority.Binder().Bind(
		context.Background(), authnPrincipal(t, "other-grant"))
	if err != nil {
		t.Fatal(err)
	}

	// Identity A owns both endpoints of the latent link, so the derived
	// assertion is visible to A and to nobody else.
	var sourceID, targetID shoal.ID
	for index, source := range []explorer.Source{
		{
			URI:       "file:///authz-recompute-source.txt",
			Title:     "authz recompute source",
			MediaType: explorer.MediaTypeText,
			Content:   "authz recompute source",
		},
		{
			URI:       "file:///authz-recompute-target.txt",
			Title:     "authz recompute target",
			MediaType: explorer.MediaTypeText,
			Content:   "authz recompute target",
		},
	} {
		result, err := clientA.Ingest(ctxA, source)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			sourceID = result.Document.ID
		} else {
			targetID = result.Document.ID
		}
	}
	if err := corpus.PutLatentLinkCells(
		context.Background(), []explorer.LatentLinkCell{{
			Row:             []byte("cell-authz:" + string(sourceID)),
			ColumnFamily:    []byte("link"),
			ColumnQualifier: []byte(targetID),
			Timestamp:       42,
			Value:           []byte("0.91"),
		}},
	); err != nil {
		t.Fatal(err)
	}

	serviceA, err := webapi.NewEmbeddedService(clientA)
	if err != nil {
		t.Fatal(err)
	}
	serviceB, err := webapi.NewEmbeddedService(clientB)
	if err != nil {
		t.Fatal(err)
	}

	fixture := &recomputeAuthzFixture{
		serviceA: serviceA, serviceB: serviceB,
		ctxA: ctxA, ctxB: ctxB, sourceID: sourceID,
	}
	fixture.assertionID = fixture.derivedAssertionID(t)
	return fixture
}

// derivedAssertionID reads the derived assertion identifier the way a browser
// does: off the serialized neighborhood response, then decoded from the wire
// encoding. Reading it through the wire keeps the test honest about the
// identifier a caller actually holds and could replay.
func (f *recomputeAuthzFixture) derivedAssertionID(t *testing.T) shoal.ID {
	t.Helper()
	response, err := f.serviceA.Neighborhood(f.ctxA, webapi.NeighborhoodRequest{
		NodeIDs: []shoal.ID{f.sourceID},
		Depth:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Neighborhood struct {
			Assertions []struct {
				ID     string `json:"id"`
				Origin string `json:"origin"`
			} `json:"assertions"`
		} `json:"neighborhood"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Neighborhood.Assertions) != 1 ||
		wire.Neighborhood.Assertions[0].Origin != "derived" {
		t.Fatalf("neighborhood assertions = %+v, want one derived assertion",
			wire.Neighborhood.Assertions)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(
		wire.Neighborhood.Assertions[0].ID)
	if err != nil {
		t.Fatalf("assertion ID is not wire encoded: %v", err)
	}
	return shoal.ID(decoded)
}

// serializedRecomputeError mirrors the body writeError puts on the wire for a
// failed request, so two failures can be compared exactly as a caller observes
// them rather than as Go values.
func serializedRecomputeError(t *testing.T, err error) []byte {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var shoalErr *shoal.Error
	if !errors.As(err, &shoalErr) {
		t.Fatalf("error is not a shoal error: %v", err)
	}
	payload, marshalErr := json.Marshal(struct {
		Code    shoal.ErrorCode `json:"code"`
		Message string          `json:"message"`
	}{Code: shoalErr.Code, Message: err.Error()})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	return payload
}

// TestRecomputeIsScopedToTheCallingIdentity pins the authorization scope of the
// recompute endpoint. The endpoint takes a caller-supplied assertion ID, so
// without this pin a refactor that sourced the assertion outside the authorized
// read path would turn recompute into a cross-identity read of derived
// assertions — producer model, version, threshold, tessellation cell, and
// score — with the whole suite still green.
//
// Load-bearing: the refusal is produced by the authorized bounded read path,
// whose provenance-seed guard returns auth.ObjectNotFound rather than an empty
// success. Removing the seed check and the neighborhood filter together lets
// identity B read identity A's full derivation detail and fails this test.
//
// The third assertion is the one that matters most: B's failure for a real but
// unauthorized assertion must be byte-identical to its failure for an assertion
// that does not exist. Any difference between the two is an existence oracle.
func TestRecomputeIsScopedToTheCallingIdentity(t *testing.T) {
	f := newRecomputeAuthzFixture(t)

	// 1. The owning identity can recompute its own derived assertion.
	owned, err := f.serviceA.Recompute(f.ctxA, webapi.RecomputeDerivationRequest{
		AssertionID: f.assertionID,
	})
	if err != nil {
		t.Fatalf("owner recompute failed: %v", err)
	}
	if owned.Detail.AssertionID != f.assertionID {
		t.Fatalf("owner recompute returned assertion %q, want %q",
			owned.Detail.AssertionID, f.assertionID)
	}

	// 2. A different identity replaying the same real assertion ID is refused.
	unauthorized, err := f.serviceB.Recompute(
		f.ctxB, webapi.RecomputeDerivationRequest{AssertionID: f.assertionID})
	if err == nil {
		t.Fatalf("unauthorized identity recomputed another identity's "+
			"assertion: %+v", unauthorized)
	}
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("unauthorized recompute error = %v, want not_found", err)
	}

	// 3. That refusal must be indistinguishable from one for an assertion that
	// was never derived at all.
	_, missingErr := f.serviceB.Recompute(
		f.ctxB, webapi.RecomputeDerivationRequest{
			AssertionID: shoal.ID("assertion-that-was-never-derived"),
		})
	withheld := serializedRecomputeError(t, err)
	missing := serializedRecomputeError(t, missingErr)
	if string(withheld) != string(missing) {
		t.Fatalf("recompute is an existence oracle:\n withheld = %s\n missing  = %s",
			withheld, missing)
	}
}
