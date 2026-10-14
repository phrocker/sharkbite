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

// Package qwal implements shoal's reader for in-flight (pre-flush) WAL
// segments served by the per-pod wal-quorum-sidecar over gRPC.
//
// quorum.go is a faithful Go port of QuorumWalLogCloser.evaluatePeerAgreement
// (server/pvc-fs/.../QuorumWalLogCloser.java). It is pure decision logic over
// a set of peer views — no I/O — so shoal independently picks the same
// quorum-truncated segment length the manager's recovery path does. Keeping
// the two implementations in lockstep is what stops shoal from serving a
// different fresh-write view than the tserver replays during recovery.
package qwal

// AgreementOutcome classifies a set of per-peer segment views. Mirrors the
// Java enum of the same name; the doc comments are carried over verbatim
// because the contract — not the wording — is what must stay identical.
type AgreementOutcome int

const (
	// SealedAgreed: any sealed peer is at the quorum-agreed size. Bytes are
	// durable, sort-ready.
	SealedAgreed AgreementOutcome = iota
	// ReadyToSeal: no sealed peer; all unsealed peers agree on size.
	ReadyToSeal
	// QuorumTruncate: no sealed peer; a quorum (floor(N/2)+1) of unsealed
	// peers agree on a size and the minority disagrees. The minority's
	// surplus bytes are uncommitted writes (the quorum never acked them);
	// seal/read at the quorum-agreed size.
	QuorumTruncate
	// SealedDivergence: a sealed peer's size disagrees with the quorum, or
	// two sealed peers disagree with each other. Committed bytes diverge and
	// there is no safe automatic resolution. Operator intervention required.
	SealedDivergence
	// UnsealedDivergence: no size has quorum support. Could be a transient
	// mid-write split. Retry rather than guess.
	UnsealedDivergence
	// NoPeers: no peer holds the segment at all.
	NoPeers
)

// String renders the outcome for logs.
func (o AgreementOutcome) String() string {
	switch o {
	case SealedAgreed:
		return "SEALED_AGREED"
	case ReadyToSeal:
		return "READY_TO_SEAL"
	case QuorumTruncate:
		return "QUORUM_TRUNCATE"
	case SealedDivergence:
		return "SEALED_DIVERGENCE"
	case UnsealedDivergence:
		return "UNSEALED_DIVERGENCE"
	case NoPeers:
		return "NO_PEERS"
	default:
		return "UNKNOWN"
	}
}

// PeerView is one peer's view of a WAL segment, captured at read-time. Used
// by the cross-peer size-agreement check before deciding which replica (and
// at what length) to stream.
type PeerView struct {
	// Addr is the "host:port" of the peer sidecar that reported this view.
	Addr string
	// Size is the byte length the peer holds for the segment.
	Size int64
	// Sealed reports whether the peer has sealed (finalized) its replica.
	Sealed bool
}

// AgreementResult bundles the outcome of evaluatePeerAgreement. AgreedSize is
// the quorum-agreed length on success; on divergent outcomes it is -1.
// MinSealedSize/MaxSealedSize are populated only for SealedDivergence (and
// otherwise -1) to mirror the Java struct.
type AgreementResult struct {
	Outcome       AgreementOutcome
	AgreedSize    int64
	MinSealedSize int64
	MaxSealedSize int64
}

// EvaluatePeerAgreement is the pure decision logic over peer views — a
// direct port of QuorumWalLogCloser.evaluatePeerAgreement. No I/O, no gRPC.
//
// Contract (unchanged from Java):
//   - Multiple sealed peers at different sizes -> SealedDivergence.
//   - Sealed peer at a non-quorum size -> SealedDivergence.
//   - Quorum exists, includes a sealed peer at the quorum size -> SealedAgreed.
//   - Quorum exists, no sealed peers, all peers agree -> ReadyToSeal.
//   - Quorum exists, no sealed peers, minority disagrees -> QuorumTruncate.
//   - No quorum at any size -> UnsealedDivergence.
//   - Empty peer set -> NoPeers.
func EvaluatePeerAgreement(peers []PeerView) AgreementResult {
	if len(peers) == 0 {
		return AgreementResult{Outcome: NoPeers, AgreedSize: -1, MinSealedSize: -1, MaxSealedSize: -1}
	}

	sizeCounts := make(map[int64]int)
	anySealed := false
	var maxSealedSize int64 = -1
	var minSealedSize int64 = 1<<63 - 1
	for _, v := range peers {
		sizeCounts[v.Size]++
		if v.Sealed {
			anySealed = true
			if v.Size > maxSealedSize {
				maxSealedSize = v.Size
			}
			if v.Size < minSealedSize {
				minSealedSize = v.Size
			}
		}
	}

	// Two sealed peers at different sizes — both committed, no safe pick.
	if anySealed && maxSealedSize != minSealedSize {
		return AgreementResult{Outcome: SealedDivergence, AgreedSize: -1,
			MinSealedSize: minSealedSize, MaxSealedSize: maxSealedSize}
	}

	// Quorum threshold: floor(N/2)+1. With 3 peers that's 2; with 5 that's 3.
	// An even N=4 50/50 split fails to reach quorum.
	quorum := len(peers)/2 + 1
	var quorumSize int64 = -1
	quorumFound := false
	for size, count := range sizeCounts {
		// Mathematically only one size can exceed N/2, so iteration order
		// doesn't affect correctness — the first match is the only match.
		if count >= quorum {
			quorumSize = size
			quorumFound = true
			break
		}
	}

	if !quorumFound {
		return AgreementResult{Outcome: UnsealedDivergence, AgreedSize: -1, MinSealedSize: -1, MaxSealedSize: -1}
	}

	allAgree := len(sizeCounts) == 1

	if anySealed {
		// Sealed peers all agree (else we'd have returned SealedDivergence).
		// If that single sealed size IS the quorum size, the segment is
		// sort-ready.
		if maxSealedSize == quorumSize {
			return AgreementResult{Outcome: SealedAgreed, AgreedSize: quorumSize,
				MinSealedSize: quorumSize, MaxSealedSize: quorumSize}
		}
		// Sealed peer is not at the quorum size — sealed bytes diverge from
		// the majority view. No safe automatic resolution.
		return AgreementResult{Outcome: SealedDivergence, AgreedSize: -1,
			MinSealedSize: maxSealedSize, MaxSealedSize: maxSealedSize}
	}

	if allAgree {
		return AgreementResult{Outcome: ReadyToSeal, AgreedSize: quorumSize, MinSealedSize: -1, MaxSealedSize: -1}
	}
	return AgreementResult{Outcome: QuorumTruncate, AgreedSize: quorumSize, MinSealedSize: -1, MaxSealedSize: -1}
}

// Retryable reports whether the outcome is one a caller should retry later
// (transient divergence) rather than treat as a hard, operator-only error.
func (r AgreementResult) Retryable() bool {
	return r.Outcome == UnsealedDivergence
}
