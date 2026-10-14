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
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/disclosureconformance"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// withholdingClientStub serves one snapshot and reports whatever withholding
// the probe asks for. The two responses a probe compares must differ in
// nothing except whether content was withheld, so the stub returns identical
// documents and results either way: an empty corpus view, which is exactly
// what a caller sees when matches are withheld.
type withholdingClientStub struct {
	explorer.BoundedClient
	snapshot   explorer.Snapshot
	disclosure authorized.Disclosure
	embedding  *authorized.EmbeddingQueryReport
	// retrieveErr makes the stub fail the way a provider outage does, so the
	// error branch that carries an embedding report is exercised too.
	retrieveErr error
}

func (c *withholdingClientStub) Snapshot(
	context.Context,
) (explorer.Snapshot, error) {
	return c.snapshot, nil
}

func (c *withholdingClientStub) DocumentsWithDisclosure(
	context.Context,
) ([]explorer.DocumentSummary, authorized.Disclosure, error) {
	return nil, c.disclosure, nil
}

func (c *withholdingClientStub) RetrieveWithReport(
	context.Context, retrieval.Request,
) (retrieval.Response, authorized.RetrievalReport, error) {
	return retrieval.Response{}, authorized.RetrievalReport{
		Disclosure: c.disclosure, Embedding: c.embedding,
	}, c.retrieveErr
}

func withholdingService(
	t *testing.T, conceal bool, disclosure authorized.Disclosure,
	embedding *authorized.EmbeddingQueryReport,
) *EmbeddedService {
	t.Helper()
	return withholdingServiceFailing(t, conceal, disclosure, embedding, nil)
}

func withholdingServiceFailing(
	t *testing.T, conceal bool, disclosure authorized.Disclosure,
	embedding *authorized.EmbeddingQueryReport, retrieveErr error,
) *EmbeddedService {
	t.Helper()
	service, err := NewEmbeddedService(&withholdingClientStub{
		retrieveErr: retrieveErr,
		snapshot: explorer.Snapshot{
			ID: "snapshot", AsOf: time.Unix(0, 0).UTC(), Frontier: 1,
		},
		disclosure: disclosure,
		embedding:  embedding,
	})
	if err != nil {
		t.Fatal(err)
	}
	service.ConcealWithholding(conceal)
	return service
}

func documentsProbeResponse(
	t *testing.T, conceal bool, disclosure authorized.Disclosure,
) DocumentsResponse {
	t.Helper()
	service := withholdingService(t, conceal, disclosure, nil)
	response, err := service.Documents(context.Background(), DocumentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func retrievalProbeResponse(
	t *testing.T, conceal bool, disclosure authorized.Disclosure,
	embedding *authorized.EmbeddingQueryReport,
) RetrievalResponse {
	t.Helper()
	service := withholdingService(t, conceal, disclosure, embedding)
	response, err := service.Retrieve(context.Background(), RetrievalRequest{
		Query: retrieval.Request{Text: "probe", TopK: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

var (
	// Content exists and was withheld from this caller.
	probeWithheld = authorized.Disclosure{Suppressed: 3, Restricted: 2}
	// Nothing matched. The caller must not be able to tell these apart.
	probeControl = authorized.Disclosure{}
)

// TestDisclosureIsConcealedUniformly is the regression gate. With concealment
// on, no response surface may vary with whether content was withheld. It
// compares encoded responses rather than named fields, so a field added later
// that happens to carry the signal fails here without anyone remembering to
// extend the test.
func TestDisclosureIsConcealedUniformly(t *testing.T) {
	withheldEmbedding := &authorized.EmbeddingQueryReport{
		Observed: true, FanoutLimit: 4, Suppressed: true, Restricted: true,
	}
	controlEmbedding := &authorized.EmbeddingQueryReport{
		Observed: true, FanoutLimit: 4,
	}
	disclosureconformance.Run(t,
		disclosureconformance.Probe{
			Name:     "documents",
			Withheld: documentsProbeResponse(t, true, probeWithheld),
			Control:  documentsProbeResponse(t, true, probeControl),
		},
		disclosureconformance.Probe{
			Name:     "retrieve",
			Withheld: retrievalProbeResponse(t, true, probeWithheld, nil),
			Control:  retrievalProbeResponse(t, true, probeControl, nil),
		},
		disclosureconformance.Probe{
			Name: "retrieve/embedding-report",
			Withheld: retrievalProbeResponse(
				t, true, probeWithheld, withheldEmbedding),
			Control: retrievalProbeResponse(
				t, true, probeControl, controlEmbedding),
		},
	)
}

// TestDefaultDisclosureRemainsDeliberate pins the accepted trade rather than
// assuming it. The default emits the withholding counts on purpose, so that a
// short answer is never silently mistaken for an empty corpus; the reasoning
// is recorded on EmbeddedService.retrieveReporting. If that default ever
// changes silently, this fails and the change has to be argued for.
func TestDefaultDisclosureRemainsDeliberate(t *testing.T) {
	const reason = "the default discloses withholding so a short answer is " +
		"never silently mistaken for an empty corpus; see retrieveReporting"
	disclosureconformance.Run(t,
		disclosureconformance.Probe{
			Name:            "documents",
			Withheld:        documentsProbeResponse(t, false, probeWithheld),
			Control:         documentsProbeResponse(t, false, probeControl),
			Distinguishable: true,
			Reason:          reason,
		},
		disclosureconformance.Probe{
			Name:            "retrieve",
			Withheld:        retrievalProbeResponse(t, false, probeWithheld, nil),
			Control:         retrievalProbeResponse(t, false, probeControl, nil),
			Distinguishable: true,
			Reason:          reason,
		},
	)
}

// failedRetrievalOutcome is what a caller observes when retrieval fails while
// an embedding report is in flight. The report rides out on the error, so the
// error is as much a disclosure surface as a successful body is.
type failedRetrievalOutcome struct {
	ErrorText string                    `json:"error_text"`
	Embedding *wireEmbeddingQueryReport `json:"embedding,omitempty"`
	Status    int                       `json:"status"`
	Body      json.RawMessage           `json:"body"`
}

func failedRetrievalProbe(
	t *testing.T, conceal bool, disclosure authorized.Disclosure,
	embedding *authorized.EmbeddingQueryReport,
) failedRetrievalOutcome {
	t.Helper()
	service := withholdingServiceFailing(t, conceal, disclosure, embedding,
		shoal.NewError(shoal.ErrorUnavailable, "embedding provider is down"))
	_, err := service.Retrieve(context.Background(), RetrievalRequest{
		Query: retrieval.Request{
			Text: "probe", TopK: 4, Modes: []retrieval.Mode{retrieval.ModeVector},
		},
	})
	if err == nil {
		t.Fatal("the probe requires a failing retrieval")
	}
	outcome := failedRetrievalOutcome{ErrorText: err.Error()}
	var embeddingErr *EmbeddingQueryError
	if errors.As(err, &embeddingErr) {
		report := embeddingErr.EmbeddingQueryReport()
		outcome.Embedding = wireEmbeddingQueryReportValue(&report)
	}
	// The HTTP encoding is the surface a caller actually reads, so compare
	// that too rather than only the value the service returned.
	recorder := httptest.NewRecorder()
	writeError(recorder, err)
	outcome.Status = recorder.Code
	outcome.Body = json.RawMessage(recorder.Body.Bytes())
	return outcome
}

// TestConcealedFailedRetrievalDoesNotDiscloseWithholding covers the error
// branch. A failing retrieval still carries the embedding report, and that
// report's derived withholding booleans must be concealed exactly as they are
// on the success path, including in the encoded HTTP error a caller reads.
func TestConcealedFailedRetrievalDoesNotDiscloseWithholding(t *testing.T) {
	withheld := &authorized.EmbeddingQueryReport{
		Observed: true, FanoutLimit: 4, Degraded: true,
		Suppressed: true, Restricted: true,
	}
	control := &authorized.EmbeddingQueryReport{
		Observed: true, FanoutLimit: 4, Degraded: true,
	}
	disclosureconformance.Run(t, disclosureconformance.Probe{
		Name:     "retrieve/failed-with-embedding-report",
		Withheld: failedRetrievalProbe(t, true, probeWithheld, withheld),
		Control:  failedRetrievalProbe(t, true, probeControl, control),
	})
}

// TestFailedRetrievalDoesNotMutateTheAuditReport proves concealment copies the
// report on the error path as it does on the success path. The original is
// still the value the audit trail keeps.
func TestFailedRetrievalDoesNotMutateTheAuditReport(t *testing.T) {
	report := &authorized.EmbeddingQueryReport{
		Observed: true, Suppressed: true, Restricted: true,
	}
	failedRetrievalProbe(t, true, probeWithheld, report)
	if !report.Suppressed || !report.Restricted {
		t.Fatalf("concealment mutated the audit report: %#v", report)
	}
}
