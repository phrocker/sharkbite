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
	"fmt"
	"strconv"
)

// AnomalyDetectIterator is a deterministic read-path row filter. It buffers
// each row, reads a numeric metric cell selected by valueCF/valueCQ, and emits
// the row only when the metric lies outside a configured [min,max] band.
// Numeric values may be ASCII floats or big-endian float32/float64.
//
// Options:
//
//	valueCF  metric column family (optional)
//	valueCQ  metric column qualifier (optional)
//	min      inclusive lower bound (optional)
//	max      inclusive upper bound (optional)
type AnomalyDetectIterator struct {
	source SortedKeyValueIterator

	valueCF []byte
	valueCQ []byte
	min     float64
	max     float64
	hasMin  bool
	hasMax  bool

	out      []Cell
	outIndex int
	err      error
}

const (
	AnomalyDetectValueCF = "valueCF"
	AnomalyDetectValueCQ = "valueCQ"
	AnomalyDetectMin     = "min"
	AnomalyDetectMax     = "max"
)

func NewAnomalyDetectIterator() *AnomalyDetectIterator { return &AnomalyDetectIterator{} }

func (a *AnomalyDetectIterator) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: AnomalyDetectIterator requires a non-nil source")
	}
	a.source = source
	if s := options[AnomalyDetectValueCF]; s != "" {
		a.valueCF = []byte(s)
	}
	if s := options[AnomalyDetectValueCQ]; s != "" {
		a.valueCQ = []byte(s)
	}
	if s := options[AnomalyDetectMin]; s != "" {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("iterrt: AnomalyDetectIterator bad %s=%q", AnomalyDetectMin, s)
		}
		a.min, a.hasMin = v, true
	}
	if s := options[AnomalyDetectMax]; s != "" {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("iterrt: AnomalyDetectIterator bad %s=%q", AnomalyDetectMax, s)
		}
		a.max, a.hasMax = v, true
	}
	if !a.hasMin && !a.hasMax {
		return fmt.Errorf("iterrt: AnomalyDetectIterator requires %q or %q", AnomalyDetectMin, AnomalyDetectMax)
	}
	return nil
}

func (a *AnomalyDetectIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	a.out = a.out[:0]
	a.outIndex = 0
	a.err = nil
	if err := a.source.Seek(r, columnFamilies, inclusive); err != nil {
		a.err = err
		return err
	}
	for a.source.HasTop() {
		row := string(a.source.GetTopKey().Row)
		rowCells := []Cell{}
		anomalous := false
		for a.source.HasTop() && string(a.source.GetTopKey().Row) == row {
			k := a.source.GetTopKey().Clone()
			v := append([]byte(nil), a.source.GetTopValue()...)
			rowCells = append(rowCells, Cell{Key: k, Value: v})
			if a.metricCell(k) {
				if n, ok := parseNumericValue(v); ok && a.outOfBand(n) {
					anomalous = true
				}
			}
			if err := a.source.Next(); err != nil {
				a.err = err
				return err
			}
		}
		if anomalous {
			a.out = append(a.out, rowCells...)
		}
	}
	return nil
}

func (a *AnomalyDetectIterator) metricCell(k *Key) bool {
	if len(a.valueCF) > 0 && !bytesEqual(k.ColumnFamily, a.valueCF) {
		return false
	}
	if len(a.valueCQ) > 0 && !bytesEqual(k.ColumnQualifier, a.valueCQ) {
		return false
	}
	return true
}
func (a *AnomalyDetectIterator) outOfBand(v float64) bool {
	return (a.hasMin && v < a.min) || (a.hasMax && v > a.max)
}

func (a *AnomalyDetectIterator) HasTop() bool { return a.err == nil && a.outIndex < len(a.out) }
func (a *AnomalyDetectIterator) GetTopKey() *Key {
	if !a.HasTop() {
		return nil
	}
	return a.out[a.outIndex].Key
}
func (a *AnomalyDetectIterator) GetTopValue() []byte {
	if !a.HasTop() {
		return nil
	}
	return a.out[a.outIndex].Value
}
func (a *AnomalyDetectIterator) Next() error {
	if a.err != nil {
		return a.err
	}
	if !a.HasTop() {
		return errors.New("iterrt: AnomalyDetectIterator.Next called without a top")
	}
	a.outIndex++
	return nil
}
func (a *AnomalyDetectIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	return &AnomalyDetectIterator{source: a.source.DeepCopy(env), valueCF: a.valueCF, valueCQ: a.valueCQ, min: a.min, max: a.max, hasMin: a.hasMin, hasMax: a.hasMax}
}
