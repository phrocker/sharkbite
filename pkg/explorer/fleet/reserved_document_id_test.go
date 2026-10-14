// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"strings"
	"testing"
)

// TestTheDocumentIDShapeIsReservedOnlyForNewNames (#669): a new action or
// approval may not be named with an explorer document ID, but a record
// already stored at one still validates, so it stays readable and can still
// be driven.
func TestTheDocumentIDShapeIsReservedOnlyForNewNames(t *testing.T) {
	document := []byte("doc_" + strings.Repeat("0123456789abcdef", 2))
	if !reservedActionID(document) {
		t.Fatalf("%q is not reserved", document)
	}
	for _, id := range [][]byte{
		[]byte("doc_" + strings.Repeat("0123456789abcdef", 2) + "0"),
		[]byte("doc_release"), []byte("action-1"),
	} {
		if reservedActionID(id) {
			t.Errorf("%q is reserved", id)
		}
	}
	if err := validateOpaque("action ID", document, false); err != nil {
		t.Fatalf("a stored action ID of the document shape no longer validates: %v", err)
	}
	if reservedAdmissionID(document) {
		t.Fatal("record validation would refuse a stored document-shaped ID")
	}
}
