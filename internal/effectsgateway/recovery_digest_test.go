// Licensed under the Apache Software Foundation, Version 2.0. See LICENSE and NOTICE.

package effectsgateway

import (
	"net/http"
	"testing"
)

func TestBoundRequestDigestFramesExactRequest(t *testing.T) {
	base := BoundRequest{Method: "POST", URL: "https://api.example.test/a?x=1",
		Header: http.Header{"Idempotency-Key": {"key"}, "Content-Type": {"application/json"}},
		Body:   []byte{0, 255, 1}}
	original := boundRequestDigest(base)
	if original == ([32]byte{}) {
		t.Fatal("empty request digest")
	}
	reordered := base
	reordered.Header = http.Header{"content-type": {"application/json"}, "idempotency-key": {"key"}}
	if got := boundRequestDigest(reordered); got != original {
		t.Fatal("header insertion order or spelling changed digest")
	}
	cases := []struct {
		name   string
		change func(*BoundRequest)
	}{
		{"method", func(r *BoundRequest) { r.Method = "PUT" }},
		{"url", func(r *BoundRequest) { r.URL = "https://api.example.test/a?x=2" }},
		{"header", func(r *BoundRequest) {
			r.Header = http.Header{"Idempotency-Key": {"other"}, "Content-Type": {"application/json"}}
		}},
		{"body", func(r *BoundRequest) { r.Body = []byte{0, 255, 2} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.change(&changed)
			if boundRequestDigest(changed) == original {
				t.Fatal("changed request retained digest")
			}
		})
	}
}

func TestTargetScopeDigestBindsAccount(t *testing.T) {
	if targetScopeDigest("account-1") == targetScopeDigest("account-2") {
		t.Fatal("target account rotation retained digest")
	}
}
