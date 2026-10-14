package visfilter

import (
	"bytes"
	"testing"
)

func TestEvaluator_EmptyCVAlwaysVisible(t *testing.T) {
	e := NewEvaluator(NewAuthorizations())
	if !e.Visible(nil) {
		t.Errorf("nil CV should always be visible")
	}
	if !e.Visible([]byte{}) {
		t.Errorf("empty CV should always be visible")
	}
}

func TestEvaluator_SimpleLabel(t *testing.T) {
	auths := NewAuthorizations([]byte("A"), []byte("B"))
	e := NewEvaluator(auths)
	cases := []struct {
		cv   string
		want bool
	}{
		{"A", true},
		{"B", true},
		{"C", false},
	}
	for _, tc := range cases {
		if got := e.Visible([]byte(tc.cv)); got != tc.want {
			t.Errorf("Visible(%q) = %v; want %v", tc.cv, got, tc.want)
		}
	}
}

func TestEvaluator_AndOr(t *testing.T) {
	auths := NewAuthorizations([]byte("A"), []byte("B"))
	e := NewEvaluator(auths)
	cases := []struct {
		cv   string
		want bool
	}{
		{"A&B", true},
		{"A&C", false},
		{"A|C", true},
		{"C|D", false},
		{"(A&B)|C", true},
		{"(A&C)|B", true},
		{"(A&C)|D", false},
		{"A|B|C", true},
		{"A&B&C", false},
	}
	for _, tc := range cases {
		if got := e.Visible([]byte(tc.cv)); got != tc.want {
			t.Errorf("Visible(%q) = %v; want %v", tc.cv, got, tc.want)
		}
	}
}

func TestEvaluator_Cache(t *testing.T) {
	auths := NewAuthorizations([]byte("A"), []byte("B"))
	e := NewEvaluator(auths)
	for i := 0; i < 1000; i++ {
		_ = e.Visible([]byte("A&B"))
	}
	if e.CacheSize() != 1 {
		t.Errorf("CacheSize after 1000 same-CV calls = %d; want 1", e.CacheSize())
	}
	_ = e.Visible([]byte("A|B"))
	_ = e.Visible([]byte("C"))
	if e.CacheSize() != 3 {
		t.Errorf("CacheSize after 3 distinct CVs = %d; want 3", e.CacheSize())
	}
}

func TestEvaluator_MalformedCachedAsFailure(t *testing.T) {
	e := NewEvaluator(NewAuthorizations([]byte("A")))
	bad := []byte("A&")
	if e.Visible(bad) {
		t.Errorf("malformed CV should be invisible")
	}
	// Re-evaluate; should still be invisible AND should hit the cache.
	if e.Visible(bad) {
		t.Errorf("malformed CV (cached) should remain invisible")
	}
	if e.CacheSize() != 1 {
		t.Errorf("malformed CV cached %d entries; want 1", e.CacheSize())
	}
}

func TestEvaluator_LabelByteSet(t *testing.T) {
	auths := NewAuthorizations([]byte("user.role:admin"), []byte("group_x/y-z"))
	e := NewEvaluator(auths)
	if !e.Visible([]byte("user.role:admin")) {
		t.Errorf("dotted/colon label not accepted")
	}
	if !e.Visible([]byte("group_x/y-z")) {
		t.Errorf("underscore/slash/hyphen label not accepted")
	}
}

func TestEvaluator_AllocBehaviour(t *testing.T) {
	// First call allocs (parse + cache key + tree); subsequent calls
	// for the same CV bytes should be alloc-free thanks to the
	// `m[string(b)]` lookup optimization.
	auths := NewAuthorizations([]byte("A"), []byte("B"))
	e := NewEvaluator(auths)
	cv := []byte("A&B")
	// Warm.
	_ = e.Visible(cv)

	allocs := testing.AllocsPerRun(1000, func() {
		_ = e.Visible(cv)
	})
	if allocs > 0 {
		t.Errorf("warm-cache Visible allocates %.2f times/op; want 0", allocs)
	}
}

func TestAuthorizations_HasLabel(t *testing.T) {
	a := NewAuthorizations([]byte("X"), []byte("Y"))
	if !a.HasLabel([]byte("X")) {
		t.Errorf("X should be present")
	}
	if a.HasLabel([]byte("Z")) {
		t.Errorf("Z should be absent")
	}
}

func TestEvaluator_TransientCV(t *testing.T) {
	// Mirror the relkey lifetime: the CV slice we pass in might point
	// into a buffer that gets overwritten between calls. The evaluator
	// must copy what it needs.
	e := NewEvaluator(NewAuthorizations([]byte("A")))
	buf := []byte("Aabcd")
	cv := buf[:1] // points into buf
	if !e.Visible(cv) {
		t.Errorf("first call should be visible")
	}
	// Mutate the source buffer.
	for i := range buf {
		buf[i] = 'Z'
	}
	// Second call with a fresh "A" CV — cache should hit on the
	// previously-stored copy and still return true.
	stable := []byte{'A'}
	if !e.Visible(stable) {
		t.Errorf("post-mutation A should still be visible (cache stores its own copy)")
	}
	if e.CacheSize() != 1 {
		t.Errorf("CacheSize = %d; want 1", e.CacheSize())
	}
	_ = bytes.Equal // keep import
}
