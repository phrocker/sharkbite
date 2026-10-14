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
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestStableIDIsDeterministicAndCollisionSafe(t *testing.T) {
	first, err := ontology.NewStableID("vendor.entity", "repo", "ab", "c")
	if err != nil {
		t.Fatalf("create stable ID: %v", err)
	}
	again, err := ontology.NewStableID("vendor.entity", "repo", "ab", "c")
	if err != nil {
		t.Fatalf("repeat stable ID: %v", err)
	}
	if first != again {
		t.Fatalf("stable ID changed: %q != %q", first, again)
	}

	differentBoundary, err := ontology.NewStableID("vendor.entity", "repo", "a", "bc")
	if err != nil {
		t.Fatalf("create boundary-safe ID: %v", err)
	}
	differentNamespace, err := ontology.NewStableID("vendor.other", "repo", "ab", "c")
	if err != nil {
		t.Fatalf("create namespaced ID: %v", err)
	}
	if first == differentBoundary || first == differentNamespace {
		t.Fatal("distinct stable identities collided")
	}
	if ontology.IDNamespace(first) != "vendor.entity" {
		t.Fatalf("namespace = %q, want vendor.entity", ontology.IDNamespace(first))
	}
	parsed, err := ontology.ParseID(string(first))
	if err != nil {
		t.Fatalf("parse stable ID: %v", err)
	}
	if parsed != first {
		t.Fatalf("parsed ID = %q, want %q", parsed, first)
	}
}

func TestStableIDRejectsReservedNamespacesAndInvalidWireValues(t *testing.T) {
	for _, namespace := range []string{
		"assertion", "concept", "evidence", "extraction", "ontology-version",
		"property", "proposal", "relationship", "schema",
	} {
		t.Run(namespace, func(t *testing.T) {
			_, err := ontology.NewStableID(namespace, "caller-controlled")
			if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("reserved namespace error = %v", err)
			}
		})
	}

	invalidUTF8 := string([]byte{0xff})
	if _, err := ontology.NewStableID("vendor.entity", invalidUTF8); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("invalid UTF-8 component error = %v", err)
	}
	if _, err := ontology.NewStableID("Vendor", "value"); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("invalid namespace error = %v", err)
	}
	if _, err := ontology.NewStableID("vendor"); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("missing component error = %v", err)
	}

	valid, err := ontology.NewStableID("vendor", "value")
	if err != nil {
		t.Fatal(err)
	}
	upperDigest := strings.ToUpper(string(valid))
	invalidID, err := ontology.ParseID(upperDigest)
	if !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("uppercase canonical ID error = %v", err)
	}
	if ontology.IDNamespace(invalidID) != "" {
		t.Fatalf("invalid ID namespace = %q, want empty", ontology.IDNamespace(invalidID))
	}
}
