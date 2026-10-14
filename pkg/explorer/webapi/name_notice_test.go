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

package webapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestUploadNameNoticeIsSentOnlyForUnchangedDocuments pins when a file's
// name notice is sent (#712): only when other documents of the name the
// caller may upload to were left unchanged, so other replies stay readable
// by clients built before it, and that it survives the wire both ways.
func TestUploadNameNoticeIsSentOnlyForUnchangedDocuments(t *testing.T) {
	for _, route := range []authorized.UploadRoute{
		{},
		{Kind: authorized.UploadRouteOwn, SourceURI: "upload://workspace/u-x/a.md"},
		{Kind: authorized.UploadRouteShared, SourceURI: "upload://workspace/u-y/a.md"},
	} {
		if notice := uploadNameNotice(route); notice != nil {
			t.Fatalf("route %+v got notice %+v", route, notice)
		}
	}
	own := uploadNameNotice(authorized.UploadRoute{
		Kind: authorized.UploadRouteOwn, SourceURI: "upload://workspace/u-x/a.md",
		OtherDocuments: 2, Ambiguous: true,
	})
	legacy := uploadNameNotice(authorized.UploadRoute{
		Kind: authorized.UploadRouteLegacy, SourceURI: "upload://workspace/a.md",
		OtherDocuments: 1,
	})
	if own == nil || own.Route != "own" || !own.Ambiguous || own.OtherDocuments != 2 ||
		!strings.Contains(own.Message, "2 documents of other uploaders") {
		t.Fatalf("ambiguous notice = %+v", own)
	}
	if legacy == nil || legacy.Route != "legacy" || legacy.Ambiguous ||
		!strings.Contains(legacy.Message, "before uploads were kept per uploader") {
		t.Fatalf("legacy notice = %+v", legacy)
	}

	file := func(name string, notice *IngestNameNotice) IngestFileResult {
		id := shoal.ID("doc_" + name)
		revision := shoal.ID("rev_" + name)
		return IngestFileResult{
			Name: name, Disposition: explorer.IngestApplied,
			Document: document.Document{ID: id, RevisionID: revision, Title: name,
				RootSectionID: shoal.ID("section_" + name)},
			Revision:   document.Revision{ID: revision, DocumentID: id, CreatedAt: time.Unix(1, 0).UTC()},
			NameNotice: notice,
		}
	}
	response := IngestResponse{Files: []IngestFileResult{file("a.md", own), file("b.md", nil)}}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(encoded), `"name_notice"`) != 1 {
		t.Fatalf("encoded = %s, want one name_notice", encoded)
	}
	var decoded IngestResponse
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Files[0].NameNotice, own) || decoded.Files[1].NameNotice != nil {
		t.Fatalf("decoded notices = %+v, %+v", decoded.Files[0].NameNotice, decoded.Files[1].NameNotice)
	}
}
