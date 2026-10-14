// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package routershadow

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/router"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// RecordVersion names the shadow record format.
const RecordVersion = "shoal.router.shadow/v1"

// StageLatency is wall time per stage, in nanoseconds.
type StageLatency struct {
	EnumerateNS int64 `json:"enumerate_ns"`
	ResolveNS   int64 `json:"resolve_ns"`
	AnalyzeNS   int64 `json:"analyze_ns"`
	DecideNS    int64 `json:"decide_ns"`
	BaselineNS  int64 `json:"baseline_ns"`
	TotalNS     int64 `json:"total_ns"`
}

// MentionRecord is where a mention was and which visible nodes it named. It
// holds no text.
type MentionRecord struct {
	TokenSpan lexicon.Span `json:"token_span"`
	NodeIDs   []shoal.ID   `json:"node_ids"`
	Ambiguous bool         `json:"ambiguous,omitempty"`
}

// Record is one shadow routing. It never holds the text or any token of it:
// the utterance is represented only by UtteranceKey, a keyed hash scoped to
// the caller, so the same text from two callers gives unrelated keys and a
// reader without the host key cannot test a guess. LexiconBundleID names the
// server-filtered bundle used; it is host-internal and is not part of the
// proposal or its receipt, because a server-filtered bundle's ID changes with
// nodes the caller cannot see.
type Record struct {
	Version         string          `json:"version"`
	RecordedAt      time.Time       `json:"recorded_at"`
	Principal       shoal.ID        `json:"principal"`
	AuthFingerprint string          `json:"auth_fingerprint"`
	UtteranceKey    string          `json:"utterance_key"`
	TokenCount      int             `json:"token_count"`
	Mentions        []MentionRecord `json:"mentions,omitempty"`
	LexiconBundleID string          `json:"lexicon_bundle_id"`
	Proposal        router.Proposal `json:"proposal"`
	Baseline        router.Proposal `json:"baseline"`
	Agree           bool            `json:"agree"`
	Latency         StageLatency    `json:"latency"`
}

// UtteranceKey is HMAC-SHA256 under the host key over the caller scope
// (principal and authorization fingerprint) and the normalized text (tokens
// joined by one space), each length-prefixed.
func UtteranceKey(hostKey []byte, principal shoal.ID, fingerprint [32]byte, tokens []lexicon.Token) string {
	mac := hmac.New(sha256.New, hostKey)
	field := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		mac.Write(n[:])
		mac.Write(b)
	}
	field([]byte(RecordVersion + "/utterance"))
	field([]byte(principal))
	field(fingerprint[:])
	normalized := make([]byte, 0, 64)
	for i, t := range tokens {
		if i > 0 {
			normalized = append(normalized, ' ')
		}
		normalized = append(normalized, t.Text...)
	}
	field(normalized)
	return hex.EncodeToString(mac.Sum(nil))
}

// Recorder stores shadow records.
type Recorder interface {
	Record(context.Context, Record) error
}

// MemoryRecorder keeps records in memory.
type MemoryRecorder struct {
	mu      sync.Mutex
	records []Record
}

func (m *MemoryRecorder) Record(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, r)
	return nil
}

// Records returns a copy of the records.
func (m *MemoryRecorder) Records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Record(nil), m.records...)
}

// JSONLRecorder writes one JSON record per line.
type JSONLRecorder struct {
	mu sync.Mutex
	w  io.Writer
}

// NewJSONLRecorder writes to w.
func NewJSONLRecorder(w io.Writer) *JSONLRecorder { return &JSONLRecorder{w: w} }

func (j *JSONLRecorder) Record(_ context.Context, r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	_, err = j.w.Write(append(b, '\n'))
	return err
}
