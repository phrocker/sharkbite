// Licensed under the Apache Software Foundation, Version 2.0. See LICENSE and NOTICE.

package effectsgateway

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"net/http"
	"net/textproto"
	"sort"
)

// digestPart frames every field, so different paths through the request
// cannot produce the same byte stream. The credential is attached only after
// this commitment and is intentionally excluded.
func digestPart(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}

func boundRequestDigest(request BoundRequest) [32]byte {
	h := sha256.New()
	digestPart(h, []byte("sharkbite.gateway.request.v1"))
	digestPart(h, []byte(request.Method))
	digestPart(h, []byte(request.URL))
	keys := make([]string, 0, len(request.Header))
	for key := range request.Header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	canonical := make(http.Header)
	for _, key := range keys {
		canonical[textproto.CanonicalMIMEHeaderKey(key)] = append(
			canonical[textproto.CanonicalMIMEHeaderKey(key)], request.Header[key]...)
	}
	keys = keys[:0]
	for key := range canonical {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(keys)))
	_, _ = h.Write(count[:])
	for _, key := range keys {
		digestPart(h, []byte(key))
		values := canonical[key]
		binary.BigEndian.PutUint64(count[:], uint64(len(values)))
		_, _ = h.Write(count[:])
		for _, value := range values {
			digestPart(h, []byte(value))
		}
	}
	digestPart(h, request.Body)
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func targetScopeDigest(scope string) [32]byte {
	h := sha256.New()
	digestPart(h, []byte("sharkbite.gateway.target-scope.v1"))
	digestPart(h, []byte(scope))
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

// RecoveryDigests computes the versioned request and target-account digests
// submitted to Prepare for a request produced by Binder. The target
// credential is attached only after this step.
func RecoveryDigests(request BoundRequest, scope string) ([32]byte, [32]byte) {
	return boundRequestDigest(request), targetScopeDigest(scope)
}
