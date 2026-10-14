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

package authorized

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func documentViewNodeIDs(view explorer.DocumentView) ([]shoal.ID, error) {
	if err := view.Document.Validate(); err != nil {
		return nil, err
	}
	if err := view.Revision.Validate(); err != nil {
		return nil, err
	}
	if view.Document.RevisionID != view.Revision.ID ||
		view.Revision.DocumentID != view.Document.ID ||
		view.Root.Section.ID != view.Document.RootSectionID {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "document view ownership is inconsistent")
	}
	nodeIDs := []shoal.ID{view.Document.ID}
	seen := map[shoal.ID]struct{}{view.Document.ID: {}}
	var visit func(explorer.SectionView, shoal.ID) error
	visit = func(sectionView explorer.SectionView, parentID shoal.ID) error {
		section := sectionView.Section
		if err := section.Validate(); err != nil {
			return err
		}
		if section.DocumentID != view.Document.ID ||
			section.RevisionID != view.Revision.ID ||
			section.ParentID != parentID {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "document section ownership is inconsistent")
		}
		if _, duplicate := seen[section.ID]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "document view reuses a graph node identity")
		}
		seen[section.ID] = struct{}{}
		nodeIDs = append(nodeIDs, section.ID)
		for _, span := range sectionView.Spans {
			if err := span.Validate(); err != nil {
				return err
			}
			if span.DocumentID != view.Document.ID ||
				span.RevisionID != view.Revision.ID ||
				span.SectionID != section.ID {
				return shoal.NewError(
					shoal.ErrorInvalidArgument, "document span ownership is inconsistent")
			}
			if _, duplicate := seen[span.ID]; duplicate {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"document view reuses a graph node identity",
				)
			}
			seen[span.ID] = struct{}{}
			nodeIDs = append(nodeIDs, span.ID)
		}
		for _, child := range sectionView.Children {
			if err := visit(child, section.ID); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(view.Root, ""); err != nil {
		return nil, err
	}
	sort.Slice(nodeIDs, func(left, right int) bool {
		return shoal.CompareID(nodeIDs[left], nodeIDs[right]) < 0
	})
	return nodeIDs, nil
}

func documentViewDigest(view explorer.DocumentView) (auth.Digest, error) {
	return documentViewDigestV2(view)
}

func documentViewDigestV2(view explorer.DocumentView) (auth.Digest, error) {
	if _, err := documentViewNodeIDs(view); err != nil {
		return auth.Digest{}, err
	}
	encoder := newViewDigestEncoder()
	encoder.text("explorer-authorized-document-view-v2")
	encoder.document(view.Document)
	encoder.revision(view.Revision)
	encoder.text(view.SourceURI)
	encoder.text(view.SourceMediaType)
	encoder.sectionView(view.Root)
	var digest auth.Digest
	copy(digest[:], encoder.hash.Sum(nil))
	return digest, nil
}

func legacyDocumentViewDigestV1(view explorer.DocumentView) (auth.Digest, error) {
	if _, err := documentViewNodeIDs(view); err != nil {
		return auth.Digest{}, err
	}
	encoder := newViewDigestEncoder()
	encoder.text("explorer-authorized-document-view-v1")
	encoder.document(view.Document)
	encoder.revision(view.Revision)
	encoder.text(view.SourceURI)
	encoder.sectionView(view.Root)
	var digest auth.Digest
	copy(digest[:], encoder.hash.Sum(nil))
	return digest, nil
}

type viewDigestEncoder struct {
	hash hash.Hash
}

func newViewDigestEncoder() *viewDigestEncoder {
	return &viewDigestEncoder{hash: sha256.New()}
}

func (e *viewDigestEncoder) bytes(value []byte) {
	e.uint64(uint64(len(value)))
	_, _ = e.hash.Write(value)
}

func (e *viewDigestEncoder) text(value string) {
	e.bytes([]byte(value))
}

func (e *viewDigestEncoder) uint64(value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = e.hash.Write(encoded[:])
}

func (e *viewDigestEncoder) int64(value int64) {
	e.uint64(uint64(value))
}

func (e *viewDigestEncoder) time(value time.Time) {
	if value.IsZero() {
		e.text("")
		return
	}
	e.text(value.UTC().Format(time.RFC3339Nano))
}

func (e *viewDigestEncoder) metadata(metadata shoal.Metadata) {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		return bytes.Compare([]byte(keys[left]), []byte(keys[right])) < 0
	})
	e.uint64(uint64(len(keys)))
	for _, key := range keys {
		e.text(key)
		e.text(metadata[key])
	}
}

func (e *viewDigestEncoder) sourceRange(value document.SourceRange) {
	e.int64(value.Start.Offset)
	e.int64(int64(value.Start.Page))
	e.int64(value.End.Offset)
	e.int64(int64(value.End.Page))
}

func (e *viewDigestEncoder) document(value document.Document) {
	e.text(string(value.ID))
	e.text(string(value.RevisionID))
	e.text(value.Title)
	e.text(string(value.RootSectionID))
	e.metadata(value.Metadata)
}

func (e *viewDigestEncoder) revision(value document.Revision) {
	e.text(string(value.ID))
	e.text(string(value.DocumentID))
	e.time(value.CreatedAt)
	e.text(value.SourceVersion)
	e.metadata(value.Metadata)
}

func (e *viewDigestEncoder) section(value document.Section) {
	e.text(string(value.ID))
	e.text(string(value.DocumentID))
	e.text(string(value.RevisionID))
	e.text(string(value.ParentID))
	e.uint64(uint64(value.Order))
	e.text(value.Heading)
	e.sourceRange(value.Range)
	e.metadata(value.Metadata)
}

func (e *viewDigestEncoder) span(value document.Span) {
	e.text(string(value.ID))
	e.text(string(value.DocumentID))
	e.text(string(value.RevisionID))
	e.text(string(value.SectionID))
	e.uint64(uint64(value.Order))
	e.sourceRange(value.Range)
	e.text(value.Text)
	e.metadata(value.Metadata)
}

func (e *viewDigestEncoder) sectionView(value explorer.SectionView) {
	e.section(value.Section)
	e.uint64(uint64(len(value.Spans)))
	for _, span := range value.Spans {
		e.span(span)
	}
	e.uint64(uint64(len(value.Children)))
	for _, child := range value.Children {
		e.sectionView(child)
	}
}
