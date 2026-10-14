/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func modelFixture(t *testing.T) (string, string) {
	t.Helper()
	task, _, _, err := definitions()
	if err != nil {
		t.Fatal(err)
	}
	value := map[string]any{"schema": 1, "kind": "linear-svm", "task_id": task.ID(), "question_id": "priority", "feature_schema_id": featureSchema, "dataset_sha256": strings.Repeat("a", 64), "recipe_sha256": strings.Repeat("b", 64), "training_runtime_sha256": strings.Repeat("c", 64), "labels": []string{"ordinary", "inspect"}, "coefficients": []float64{1, 0}, "intercept": 0, "threshold": 0}
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "model.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path, hash(b)
}
func TestInquiryRestartsWithOriginalReceipt(t *testing.T) {
	model, digest := modelFixture(t)
	dir := t.TempDir()
	first, err := inquire(model, digest, dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Label != "inspect" || first.ProviderCalls != 1 || !first.ReplayMatched || first.EffectiveDevice != "cpu" || !first.Synthetic {
		t.Fatalf("unexpected first inquiry: %+v", first)
	}
	second, err := inquire(model, digest, dir)
	if err != nil {
		t.Fatal(err)
	}
	if second.ProviderCalls != 0 || second.PredictionID != first.PredictionID || second.ReceiptID != first.ReceiptID || second.RequestID != first.RequestID || !second.ReplayMatched {
		t.Fatalf("restart reinvoked or changed receipt: %+v", second)
	}
}
func TestDemoRejectsChangedModelAndCorruptState(t *testing.T) {
	model, digest := modelFixture(t)
	dir := t.TempDir()
	if _, err := inquire(model, strings.Repeat("0", 64), dir); err == nil {
		t.Fatal("bad external digest accepted")
	}
	if _, err := inquire(model, digest, dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(model)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	v["intercept"] = 1
	changed, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inquire(model, hash(changed), dir); err == nil {
		t.Fatal("new model reused old state namespace")
	}
	if err := os.WriteFile(model, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo-state.json"), []byte(`{"Schema":1,"Schema":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inquire(model, digest, dir); err == nil {
		t.Fatal("corrupt state accepted")
	}
}
func TestPrepareIsExclusiveAndPinsTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dataset.json")
	if err := prepare(path); err != nil {
		t.Fatal(err)
	}
	if err := prepare(path); err == nil {
		t.Fatal("dataset overwritten")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	task, _, _, err := definitions()
	if err != nil {
		t.Fatal(err)
	}
	if v["task_id"] != string(task.ID()) || v["provenance"] != "synthetic" {
		t.Fatal("dataset detached from synthetic task")
	}
}

func TestMissingReplayIdentityCannotBeRegenerated(t *testing.T) {
	model, digest := modelFixture(t)
	dir := t.TempDir()
	first, err := inquire(model, digest, dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.ProviderCalls != 1 {
		t.Fatal("initial model call missing")
	}
	if err := os.Remove(filepath.Join(dir, "demo-state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := inquire(model, digest, dir); err == nil {
		t.Fatal("missing request identity silently replaced")
	}
}

func TestStateDirectoryAncestorsSyncOnCreationAndRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "nested")
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	var expected []string
	for current := absolute; ; current = filepath.Dir(current) {
		expected = append(expected, current)
		if filepath.Dir(current) == current {
			break
		}
	}
	fault := errors.New("directory sync unavailable")
	for _, fail := range []bool{true, false} {
		var calls []string
		err := makeStateDirectory(path, func(p string) error {
			calls = append(calls, p)
			if fail && len(calls) == 2 {
				return fault
			}
			return nil
		})
		if fail {
			if !errors.Is(err, fault) {
				t.Fatal("sync failure hidden", err)
			}
		} else {
			if err != nil || !reflect.DeepEqual(calls, expected) {
				t.Fatalf("retry omitted ancestor synchronization: %v %v", calls, err)
			}
		}
	}
}
