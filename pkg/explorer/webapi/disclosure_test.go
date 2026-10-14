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

package webapi

import (
	"reflect"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
)

// TestConcealWithholdingClearsBothReasonClasses proves the two counts are
// concealed together. Omitting one while emitting the other would separate the
// reason classes in exactly the way concealment exists to prevent.
func TestConcealWithholdingClearsBothReasonClasses(t *testing.T) {
	disclosure := authorized.Disclosure{Suppressed: 3, Restricted: 2}
	open := &EmbeddedService{}
	if got := open.disclose(disclosure); got != disclosure {
		t.Fatalf("default must preserve the documented disclosure: %#v", got)
	}
	concealed := &EmbeddedService{}
	concealed.ConcealWithholding(true)
	if got := concealed.disclose(disclosure); got != (authorized.Disclosure{}) {
		t.Fatalf("concealed disclosure = %#v", got)
	}
}

// TestConcealWithholdingClearsEmbeddingSignal proves the embedding report's
// booleans are concealed with the counts they derive from. Leaving them set
// would keep the same signal in a narrower form rather than removing it.
func TestConcealWithholdingClearsEmbeddingSignal(t *testing.T) {
	report := &authorized.EmbeddingQueryReport{
		Spaces: []authorized.EmbeddingSpaceReport{{ID: "space"}},
		// Request-local observability, unrelated to withholding: it must
		// survive concealment.
		FanoutLimit: 4, CacheHits: 2, ProviderCalls: 1, Observed: true,
		Degraded: true, FanoutExceeded: true,
		Suppressed: true, Restricted: true,
	}
	open := &EmbeddedService{}
	if got := open.discloseEmbedding(report); got != report {
		t.Fatal("default must pass the report through untouched")
	}

	concealed := &EmbeddedService{}
	concealed.ConcealWithholding(true)
	got := concealed.discloseEmbedding(report)
	if got == nil {
		t.Fatal("concealment must not drop the report")
	}
	if got.Suppressed || got.Restricted {
		t.Fatalf("withholding signal survived concealment: %#v", got)
	}
	if !got.Observed || !got.Degraded || !got.FanoutExceeded ||
		got.FanoutLimit != 4 || got.CacheHits != 2 || got.ProviderCalls != 1 ||
		len(got.Spaces) != 1 {
		t.Fatalf("concealment altered unrelated observability: %#v", got)
	}
	// The caller's report must not be mutated: it is still the audit value.
	if !report.Suppressed || !report.Restricted {
		t.Fatalf("concealment mutated the audit report: %#v", report)
	}
	if concealed.discloseEmbedding(nil) != nil {
		t.Fatal("a nil report must stay nil")
	}
}

// TestConcealedResponsesDoNotVaryWithWithholding is the property that matters:
// with concealment on, a response where content was withheld must be
// indistinguishable from one where nothing was.
func TestConcealedResponsesDoNotVaryWithWithholding(t *testing.T) {
	service := &EmbeddedService{}
	service.ConcealWithholding(true)
	withheld := service.disclose(
		authorized.Disclosure{Suppressed: 7, Restricted: 4})
	nothing := service.disclose(authorized.Disclosure{})
	if withheld != nothing {
		t.Fatalf("withheld %#v is distinguishable from %#v", withheld, nothing)
	}
	withheldEmbedding := service.discloseEmbedding(
		&authorized.EmbeddingQueryReport{Suppressed: true, Restricted: true})
	nothingEmbedding := service.discloseEmbedding(
		&authorized.EmbeddingQueryReport{})
	if !reflect.DeepEqual(withheldEmbedding, nothingEmbedding) {
		t.Fatalf("embedding reports differ: %#v vs %#v",
			withheldEmbedding, nothingEmbedding)
	}
}
