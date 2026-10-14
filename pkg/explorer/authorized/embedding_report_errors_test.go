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

package authorized_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestAuthorizedEmbeddingCodedCancellationRedactsBackendDetails(t *testing.T) {
	for _, test := range []struct {
		name     string
		code     shoal.ErrorCode
		sentinel error
	}{
		{"canceled", shoal.ErrorCanceled, context.Canceled},
		{"deadline", shoal.ErrorDeadline, context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newEmbeddingReportFixture(t)
			f.ingest(t, f.clientA, f.admin(t),
				"file:///redacted-vector.txt", "vector evidence", publicSpaceIdentity)
			const detail = "backend-only-model-and-request-detail"
			f.backend.beforeRetrieve = func(context.Context) error {
				return shoal.NewError(test.code, detail)
			}
			response, report, err := f.clientA.RetrieveWithReport(
				f.alice(t), vectorQuery("vector"))
			if !shoal.IsErrorCode(err, test.code) {
				t.Fatalf("error = %v, want code %s", err, test.code)
			}
			if strings.Contains(err.Error(), detail) {
				t.Errorf("backend detail escaped authorization boundary: %v", err)
			}
			if !errors.Is(err, test.sentinel) {
				t.Errorf("error lost cancellation identity: %v", err)
			}
			if len(response.Results) != 0 || report.Embedding == nil || !report.Embedding.Degraded {
				t.Errorf("failed retrieval returned incorrect response/report: %+v / %+v", response, report)
			}
		})
	}
}
