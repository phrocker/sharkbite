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
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// refusingExplorer is the real Explorer, except that an ingest of refused
// content is the authorized client's historical refusal, marked as leaving
// nothing committed.
type refusingExplorer struct {
	*explorer.Explorer
	refused string
}

func (c *refusingExplorer) Ingest(
	ctx context.Context, source explorer.Source,
) (explorer.IngestResult, error) {
	if source.Content == "bad\n" {
		return explorer.IngestResult{}, shoal.NewError(shoal.ErrorNotFound, "injected")
	}
	if source.Content == c.refused {
		return explorer.IngestResult{}, explorer.MarkRefusedWithoutEffect(
			shoal.WrapError(shoal.ErrorConflict, "refused",
				authorized.ErrHistoricalRevision))
	}
	return c.Explorer.Ingest(ctx, source)
}

// TestIngestToolReportsPerFileOutcomes pins #687 on the MCP ingest tool: a
// batch that did not fully succeed is still a tool error and lists each
// file's outcome. When every file was tried and has a known outcome it keeps
// its definite code, partial or not; a batch stopped by a failure stays
// indeterminate, like every other post-dispatch failure.
func TestIngestToolReportsPerFileOutcomes(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	service, err := webapi.NewEmbeddedService(
		&refusingExplorer{Explorer: corpus, refused: "old\n"})
	if err != nil {
		t.Fatal(err)
	}
	server, _ := newTestServer(t, service, nil)
	makeReady(t, server)
	file := func(name, content string) string {
		return `{"name":"` + name + `","content":"` +
			base64.StdEncoding.EncodeToString([]byte(content)) + `"}`
	}
	refused := webapi.IngestFileOutcome{
		Name: "old.txt", Status: webapi.IngestFileRefused,
		Reason: webapi.IngestRefusedHistoricalRevision,
		Message: `upload file "old.txt" matches an earlier revision of its ` +
			`document, so the document was left unchanged; change the content to upload it`,
	}
	for _, test := range []struct {
		name  string
		files string
		code  string
		want  []webapi.IngestFileOutcome
	}{
		{"refused only", file("old.txt", "old\n"), string(shoal.ErrorConflict),
			[]webapi.IngestFileOutcome{refused}},
		{"refused and applied", file("old.txt", "old\n") + "," + file("new.txt", "new\n"),
			string(shoal.ErrorConflict), []webapi.IngestFileOutcome{refused,
				{Name: "new.txt", Status: webapi.IngestFileApplied}}},
		{"stopped at the last file", file("old.txt", "old\n") + "," + file("bad.txt", "bad\n"),
			"indeterminate", []webapi.IngestFileOutcome{refused,
				{Name: "bad.txt", Status: webapi.IngestFileFailed}}},
		{"stopped", file("old.txt", "old\n") + "," + file("bad.txt", "bad\n") + "," +
			file("later.txt", "later\n"),
			"indeterminate", []webapi.IngestFileOutcome{refused,
				{Name: "bad.txt", Status: webapi.IngestFileFailed},
				{Name: "later.txt", Status: webapi.IngestFileNotAttempted}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := callToolRequest(t, server, ToolIngest, `{"files":[`+test.files+`]}`)
			if response.Error != nil {
				t.Fatalf("protocol error: %+v", response.Error)
			}
			result := decodeToolResult(t, response)
			var failure structuredToolFailure
			if err := json.Unmarshal(result.StructuredContent, &failure); err != nil {
				t.Fatal(err)
			}
			if !result.IsError || failure.Error.Code != test.code ||
				!reflect.DeepEqual(failure.Error.Files, test.want) {
				t.Fatalf("tool failure = %+v, want code %q and files %+v",
					failure, test.code, test.want)
			}
		})
	}
}
