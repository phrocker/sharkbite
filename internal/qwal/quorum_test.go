// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// quorum_test.go mirrors QuorumWalLogCloserAgreementTest.java one-for-one so
// the Go port and the Java original stay provably in lockstep. Each test name
// and peer-size shape is carried over from the Java cases (including the
// 2026-05-04 and 2026-05-08 cl-kgun2u incident shapes).
package qwal

import "testing"

func peer(addr string, size int64, sealed bool) PeerView {
	return PeerView{Addr: addr, Size: size, Sealed: sealed}
}

func TestNoPeers_returnsNoPeers(t *testing.T) {
	r := EvaluatePeerAgreement(nil)
	if r.Outcome != NoPeers {
		t.Fatalf("want NoPeers, got %v", r.Outcome)
	}
}

func TestAllUnsealedAgree_readyToSeal(t *testing.T) {
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 13950, false),
		peer("ts1:9710", 13950, false),
		peer("ts2:9710", 13950, false),
	})
	if r.Outcome != ReadyToSeal {
		t.Fatalf("want ReadyToSeal, got %v", r.Outcome)
	}
	if r.AgreedSize != 13950 {
		t.Fatalf("want agreedSize 13950, got %d", r.AgreedSize)
	}
}

func TestAllSealedAgree_sealedAgreed(t *testing.T) {
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 13950, true),
		peer("ts1:9710", 13950, true),
		peer("ts2:9710", 13950, true),
	})
	if r.Outcome != SealedAgreed || r.AgreedSize != 13950 {
		t.Fatalf("want SealedAgreed/13950, got %v/%d", r.Outcome, r.AgreedSize)
	}
}

func TestMixedSealedAndUnsealed_sameSize_sealedAgreed(t *testing.T) {
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 13950, true),
		peer("ts1:9710", 13950, false),
		peer("ts2:9710", 13950, true),
	})
	if r.Outcome != SealedAgreed || r.AgreedSize != 13950 {
		t.Fatalf("want SealedAgreed/13950, got %v/%d", r.Outcome, r.AgreedSize)
	}
}

func TestMixedSealedAndUnsealed_sealedAtQuorumSize_sealedAgreed(t *testing.T) {
	// 2026-05-04 cl-kgun2u shape: two peers at 13950 (one sealed), third at 13568.
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 13950, true),
		peer("ts1:9710", 13950, false),
		peer("ts2:9710", 13568, false),
	})
	if r.Outcome != SealedAgreed || r.AgreedSize != 13950 {
		t.Fatalf("want SealedAgreed/13950, got %v/%d", r.Outcome, r.AgreedSize)
	}
}

func TestQuorumOfUnsealed_oneAhead_quorumTruncate(t *testing.T) {
	// 2026-05-08 cl-kgun2u shape: one peer wrote 358 bytes past the quorum.
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 16151, false),
		peer("ts1:9710", 15793, false),
		peer("ts2:9710", 15793, false),
	})
	if r.Outcome != QuorumTruncate || r.AgreedSize != 15793 {
		t.Fatalf("want QuorumTruncate/15793, got %v/%d", r.Outcome, r.AgreedSize)
	}
}

func TestQuorumOfUnsealed_oneBehind_quorumTruncate(t *testing.T) {
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 15793, false),
		peer("ts1:9710", 16151, false),
		peer("ts2:9710", 16151, false),
	})
	if r.Outcome != QuorumTruncate || r.AgreedSize != 16151 {
		t.Fatalf("want QuorumTruncate/16151, got %v/%d", r.Outcome, r.AgreedSize)
	}
}

func TestNoQuorum_threeDifferentSizes_unsealedDivergence(t *testing.T) {
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 100, false),
		peer("ts1:9710", 200, false),
		peer("ts2:9710", 300, false),
	})
	if r.Outcome != UnsealedDivergence {
		t.Fatalf("want UnsealedDivergence, got %v", r.Outcome)
	}
}

func TestSealedMinority_unsealedQuorum_sealedDivergence(t *testing.T) {
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 13950, true),
		peer("ts1:9710", 13568, false),
		peer("ts2:9710", 13568, false),
	})
	if r.Outcome != SealedDivergence {
		t.Fatalf("want SealedDivergence, got %v", r.Outcome)
	}
}

func TestFivePeerQuorum_threeAgreeTwoMinorities_quorumTruncate(t *testing.T) {
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 1000, false),
		peer("ts1:9710", 1000, false),
		peer("ts2:9710", 1000, false),
		peer("ts3:9710", 1100, false),
		peer("ts4:9710", 900, false),
	})
	if r.Outcome != QuorumTruncate || r.AgreedSize != 1000 {
		t.Fatalf("want QuorumTruncate/1000, got %v/%d", r.Outcome, r.AgreedSize)
	}
}

func TestFourPeerEvenSplit_unsealedDivergence(t *testing.T) {
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 100, false),
		peer("ts1:9710", 100, false),
		peer("ts2:9710", 200, false),
		peer("ts3:9710", 200, false),
	})
	if r.Outcome != UnsealedDivergence {
		t.Fatalf("want UnsealedDivergence, got %v", r.Outcome)
	}
}

func TestMultipleSealedDisagreeOnSize_sealedDivergence(t *testing.T) {
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 13950, true),
		peer("ts1:9710", 13568, true),
		peer("ts2:9710", 13950, false),
	})
	if r.Outcome != SealedDivergence {
		t.Fatalf("want SealedDivergence, got %v", r.Outcome)
	}
	if r.MinSealedSize != 13568 || r.MaxSealedSize != 13950 {
		t.Fatalf("want min/max 13568/13950, got %d/%d", r.MinSealedSize, r.MaxSealedSize)
	}
}

func TestSingleSealedPeer_sealedAgreed(t *testing.T) {
	r := EvaluatePeerAgreement([]PeerView{peer("ts0:9710", 1024, true)})
	if r.Outcome != SealedAgreed || r.AgreedSize != 1024 {
		t.Fatalf("want SealedAgreed/1024, got %v/%d", r.Outcome, r.AgreedSize)
	}
}

func TestAllUnsealedDisagree_unsealedDivergence(t *testing.T) {
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 100, false),
		peer("ts1:9710", 200, false),
	})
	if r.Outcome != UnsealedDivergence {
		t.Fatalf("want UnsealedDivergence, got %v", r.Outcome)
	}
}

func TestZeroSizeAllAgree_readyToSeal(t *testing.T) {
	r := EvaluatePeerAgreement([]PeerView{
		peer("ts0:9710", 0, false),
		peer("ts1:9710", 0, false),
		peer("ts2:9710", 0, false),
	})
	if r.Outcome != ReadyToSeal || r.AgreedSize != 0 {
		t.Fatalf("want ReadyToSeal/0, got %v/%d", r.Outcome, r.AgreedSize)
	}
}
