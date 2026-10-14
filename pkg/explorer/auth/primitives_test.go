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

package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestAuthorizationFingerprintIsDeterministicAndNonDisclosing(t *testing.T) {
	firstConfig := baseDecisionConfig()
	firstConfig.AllowedOperations = []auth.Operation{
		auth.OperationRetrieve, auth.OperationRead, auth.OperationRetrieve}
	firstConfig.PermittedSourceIDs = [][]byte{
		[]byte("source-b"), []byte("source-a"), []byte("source-a")}
	firstConfig.PermittedPolicyIDs = [][]byte{
		[]byte("policy-b"), []byte("policy-a"), []byte("policy-a")}
	first := mustDecision(t, firstConfig)

	secondConfig := baseDecisionConfig()
	secondConfig.AuthenticationExpires = testNow.Add(2 * firstConfig.AuthenticationExpires.Sub(testNow))
	secondConfig.RequestID = "different-request"
	secondConfig.CorrelationID = "different-correlation"
	secondConfig.AuditPurpose = "different purpose"
	second := mustDecision(t, secondConfig)

	firstFingerprint, err := auth.AuthorizationFingerprint(first)
	if err != nil {
		t.Fatalf("AuthorizationFingerprint(first) = %v", err)
	}
	secondFingerprint, err := auth.AuthorizationFingerprint(second)
	if err != nil {
		t.Fatalf("AuthorizationFingerprint(second) = %v", err)
	}
	if firstFingerprint != secondFingerprint {
		t.Fatalf("equivalent fingerprints differ: %s != %s",
			firstFingerprint, secondFingerprint)
	}

	changedConfig := baseDecisionConfig()
	changedConfig.PermittedSourceIDs = [][]byte{[]byte("source-a")}
	changed := mustDecision(t, changedConfig)
	changedFingerprint, _ := auth.AuthorizationFingerprint(changed)
	if firstFingerprint == changedFingerprint {
		t.Fatalf("changed grants retained fingerprint %s", firstFingerprint)
	}
	for _, raw := range []string{
		"subject-secret", "actor-secret", "domain-secret", "source-a", "policy-a",
	} {
		if strings.Contains(firstFingerprint.String(), raw) {
			t.Fatalf("Fingerprint.String() exposed %q in %q",
				raw, firstFingerprint.String())
		}
	}
}

func cacheConfig(decision auth.Decision) auth.CacheKeyConfig {
	return auth.CacheKeyConfig{
		Decision:            decision,
		AuthorizationDomain: []byte("domain-secret"),
		PolicyCopyPin:       []byte("raw-policy-copy-pin"),
		SnapshotFrontier:    42,
		HistoryFloor:        3,
		RetentionGeneration: 9,
		Request: retrieval.Request{
			Text:  "raw secret query",
			TopK:  5,
			Modes: []retrieval.Mode{retrieval.ModeVector, retrieval.ModeLexical},
			Scope: retrieval.Scope{
				DocumentIDs: []shoal.ID{"document-b", "document-a", "document-b"},
				NodeIDs:     []shoal.ID{"node-a"},
			},
			Explain: true,
		},
		Limits: map[string]uint64{
			"frontier_nodes": 100,
			"response_bytes": 4096,
		},
		IndexGenerations: map[string][]byte{
			"lexical": []byte("raw-lexical-generation"),
			"vector":  []byte("raw-vector-generation"),
		},
	}
}

func TestCacheKeyIsDeterministicDefensiveAndFullyPartitioned(t *testing.T) {
	decision := mustDecision(t, baseDecisionConfig())
	firstConfig := cacheConfig(decision)
	first, err := auth.NewCacheKey(firstConfig)
	if err != nil {
		t.Fatalf("NewCacheKey(first) = %v", err)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("CacheKey.Validate() = %v", err)
	}
	if first.PartitionDigest() != first.Digest() {
		t.Fatal("positive/negative partition digest differs from cache digest")
	}

	secondConfig := cacheConfig(decision)
	secondConfig.Limits = map[string]uint64{
		"response_bytes": 4096,
		"frontier_nodes": 100,
	}
	secondConfig.IndexGenerations = map[string][]byte{
		"vector":  []byte("raw-vector-generation"),
		"lexical": []byte("raw-lexical-generation"),
	}
	second, err := auth.NewCacheKey(secondConfig)
	if err != nil {
		t.Fatalf("NewCacheKey(second) = %v", err)
	}
	if first.Digest() != second.Digest() {
		t.Fatalf("map order changed digest: %s != %s", first.Digest(), second.Digest())
	}

	saved := first.Digest()
	firstConfig.AuthorizationDomain[0] = 'X'
	firstConfig.PolicyCopyPin[0] = 'X'
	firstConfig.Request.Text = "changed"
	firstConfig.Request.Modes[0] = retrieval.ModeGraph
	firstConfig.Request.Scope.DocumentIDs[0] = "changed"
	firstConfig.IndexGenerations["lexical"][0] = 'X'
	firstConfig.Limits["frontier_nodes"] = 1
	if first.Digest() != saved {
		t.Fatal("cache key retained caller-owned mutable input")
	}

	queryChanged := cacheConfig(decision)
	queryChanged.Request.Text = "different query"
	queryKey, err := auth.NewCacheKey(queryChanged)
	if err != nil {
		t.Fatalf("NewCacheKey(query changed) = %v", err)
	}
	if queryKey.Digest() == first.Digest() {
		t.Fatal("query change did not partition cache")
	}

	generationConfig := baseDecisionConfig()
	generationConfig.PolicyGeneration++
	generationDecision := mustDecision(t, generationConfig)
	generationKey, err := auth.NewCacheKey(cacheConfig(generationDecision))
	if err != nil {
		t.Fatalf("NewCacheKey(generation changed) = %v", err)
	}
	if generationKey.Digest() == first.Digest() {
		t.Fatal("policy generation change did not partition cache")
	}

	for _, raw := range []string{
		"raw secret query",
		"document-a",
		"node-a",
		"raw-policy-copy-pin",
		"raw-lexical-generation",
		"domain-secret",
	} {
		if strings.Contains(first.String(), raw) {
			t.Fatalf("CacheKey.String() exposed %q in %q", raw, first.String())
		}
	}
}

func TestRedactionAndAuditEventsRetainNoRawSensitiveValues(t *testing.T) {
	rawValues := []string{
		"raw-query-secret",
		"raw-quote-secret",
		"raw-source-text-secret",
		"raw-object-id-secret",
		"raw-label-secret",
		"raw-credential-secret",
		"raw-visibility-secret",
		"raw-row-secret",
		"raw-table-secret",
		"raw-response-secret",
	}
	attributeNames := []auth.AuditAttributeName{
		auth.AuditAttributeRequest,
		auth.AuditAttributeObject,
		auth.AuditAttributeSource,
		auth.AuditAttributeSubject,
		auth.AuditAttributePolicy,
		auth.AuditAttributeServiceCeiling,
		auth.AuditAttributeAuthorizationDomain,
		auth.AuditAttributeCache,
		auth.AuditAttributeGeneration,
		auth.AuditAttributeCorrelation,
	}
	attributes := make([]auth.AuditAttribute, 0, len(rawValues))
	for index, raw := range rawValues {
		redacted := auth.Redact([]byte(raw))
		if strings.Contains(redacted.String(), raw) {
			t.Fatalf("Redact().String() exposed %q: %q", raw, redacted)
		}
		if redacted.Digest() ==
			auth.DigestBytes("explorer-redacted-value-v1", []byte(raw)) {
			t.Fatalf("Redact() retained a guessable unkeyed digest for %q", raw)
		}
		if redacted.Size() != len(raw) {
			t.Fatalf("Redact().Size() = %d, want %d", redacted.Size(), len(raw))
		}
		attribute, err := auth.NewAuditAttribute(attributeNames[index], []byte(raw))
		if err != nil {
			t.Fatalf("NewAuditAttribute(%q) = %v", attributeNames[index], err)
		}
		if strings.Contains(attribute.String(), raw) ||
			strings.Contains(attribute.Value().String(), raw) {
			t.Fatalf("audit attribute exposed %q: %q", raw, attribute)
		}
		attributes = append(attributes, attribute)
	}
	if _, err := auth.NewAuditAttribute(
		auth.AuditAttributeName("raw query text"), []byte(rawValues[0]),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("arbitrary audit attribute name = %v", err)
	}

	decision := mustDecision(t, baseDecisionConfig())
	fingerprint, _ := auth.AuthorizationFingerprint(decision)
	cacheKey, err := auth.NewCacheKey(cacheConfig(decision))
	if err != nil {
		t.Fatalf("NewCacheKey() = %v", err)
	}
	event, err := auth.NewAuditEvent(auth.AuditEventConfig{
		OccurredAt:               testNow,
		Operation:                auth.OperationRetrieve,
		Outcome:                  auth.AuditDenied,
		AuthorizationFingerprint: fingerprint,
		RequestDigest:            cacheKey.RequestDigest(),
		Attributes:               attributes,
	})
	if err != nil {
		t.Fatalf("NewAuditEvent() = %v", err)
	}
	for _, raw := range rawValues {
		if strings.Contains(event.String(), raw) {
			t.Fatalf("AuditEvent.String() exposed %q: %q", raw, event)
		}
	}
	returnedAttributes := event.Attributes()
	returnedAttributes[0], _ = auth.NewAuditAttribute(
		auth.AuditAttributeCache, []byte("changed"))
	if event.Attributes()[0].Name() != auth.AuditAttributeRequest {
		t.Fatal("AuditEvent.Attributes() handed out the live slice")
	}
}

type generationReaderFunc func(context.Context, []byte) (int64, error)

func (f generationReaderFunc) CurrentPolicyGeneration(
	ctx context.Context,
	domain []byte,
) (int64, error) {
	return f(ctx, domain)
}

func TestGenerationGuardDetectsChangeAndMapsCancellation(t *testing.T) {
	decision := mustDecision(t, baseDecisionConfig())
	current := int64(7)
	var observedDomain []byte
	reader := generationReaderFunc(func(_ context.Context, domain []byte) (int64, error) {
		observedDomain = append([]byte(nil), domain...)
		domain[0] = 'X'
		return current, nil
	})
	guard, err := auth.NewGenerationGuard(decision, reader)
	if err != nil {
		t.Fatalf("NewGenerationGuard() = %v", err)
	}
	if err := guard.Check(context.Background()); err != nil {
		t.Fatalf("Check(current) = %v", err)
	}
	if string(observedDomain) != "domain-secret" {
		t.Fatalf("reader domain = %q", observedDomain)
	}
	if guard.ResolvedGeneration() != 7 {
		t.Fatalf("ResolvedGeneration() = %d", guard.ResolvedGeneration())
	}

	current = 8
	if err := guard.Check(context.Background()); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("Check(changed) = %v", err)
	}

	failed, err := auth.NewGenerationGuard(
		decision,
		generationReaderFunc(func(context.Context, []byte) (int64, error) {
			return 0, errors.New("backend failed")
		}),
	)
	if err != nil {
		t.Fatalf("NewGenerationGuard(failed) = %v", err)
	}
	if err := failed.Check(context.Background()); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("Check(reader error) = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := guard.Check(canceled); !errors.Is(err, context.Canceled) ||
		!shoal.IsErrorCode(err, shoal.ErrorCanceled) {
		t.Fatalf("Check(canceled) = %v", err)
	}
}
