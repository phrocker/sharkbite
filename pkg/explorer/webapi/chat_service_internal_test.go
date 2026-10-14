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
	"testing"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestCitationDocumentKeyPreservesOpaqueIdentityBoundaries(t *testing.T) {
	first := citationDocumentKey{
		documentID: shoal.ID([]byte{'a', 0, 0xff}),
		revisionID: "b",
	}
	second := citationDocumentKey{
		documentID: shoal.ID([]byte{'a', 0}),
		revisionID: shoal.ID([]byte{0xff, 0, 'b'}),
	}
	if first == second {
		t.Fatal("opaque citation identity pairs collided")
	}
	links := map[citationDocumentKey]string{
		first:  "https://example.test/first",
		second: "https://example.test/second",
	}
	if links[first] != "https://example.test/first" ||
		links[second] != "https://example.test/second" {
		t.Fatalf("opaque citation links were conflated: %v", links)
	}
}
