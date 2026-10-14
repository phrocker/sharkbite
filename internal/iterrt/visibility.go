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
	"errors"

	"github.com/phrocker/shoal-oss/internal/visfilter"
)

// VisibilityFilter drops cells whose ColumnVisibility the environment's
// Authorizations cannot satisfy. It reuses internal/visfilter for the CV
// expression parse + evaluation — the same evaluator the V0 scan path
// already uses — so a cell's visibility decision is identical whether it
// flows through a Thrift scan or a shoal compaction stack.
//
// Scope: active ONLY at ScopeScan. Compaction scopes (minc/majc) never
// visibility-filter — a compaction must preserve every cell regardless
// of who later reads it; dropping cells at compaction time would
// permanently destroy data a differently-authorized scan should see.
// At minc/majc this iterator is a transparent passthrough. This mirrors
// how Java wires the visibility filter as a scan-time system iterator,
// not a compaction one.
//
// A nil env.Authorizations means "system context" — every cell visible
// (passthrough), matching iterrt.IteratorEnvironment's documented
// contract.
type VisibilityFilter struct {
	source SortedKeyValueIterator
	active bool
	eval   *visfilter.Evaluator
	err    error
}

// NewVisibilityFilter constructs an un-Init'd VisibilityFilter.
func NewVisibilityFilter() *VisibilityFilter {
	return &VisibilityFilter{}
}

// Init records the source and, when the scope + auths warrant it,
// compiles a visfilter.Evaluator over env.Authorizations. When inactive
// (non-scan scope, or nil auths) the iterator is a passthrough and no
// evaluator is built.
func (vf *VisibilityFilter) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: VisibilityFilter requires a non-nil source")
	}
	vf.source = source
	vf.active = env.Scope == ScopeScan && env.Authorizations != nil
	if vf.active {
		auths := visfilter.NewAuthorizations(env.Authorizations...)
		vf.eval = visfilter.NewEvaluator(auths)
	}
	return nil
}

// Seek seeks the source, then skips forward past any leading cell the
// caller is not authorized to see.
func (vf *VisibilityFilter) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	vf.err = nil
	if err := vf.source.Seek(r, columnFamilies, inclusive); err != nil {
		vf.err = err
		return err
	}
	return vf.skipInvisible()
}

// Next advances past the current top, then skips any cells the caller
// cannot see.
func (vf *VisibilityFilter) Next() error {
	if vf.err != nil {
		return vf.err
	}
	if !vf.source.HasTop() {
		return errors.New("iterrt: VisibilityFilter.Next called without a top")
	}
	if err := vf.source.Next(); err != nil {
		vf.err = err
		return err
	}
	return vf.skipInvisible()
}

// skipInvisible advances the source until its top is visible or it runs
// out. A no-op when the filter is inactive.
func (vf *VisibilityFilter) skipInvisible() error {
	if !vf.active {
		return nil
	}
	for vf.source.HasTop() {
		if vf.eval.Visible(vf.source.GetTopKey().ColumnVisibility) {
			return nil
		}
		if err := vf.source.Next(); err != nil {
			vf.err = err
			return err
		}
	}
	return nil
}

// HasTop reports whether a visible cell is available.
func (vf *VisibilityFilter) HasTop() bool {
	return vf.err == nil && vf.source.HasTop()
}

// GetTopKey returns the current top key (passthrough from the source).
func (vf *VisibilityFilter) GetTopKey() *Key {
	if !vf.source.HasTop() {
		return nil
	}
	return vf.source.GetTopKey()
}

// GetTopValue returns the current top value (passthrough from the source).
func (vf *VisibilityFilter) GetTopValue() []byte {
	if !vf.source.HasTop() {
		return nil
	}
	return vf.source.GetTopValue()
}

// DeepCopy returns an independent VisibilityFilter over a DeepCopy'd
// source. env replaces the original environment for the copy — so the
// copy re-derives active/eval from the new env, matching the iterrt
// contract that DeepCopy's env "replaces the original".
func (vf *VisibilityFilter) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	cp := &VisibilityFilter{
		source: vf.source.DeepCopy(env),
		active: env.Scope == ScopeScan && env.Authorizations != nil,
	}
	if cp.active {
		cp.eval = visfilter.NewEvaluator(visfilter.NewAuthorizations(env.Authorizations...))
	}
	return cp
}
