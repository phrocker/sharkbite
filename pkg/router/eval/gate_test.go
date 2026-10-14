// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package eval

import (
	"errors"
	"testing"
)

// TestTestSplitOnlyThroughTheGate: Split refuses the held-out split, and
// PreRegistered releases it only under the registered digest.
func TestTestSplitOnlyThroughTheGate(t *testing.T) {
	_, cases := load(t)
	for _, split := range []string{"test", "TEST", "", "other"} {
		if got, err := cases.Split(split); !errors.Is(err, ErrHeldOut) || got != nil {
			t.Fatalf("Split(%q) = %d cases, %v", split, len(got), err)
		}
	}
	if cases.Digest("test") != PreRegisteredTestDigest {
		t.Fatalf("test split digest %s is not the registered one", cases.Digest("test"))
	}
	released, err := cases.PreRegistered("test")
	if err != nil || len(released) != 103 {
		t.Fatalf("gate = %d cases, %v", len(released), err)
	}
	// A different test split is refused.
	tampered := &Cases{all: append([]Case(nil), cases.all...)}
	for i := range tampered.all {
		if tampered.all[i].Split == "test" {
			tampered.all[i].line = append([]byte(nil), tampered.all[i].line...)
			tampered.all[i].line[0] = ' '
			break
		}
	}
	if _, err := tampered.PreRegistered("test"); err == nil {
		t.Fatal("a test split with another digest was released")
	}
}
