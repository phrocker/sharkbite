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

package ontology_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type fakeExtractor struct {
	extract func(context.Context, ontology.ExtractionRequest) (ontology.ExtractionResult, error)
}

func (f *fakeExtractor) Extract(
	ctx context.Context, request ontology.ExtractionRequest,
) (ontology.ExtractionResult, error) {
	return f.extract(ctx, request)
}

var _ ontology.Extractor = (*fakeExtractor)(nil)
var _ interface{ Validate() error } = ontology.ExtractionRequest{}
var _ interface{ Validate() error } = ontology.ExtractionResult{}

func TestExtractorContractWithFake(t *testing.T) {
	fixture := newOntologyFixture(t)
	requestMetadata := shoal.Metadata{"tenant": "test"}
	request, err := ontology.NewExtractionRequest(
		fixture.version,
		[]ontology.EvidenceRef{fixture.evidence},
		"Extract facts using only cited text.",
		fixture.provenance,
		ontology.DefaultExtractionLimits(),
		requestMetadata,
	)
	if err != nil {
		t.Fatal(err)
	}
	requestMetadata["tenant"] = "mutated"

	fake := &fakeExtractor{
		extract: func(
			ctx context.Context, received ontology.ExtractionRequest,
		) (ontology.ExtractionResult, error) {
			if err := ctx.Err(); err != nil {
				return ontology.ExtractionResult{}, err
			}
			if err := received.Validate(); err != nil {
				return ontology.ExtractionResult{}, err
			}
			if received.ID() != request.ID() ||
				received.Metadata()["tenant"] != "test" {
				return ontology.ExtractionResult{}, errors.New("request changed in transit")
			}
			return ontology.NewExtractionResult(
				received,
				[]ontology.Assertion{fixture.inferred, fixture.explicit},
				nil,
				testTime.Add(time.Minute),
				shoal.Metadata{"provider_request_id": "request-1"},
			)
		},
	}

	result, err := fake.Extract(context.Background(), request)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if err := result.ValidateFor(request); err != nil {
		t.Fatalf("validate extraction result: %v", err)
	}
	if result.RequestID() != request.ID() || len(result.Assertions()) != 2 {
		t.Fatal("fake extractor did not preserve the request/result contract")
	}
	if request.ContractVersion() != ontology.ExtractionContractV1 ||
		result.ContractVersion() != ontology.ExtractionContractV1 ||
		request.Provenance().Model() != result.Provenance().Model() ||
		request.Provenance().Prompt() != result.Provenance().Prompt() ||
		request.Provenance().Extractor() != result.Provenance().Extractor() {
		t.Fatal("extraction version or provenance was not bound to the request")
	}
	assertSortedIDs(t, result.Assertions()[0].ID(), result.Assertions()[1].ID())
	if result.Provenance().Model() != "test-model" ||
		result.Provenance().Prompt() != "ontology-v1" ||
		result.Provenance().Extractor() != "fake-extractor" {
		t.Fatal("result lost model, prompt, or extractor provenance")
	}

	assertions := result.Assertions()
	assertions[0] = ontology.Assertion{}
	metadata := result.Metadata()
	metadata["provider_request_id"] = "changed"
	if err := result.ValidateFor(request); err != nil {
		t.Fatalf("returned values mutated extraction result: %v", err)
	}
	if result.Metadata()["provider_request_id"] != "request-1" {
		t.Fatal("extraction result returned mutable metadata")
	}
}

func TestExtractorHonorsContextCancellation(t *testing.T) {
	fixture := newOntologyFixture(t)
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeExtractor{
		extract: func(
			ctx context.Context, _ ontology.ExtractionRequest,
		) (ontology.ExtractionResult, error) {
			return ontology.ExtractionResult{}, ctx.Err()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fake.Extract(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled extraction error = %v", err)
	}
}

func TestExtractionRequestCanonicalizesEvidenceAndClonesInputs(t *testing.T) {
	fixture := newOntologyFixture(t)
	earlier := mustEvidence(t, 0, 2, "a")
	later := mustEvidence(t, 3, 5, "b")
	input := []ontology.EvidenceRef{later, earlier}
	first, err := ontology.NewExtractionRequest(
		fixture.version, input, "Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), shoal.Metadata{"key": "value"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{earlier, later},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), shoal.Metadata{"key": "value"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != second.ID() {
		t.Fatal("evidence input order changed extraction request identity")
	}
	assertSortedIDs(t, first.Evidence()[0].ID(), first.Evidence()[1].ID())

	differentLimits := ontology.DefaultExtractionLimits()
	differentLimits.MaxAssertions--
	limited, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{earlier, later},
		"Extract cited facts.", fixture.provenance,
		differentLimits, shoal.Metadata{"key": "value"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() == limited.ID() {
		t.Fatal("extraction limits did not affect request identity")
	}

	input[0] = ontology.EvidenceRef{}
	returned := first.Evidence()
	returned[0] = ontology.EvidenceRef{}
	metadata := first.Metadata()
	metadata["key"] = "changed"
	if err := first.Validate(); err != nil {
		t.Fatalf("request exposed mutable values: %v", err)
	}
}

func TestExtractionRejectsEvidenceOutsideRequest(t *testing.T) {
	fixture := newOntologyFixture(t)
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	outside := mustEvidence(t, 10, 15, "outside")
	assertion := mustAssertion(
		t, fixture.person.ID(), fixture.title.ID(), ontology.AssertionExplicit,
		[]ontology.EvidenceRef{outside}, fixture.provenance,
	)
	_, err = ontology.NewExtractionResult(
		request, []ontology.Assertion{assertion}, nil,
		testTime.Add(time.Minute), nil,
	)
	assertInvalidArgument(t, err)
}

func TestExtractionRejectsUnknownAndMismatchedDefinitions(t *testing.T) {
	fixture := newOntologyFixture(t)
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	unknown := mustProperty(
		t, "unknown", "Unknown", "", ontology.ValueString, nil, nil)
	unknownAssertion := mustAssertion(
		t, fixture.person.ID(), unknown.ID(), ontology.AssertionExplicit,
		[]ontology.EvidenceRef{fixture.evidence}, fixture.provenance,
	)
	_, err = ontology.NewExtractionResult(
		request, []ontology.Assertion{unknownAssertion}, nil,
		testTime.Add(time.Minute), nil,
	)
	assertInvalidArgument(t, err)

	stringValue, err := ontology.NewStringValue("not a number")
	if err != nil {
		t.Fatal(err)
	}
	wrongType, err := ontology.NewAssertion(
		"entity:person-1", fixture.score.ID(), stringValue,
		ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{fixture.evidence}, fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.project.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ontology.NewExtractionResult(
		request, []ontology.Assertion{wrongType}, nil,
		testTime.Add(time.Minute), nil,
	)
	assertInvalidArgument(t, err)

	relationshipValue, err := ontology.NewStringValue("project-1")
	if err != nil {
		t.Fatal(err)
	}
	wrongRelationshipType, err := ontology.NewAssertion(
		"entity:person-1", fixture.worksOn.ID(), relationshipValue,
		ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{fixture.evidence}, fixture.provenance, nil,
		ontology.WithAssertionSubjectType(fixture.person.ID()),
		ontology.WithAssertionObjectType(fixture.project.ID()),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ontology.NewExtractionResult(
		request, []ontology.Assertion{wrongRelationshipType}, nil,
		testTime.Add(time.Minute), nil,
	)
	assertInvalidArgument(t, err)
}

func TestExtractionValidatesWireValuesAndProvenance(t *testing.T) {
	fixture := newOntologyFixture(t)
	invalidUTF8 := string([]byte{0xff})
	if _, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		invalidUTF8, fixture.provenance, ontology.DefaultExtractionLimits(), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("invalid instructions error = %v", err)
	}
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	otherProvenance, err := ontology.NewExtractionProvenance(
		"other-provider", "other-model", "1", "other-prompt", "1",
		"other-extractor", "1", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	otherRequest, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", otherProvenance,
		ontology.DefaultExtractionLimits(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if request.ID() == otherRequest.ID() {
		t.Fatal("extraction provenance did not affect request identity")
	}
	if _, err := ontology.NewExtractionResult(
		otherRequest, []ontology.Assertion{fixture.explicit}, nil,
		testTime.Add(time.Minute), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("mismatched provenance error = %v", err)
	}
	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{fixture.explicit}, nil,
		time.Time{}, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("missing completion timestamp error = %v", err)
	}
}

func TestExtractionLimitsFailClosedAndAllowNoAssertion(t *testing.T) {
	fixture := newOntologyFixture(t)
	limits := ontology.DefaultExtractionLimits()
	limits.MaxAssertions = 0
	limits.MaxProposals = 0
	request, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance, limits, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ontology.NewExtractionResult(
		request, nil, nil, testTime.Add(time.Minute), nil,
	)
	if err != nil {
		t.Fatalf("zero-assertion result: %v", err)
	}
	if err := result.ValidateFor(request); err != nil {
		t.Fatalf("validate zero-assertion result: %v", err)
	}
	if len(result.Assertions()) != 0 {
		t.Fatal("zero-assertion result unexpectedly contains assertions")
	}

	if _, err := ontology.NewExtractionResult(
		request, []ontology.Assertion{fixture.explicit}, nil,
		testTime.Add(time.Minute), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("disabled assertion output error = %v", err)
	}

	nextVersion, err := ontology.NewOntologyVersion(
		fixture.schema, "2", testTime.Add(2*time.Hour),
		fixture.version.Concepts(), fixture.version.Relationships(),
		fixture.version.Properties(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := ontology.NewGovernedProposal(
		fixture.schema, fixture.version, nextVersion,
		"extractor", "proposed ontology update", testTime.Add(3*time.Hour), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewExtractionResult(
		request, nil, []ontology.GovernedProposal{proposal},
		testTime.Add(4*time.Hour), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("disabled proposal output error = %v", err)
	}

	value, err := ontology.NewStringValue("subject")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ontology.NewAssertion(
		"entity:person-1", fixture.title.ID(), value,
		ontology.AssertionExplicit, 1, nil, fixture.provenance, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("uncited assertion error = %v", err)
	}
}

func TestExtractionRequestRejectsExceededBounds(t *testing.T) {
	fixture := newOntologyFixture(t)
	second := mustEvidence(t, 8, 12, "more")

	tooFewEvidence := ontology.DefaultExtractionLimits()
	tooFewEvidence.MaxEvidence = 1
	if _, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence, second},
		"Extract cited facts.", fixture.provenance, tooFewEvidence, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("evidence bound error = %v", err)
	}

	tooSmallQuote := ontology.DefaultExtractionLimits()
	tooSmallQuote.MaxQuoteBytes = 1
	if _, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance, tooSmallQuote, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("quote bound error = %v", err)
	}

	tooSmallInstructions := ontology.DefaultExtractionLimits()
	tooSmallInstructions.MaxInstructionBytes = 1
	if _, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance, tooSmallInstructions, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("instruction bound error = %v", err)
	}

	tooFewSchemaMembers := ontology.DefaultExtractionLimits()
	tooFewSchemaMembers.MaxSchemaMembers = 1
	if _, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance, tooFewSchemaMembers, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("schema member bound error = %v", err)
	}

	tooFewMetadataEntries := ontology.DefaultExtractionLimits()
	tooFewMetadataEntries.MaxMetadataEntries = 1
	if _, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance, tooFewMetadataEntries,
		shoal.Metadata{"first": "1", "second": "2"},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("metadata entry bound error = %v", err)
	}

	invalidLimits := ontology.DefaultExtractionLimits()
	invalidLimits.MaxEvidence = ^uint32(0)
	if _, err := ontology.NewExtractionRequest(
		fixture.version, []ontology.EvidenceRef{fixture.evidence},
		"Extract cited facts.", fixture.provenance, invalidLimits, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("hard limit error = %v", err)
	}
}
