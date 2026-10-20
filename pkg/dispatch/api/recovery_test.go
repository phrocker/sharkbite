// Licensed under the Apache Software Foundation, Version 2.0. See LICENSE and NOTICE.

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestPrepareRefusesReceiptForDifferentBoundRequest(t *testing.T) {
	request := RecoveryPrepare{
		RecoveryClaim: RecoveryClaim{Context: testContext(), ClaimID: []byte("claim"), ClaimFence: 1},
		Mode:          "key", RequestDigest: [32]byte{1}, TargetScopeDigest: [32]byte{2},
	}
	answer := PrepareReceipt{ID: EncodeID([]byte("action")), ClaimID: EncodeID(request.ClaimID),
		ClaimFence: 1, Version: 3, Committed: true, Mode: "key",
		RequestDigest:     base64.RawURLEncoding.EncodeToString(request.RequestDigest[:]),
		TargetScopeDigest: base64.RawURLEncoding.EncodeToString(request.TargetScopeDigest[:])}
	for _, changed := range []string{"mode", "request", "scope"} {
		t.Run(changed, func(t *testing.T) {
			stale := answer
			switch changed {
			case "mode":
				stale.Mode = "natural"
			case "request":
				stale.RequestDigest = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			case "scope":
				stale.TargetScopeDigest = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			}
			body, _ := json.Marshal(stale)
			client, _ := testClient(t, respond(200, string(body)))
			if _, err := client.Prepare(context.Background(), []byte("action"), request); Kind(err) != KindProtocol {
				t.Fatalf("substituted receipt = %v, want protocol refusal", err)
			}
		})
	}
	body, _ := json.Marshal(answer)
	client, _ := testClient(t, respond(200, string(body)))
	if version, err := client.Prepare(context.Background(), []byte("action"), request); err != nil || version != 3 {
		t.Fatalf("matching receipt: version=%d error=%v", version, err)
	}
}
