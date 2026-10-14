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
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// GraphAggregationIterator is a deterministic read-path aggregation iterator.
// It drains the seek range, groups cells by a configurable key, and emits one
// synthetic cell per group: row="_agg", cf="result", cq=<group>, value=<number>.
//
// Options:
//
//	aggregationOp  "count" (default) | "sum" | "min" | "max" | "avg"
//	groupBy        "row" (default) | "cf" | "cq" | "cv" | "rowPrefix"
//	rowPrefixSep   separator used by groupBy=rowPrefix (default ":")
//	valueCF        optional CF restriction for aggregated input cells
//	valueCQ        optional CQ restriction for aggregated input cells
//	resultRow      output row (default "_agg")
//	resultCF       output column family (default "result")
type GraphAggregationIterator struct {
	source SortedKeyValueIterator

	op           string
	groupBy      string
	rowPrefixSep string
	valueCF      []byte
	valueCQ      []byte
	resultRow    string
	resultCF     string

	out      []Cell
	outIndex int
	err      error
}

const (
	GraphAggregationOp           = "aggregationOp"
	GraphAggregationGroupBy      = "groupBy"
	GraphAggregationRowPrefixSep = "rowPrefixSep"
	GraphAggregationValueCF      = "valueCF"
	GraphAggregationValueCQ      = "valueCQ"
	GraphAggregationResultRow    = "resultRow"
	GraphAggregationResultCF     = "resultCF"

	graphAggregationCount = "count"
	graphAggregationSum   = "sum"
	graphAggregationMin   = "min"
	graphAggregationMax   = "max"
	graphAggregationAvg   = "avg"
)

func NewGraphAggregationIterator() *GraphAggregationIterator {
	return &GraphAggregationIterator{op: graphAggregationCount, groupBy: "row", rowPrefixSep: ":", resultRow: "_agg", resultCF: "result"}
}

func (g *GraphAggregationIterator) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: GraphAggregationIterator requires a non-nil source")
	}
	g.source = source
	if s := options[GraphAggregationOp]; s != "" {
		switch s {
		case graphAggregationCount, graphAggregationSum, graphAggregationMin, graphAggregationMax, graphAggregationAvg:
			g.op = s
		default:
			return fmt.Errorf("iterrt: GraphAggregationIterator bad %s=%q", GraphAggregationOp, s)
		}
	}
	if s := options[GraphAggregationGroupBy]; s != "" {
		switch s {
		case "row", "cf", "cq", "cv", "rowPrefix":
			g.groupBy = s
		default:
			return fmt.Errorf("iterrt: GraphAggregationIterator bad %s=%q", GraphAggregationGroupBy, s)
		}
	}
	if s := options[GraphAggregationRowPrefixSep]; s != "" {
		g.rowPrefixSep = s
	}
	if s := options[GraphAggregationValueCF]; s != "" {
		g.valueCF = []byte(s)
	}
	if s := options[GraphAggregationValueCQ]; s != "" {
		g.valueCQ = []byte(s)
	}
	if s := options[GraphAggregationResultRow]; s != "" {
		g.resultRow = s
	}
	if s := options[GraphAggregationResultCF]; s != "" {
		g.resultCF = s
	}
	return nil
}

func (g *GraphAggregationIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	g.out = g.out[:0]
	g.outIndex = 0
	g.err = nil
	if err := g.source.Seek(r, columnFamilies, inclusive); err != nil {
		g.err = err
		return err
	}
	groups := map[string]*aggState{}
	for g.source.HasTop() {
		k := g.source.GetTopKey()
		if g.matches(k) {
			group := g.groupKey(k)
			st := groups[group]
			if st == nil {
				st = &aggState{}
				groups[group] = st
			}
			if g.op == graphAggregationCount {
				st.add(1)
			} else if n, ok := parseNumericValue(g.source.GetTopValue()); ok {
				st.add(n)
			}
		}
		if err := g.source.Next(); err != nil {
			g.err = err
			return err
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, group := range keys {
		st := groups[group]
		if st.count == 0 {
			continue
		}
		g.out = append(g.out, Cell{Key: &wire.Key{Row: []byte(g.resultRow), ColumnFamily: []byte(g.resultCF), ColumnQualifier: []byte(group)}, Value: []byte(formatAgg(g.op, st))})
	}
	return nil
}

func (g *GraphAggregationIterator) matches(k *Key) bool {
	if len(g.valueCF) > 0 && !bytesEqual(k.ColumnFamily, g.valueCF) {
		return false
	}
	if len(g.valueCQ) > 0 && !bytesEqual(k.ColumnQualifier, g.valueCQ) {
		return false
	}
	return true
}

func (g *GraphAggregationIterator) groupKey(k *Key) string {
	switch g.groupBy {
	case "cf":
		return string(k.ColumnFamily)
	case "cq":
		return string(k.ColumnQualifier)
	case "cv":
		return string(k.ColumnVisibility)
	case "rowPrefix":
		row := string(k.Row)
		for i := 0; i+len(g.rowPrefixSep) <= len(row); i++ {
			if row[i:i+len(g.rowPrefixSep)] == g.rowPrefixSep {
				return row[:i]
			}
		}
		return row
	default:
		return string(k.Row)
	}
}

type aggState struct {
	count         int
	sum, min, max float64
}

func (a *aggState) add(v float64) {
	if a.count == 0 || v < a.min {
		a.min = v
	}
	if a.count == 0 || v > a.max {
		a.max = v
	}
	a.sum += v
	a.count++
}
func formatAgg(op string, st *aggState) string {
	switch op {
	case graphAggregationCount:
		return strconv.Itoa(st.count)
	case graphAggregationMin:
		return strconv.FormatFloat(st.min, 'f', -1, 64)
	case graphAggregationMax:
		return strconv.FormatFloat(st.max, 'f', -1, 64)
	case graphAggregationAvg:
		return strconv.FormatFloat(st.sum/float64(st.count), 'f', -1, 64)
	default:
		return strconv.FormatFloat(st.sum, 'f', -1, 64)
	}
}

func parseNumericValue(v []byte) (float64, bool) {
	if f, err := strconv.ParseFloat(string(v), 64); err == nil {
		return f, true
	}
	if len(v) == 4 {
		return float64(math.Float32frombits(binary.BigEndian.Uint32(v))), true
	}
	if len(v) == 8 {
		return math.Float64frombits(binary.BigEndian.Uint64(v)), true
	}
	return 0, false
}

func (g *GraphAggregationIterator) HasTop() bool { return g.err == nil && g.outIndex < len(g.out) }
func (g *GraphAggregationIterator) GetTopKey() *Key {
	if !g.HasTop() {
		return nil
	}
	return g.out[g.outIndex].Key
}
func (g *GraphAggregationIterator) GetTopValue() []byte {
	if !g.HasTop() {
		return nil
	}
	return g.out[g.outIndex].Value
}
func (g *GraphAggregationIterator) Next() error {
	if g.err != nil {
		return g.err
	}
	if !g.HasTop() {
		return errors.New("iterrt: GraphAggregationIterator.Next called without a top")
	}
	g.outIndex++
	return nil
}
func (g *GraphAggregationIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	return &GraphAggregationIterator{source: g.source.DeepCopy(env), op: g.op, groupBy: g.groupBy, rowPrefixSep: g.rowPrefixSep, valueCF: g.valueCF, valueCQ: g.valueCQ, resultRow: g.resultRow, resultCF: g.resultCF}
}
