// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package explorerfleetcap

import "testing"

func TestCapabilityRequiresExactAuthority(t *testing.T) {
	first := New()
	second := New()
	if !first.Valid() || !first.Matches(first) {
		t.Fatal("minted capability is invalid")
	}
	if first.Matches(second) ||
		first.Matches(Capability{}) ||
		(Capability{}).Matches(first) {
		t.Fatal("distinct or zero capability matched")
	}
}
