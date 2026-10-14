// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package interaction

import "testing"

// TestARecordedRestrictionMayOnlyBeTranslated pins what a trusted sink may do
// to a session's output restriction (#564): replace a free-form label with
// structured grant labels, and nothing else.
func TestARecordedRestrictionMayOnlyBeTranslated(t *testing.T) {
	const grant = "g:abc:e:1"
	for _, probe := range []struct {
		name                string
		requested, recorded []string
		accepted            bool
	}{
		{"unchanged", []string{"secret"}, []string{"secret"}, true},
		{"unlabelled", nil, nil, true},
		{"free-form translated", []string{"secret"}, []string{"d:aa", grant, "s:bb"}, true},
		{"structured kept, free-form translated",
			[]string{"secret", "s:bb"}, []string{grant, "s:bb"}, true},
		{"free-form dropped", []string{"secret"}, nil, false},
		{"structured dropped", []string{grant}, nil, false},
		{"structured replaced", []string{grant}, []string{"g:other:e:1"}, false},
		{"free-form added", nil, []string{"secret"}, false},
		{"free-form swapped", []string{"secret"}, []string{"public"}, false},
	} {
		t.Run(probe.name, func(t *testing.T) {
			if got := translatedRestriction(probe.requested, probe.recorded); got != probe.accepted {
				t.Fatalf("accepted = %v, want %v", got, probe.accepted)
			}
		})
	}
}
