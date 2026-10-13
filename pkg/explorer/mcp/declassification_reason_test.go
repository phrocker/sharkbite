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

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// reasonRecordingService records the ingest requests the tool passes on.
type reasonRecordingService struct {
	*allOptionalService
	requests []webapi.IngestRequest
}

func (s *reasonRecordingService) Ingest(
	_ context.Context,
	request webapi.IngestRequest,
) (webapi.IngestResponse, error) {
	s.requests = append(s.requests, request)
	return webapi.IngestResponse{}, nil
}

// TestIngestToolCarriesDeclassificationReason pins #664 on the MCP ingest
// tool: a valid reason reaches the service, absent means none, and an invalid
// one is an argument error (not an indeterminate mutating failure) that never
// reaches the service.
func TestIngestToolCarriesDeclassificationReason(t *testing.T) {
	service := &reasonRecordingService{allOptionalService: &allOptionalService{
		stubService: &stubService{}, calls: map[string]int{},
	}}
	server, _ := newTestServer(t, service, nil)
	makeReady(t, server)
	const file = `{"name":"notes.txt","content":"aGk="}`

	response := callToolRequest(t, server, ToolIngest,
		`{"files":[`+file+`],"declassification_reason":"ticket:SEC-1"}`)
	if result := decodeToolResult(t, response); response.Error != nil || result.IsError {
		t.Fatalf("ingest with a reason = %+v / %+v", response.Error, result)
	}
	response = callToolRequest(t, server, ToolIngest, `{"files":[`+file+`]}`)
	if result := decodeToolResult(t, response); response.Error != nil || result.IsError {
		t.Fatalf("ingest without a reason = %+v / %+v", response.Error, result)
	}
	if len(service.requests) != 2 ||
		service.requests[0].DeclassificationReason != "ticket:SEC-1" ||
		service.requests[1].DeclassificationReason != "" {
		t.Fatalf("service requests = %+v", service.requests)
	}

	for _, reason := range []string{
		"has space", "semi;colon", "café",
		strings.Repeat("a", authorized.MaxDeclassificationReasonBytes+1),
	} {
		encoded, err := json.Marshal(reason)
		if err != nil {
			t.Fatal(err)
		}
		response := callToolRequest(t, server, ToolIngest,
			`{"files":[`+file+`],"declassification_reason":`+string(encoded)+`}`)
		if response.Error != nil {
			t.Fatalf("reason %q became a protocol error: %+v", reason, response.Error)
		}
		result := decodeToolResult(t, response)
		var failure structuredToolFailure
		if err := json.Unmarshal(result.StructuredContent, &failure); err != nil {
			t.Fatal(err)
		}
		if !result.IsError ||
			failure.Error.Code != string(shoal.ErrorInvalidArgument) ||
			failure.Error.Message != ToolIngest+" arguments are invalid" {
			t.Fatalf("reason %q = %+v, want the stable argument error", reason, failure)
		}
	}
	if len(service.requests) != 2 {
		t.Fatalf("an invalid reason reached the service: %+v", service.requests)
	}
}

// TestIngestSchemaReasonBoundIsTheAuthorizedBound pins the advertised
// maxLength to the bound the authorized client enforces, so the two cannot
// drift apart.
func TestIngestSchemaReasonBoundIsTheAuthorizedBound(t *testing.T) {
	var schema struct {
		Properties struct {
			Reason struct {
				MaxLength int `json:"maxLength"`
				MinLength int `json:"minLength"`
			} `json:"declassification_reason"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(ingestSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties.Reason.MaxLength != authorized.MaxDeclassificationReasonBytes ||
		schema.Properties.Reason.MinLength != 1 {
		t.Fatalf("declassification_reason schema = %+v", schema.Properties.Reason)
	}
}
