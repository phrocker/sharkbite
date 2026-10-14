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

package iterrt

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// ScoreFilterIterator is a terminal ranking iterator for generic score-based
// pushdowns. It buffers candidate cells in the requested range, computes a
// float32 score, keeps topK (default 10), and emits score-descending cells with
// values replaced by the big-endian float32 score. Ties are broken by ascending
// key via the shared VectorKNN heap helpers.
//
// Methods are intentionally simple and schema-neutral:
//   - vector_sim: cosine similarity between query.b64 and the cell's packed
//     big-endian float32 vector value.
//   - time_decay: exp(-ln2 * age / halfLifeMs), where timestamps are interpreted
//     as Unix milliseconds; future cells are clamped to score 1 and underflow is
//     naturally clamped at 0.
//   - linear (default): params[0] (or 1 when absent) times the first big-endian
//     float32 stored in the cell value.
type ScoreFilterIterator struct {
	source SortedKeyValueIterator

	scoreCF           []byte
	method            string
	query             []float32
	queryNorm         float32
	topK              int
	params            []float32
	timestampAnchorMs int64
	halfLifeMs        int64

	out      []Cell
	outIndex int
	err      error
}

// ScoreFilterIterator option keys.
const (
	ScoreFilterScoreCF           = "scoreCF"
	ScoreFilterMethod            = "method"
	ScoreFilterQuery             = "query.b64"
	ScoreFilterTopK              = "topK"
	ScoreFilterParamCount        = "paramCount"
	ScoreFilterParamPrefix       = "param."
	ScoreFilterTimestampAnchorMs = "timestampAnchorMs"
	ScoreFilterHalfLifeMs        = "halfLifeMs"

	scoreFilterMethodVectorSim = "vector_sim"
	scoreFilterMethodTimeDecay = "time_decay"
	scoreFilterMethodLinear    = "linear"
)

// NewScoreFilterIterator constructs an un-Init'd iterator.
func NewScoreFilterIterator() *ScoreFilterIterator {
	return &ScoreFilterIterator{}
}

// Init wires the source and parses score-filter options.
func (s *ScoreFilterIterator) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: ScoreFilterIterator requires a non-nil source")
	}
	s.source = source
	if cf, ok := options[ScoreFilterScoreCF]; ok && cf != "" {
		s.scoreCF = []byte(cf)
	}

	s.method = scoreFilterMethodLinear
	switch options[ScoreFilterMethod] {
	case "", scoreFilterMethodLinear:
		s.method = scoreFilterMethodLinear
	case scoreFilterMethodVectorSim:
		s.method = scoreFilterMethodVectorSim
	case scoreFilterMethodTimeDecay:
		s.method = scoreFilterMethodTimeDecay
	default:
		return fmt.Errorf("iterrt: ScoreFilterIterator bad %s=%q (want %q, %q or %q)",
			ScoreFilterMethod, options[ScoreFilterMethod],
			scoreFilterMethodVectorSim, scoreFilterMethodTimeDecay, scoreFilterMethodLinear)
	}

	s.topK = 10
	if v := options[ScoreFilterTopK]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("iterrt: ScoreFilterIterator bad %s=%q", ScoreFilterTopK, v)
		}
		s.topK = n
	}

	paramN := 0
	if v := options[ScoreFilterParamCount]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("iterrt: ScoreFilterIterator bad %s=%q", ScoreFilterParamCount, v)
		}
		paramN = n
	}
	s.params = make([]float32, 0, paramN)
	for i := 0; i < paramN; i++ {
		key := fmt.Sprintf("%s%d", ScoreFilterParamPrefix, i)
		v, ok := options[key]
		if !ok {
			return fmt.Errorf("iterrt: ScoreFilterIterator missing option %q (paramCount=%d)", key, paramN)
		}
		f, err := strconv.ParseFloat(v, 32)
		if err != nil {
			return fmt.Errorf("iterrt: ScoreFilterIterator bad %s=%q", key, v)
		}
		s.params = append(s.params, float32(f))
	}

	if v := options[ScoreFilterTimestampAnchorMs]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("iterrt: ScoreFilterIterator bad %s=%q", ScoreFilterTimestampAnchorMs, v)
		}
		s.timestampAnchorMs = n
	}
	if s.timestampAnchorMs == 0 {
		s.timestampAnchorMs = time.Now().UnixMilli()
	}
	if v := options[ScoreFilterHalfLifeMs]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("iterrt: ScoreFilterIterator bad %s=%q", ScoreFilterHalfLifeMs, v)
		}
		s.halfLifeMs = n
	}

	if s.method == scoreFilterMethodVectorSim {
		qB64 := options[ScoreFilterQuery]
		if qB64 == "" {
			return fmt.Errorf("iterrt: ScoreFilterIterator missing option %q", ScoreFilterQuery)
		}
		qBytes, err := base64.StdEncoding.DecodeString(qB64)
		if err != nil {
			return fmt.Errorf("iterrt: ScoreFilterIterator bad %s: %w", ScoreFilterQuery, err)
		}
		s.query, err = unpackFloat32BE(qBytes)
		if err != nil {
			return fmt.Errorf("iterrt: ScoreFilterIterator %s: %w", ScoreFilterQuery, err)
		}
		if len(s.query) == 0 {
			return fmt.Errorf("iterrt: ScoreFilterIterator %s is empty", ScoreFilterQuery)
		}
		s.queryNorm = knnNorm(s.query)
	}
	if s.method == scoreFilterMethodTimeDecay && s.halfLifeMs <= 0 {
		return fmt.Errorf("iterrt: ScoreFilterIterator %s requires positive %s",
			scoreFilterMethodTimeDecay, ScoreFilterHalfLifeMs)
	}
	return nil
}

// Seek scores candidates in range and buffers the top-k in score-descending
// order.
func (s *ScoreFilterIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	s.out = s.out[:0]
	s.outIndex = 0
	s.err = nil

	if err := s.source.Seek(r, columnFamilies, inclusive); err != nil {
		s.err = err
		return err
	}
	h := &knnHeap{cap: s.topK}
	for s.source.HasTop() {
		k := s.source.GetTopKey()
		if len(s.scoreCF) == 0 || bytesEqual(k.ColumnFamily, s.scoreCF) {
			if score, ok := s.score(k, s.source.GetTopValue()); ok {
				h.offer(&knnScored{key: k.Clone(), score: score})
			}
		}
		if err := s.source.Next(); err != nil {
			s.err = err
			return err
		}
	}
	for _, scored := range h.drainDescending() {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, math.Float32bits(scored.score))
		s.out = append(s.out, Cell{Key: scored.key, Value: buf})
	}
	return nil
}

func (s *ScoreFilterIterator) score(k *Key, value []byte) (float32, bool) {
	switch s.method {
	case scoreFilterMethodVectorSim:
		vec, err := unpackFloat32BE(value)
		if err != nil || len(vec) != len(s.query) {
			return 0, false
		}
		den := s.queryNorm * knnNorm(vec)
		if den == 0 {
			return 0, true
		}
		return knnDot(s.query, vec) / den, true
	case scoreFilterMethodTimeDecay:
		age := s.timestampAnchorMs - k.Timestamp
		if age <= 0 {
			return 1, true
		}
		score := math.Exp(-math.Ln2 * float64(age) / float64(s.halfLifeMs))
		if score < 0 {
			score = 0
		}
		if score > 1 {
			score = 1
		}
		return float32(score), true
	default:
		if len(value) < 4 {
			return 0, false
		}
		factor := float32(1)
		if len(s.params) > 0 {
			factor = s.params[0]
		}
		return factor * math.Float32frombits(binary.BigEndian.Uint32(value[:4])), true
	}
}

// HasTop reports whether a cell is available.
func (s *ScoreFilterIterator) HasTop() bool {
	return s.err == nil && s.outIndex < len(s.out)
}

// GetTopKey returns the current top key, or nil when exhausted.
func (s *ScoreFilterIterator) GetTopKey() *Key {
	if !s.HasTop() {
		return nil
	}
	return s.out[s.outIndex].Key
}

// GetTopValue returns the current top score value, or nil when exhausted.
func (s *ScoreFilterIterator) GetTopValue() []byte {
	if !s.HasTop() {
		return nil
	}
	return s.out[s.outIndex].Value
}

// Next advances past the current top.
func (s *ScoreFilterIterator) Next() error {
	if s.err != nil {
		return s.err
	}
	if !s.HasTop() {
		return errors.New("iterrt: ScoreFilterIterator.Next called without a top")
	}
	s.outIndex++
	return nil
}

// DeepCopy returns an un-Seeked iterator over a DeepCopy'd source, carrying the
// same resolved options forward.
func (s *ScoreFilterIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	cp := &ScoreFilterIterator{
		source:            s.source.DeepCopy(env),
		scoreCF:           s.scoreCF,
		method:            s.method,
		query:             s.query,
		queryNorm:         s.queryNorm,
		topK:              s.topK,
		timestampAnchorMs: s.timestampAnchorMs,
		halfLifeMs:        s.halfLifeMs,
	}
	cp.params = append([]float32(nil), s.params...)
	return cp
}
