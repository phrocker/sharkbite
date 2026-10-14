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

package authorized

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// A scalar ledger proves the substrate does not require domain-set semantics.
type counterLedger struct {
	records           map[string]AccumulatorRecord[int]
	loads, stores     int
	loadErr, storeErr error
}

func (l *counterLedger) Load(_ context.Context, key string) (AccumulatorRecord[int], bool, error) {
	l.loads++
	record, found := l.records[key]
	return record, found, l.loadErr
}
func (l *counterLedger) Store(_ context.Context, key string, record AccumulatorRecord[int]) error {
	l.stores++
	if l.storeErr != nil {
		return l.storeErr
	}
	if l.records == nil {
		l.records = make(map[string]AccumulatorRecord[int])
	}
	l.records[key] = record
	return nil
}
func increment(state int) (int, error) { return state + 1, nil }

func TestAccumulatorConstruction(t *testing.T) {
	var typedNil *counterLedger
	for _, tc := range []struct {
		name      string
		ledger    AccumulatorLedger[int]
		window    time.Duration
		namespace string
	}{
		{"missing ledger", nil, time.Hour, "counter"},
		{"typed nil ledger", typedNil, time.Hour, "counter"},
		{"zero window", &counterLedger{}, 0, "counter"},
		{"negative window", &counterLedger{}, -time.Hour, "counter"},
		{"missing namespace", &counterLedger{}, time.Hour, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if a, err := NewAccumulator(tc.ledger, tc.window, tc.namespace); a != nil || err == nil {
				t.Fatalf("construction = %v, %v; want nil and error", a, err)
			}
		})
	}
	for _, a := range []*Accumulator[int]{nil, {}} {
		if state, err := a.Update(context.Background(), auth.Decision{}, time.Now(), increment); state != 0 || err == nil {
			t.Fatalf("uninitialized update = %d, %v", state, err)
		}
	}
}

func TestAccumulatorWindows(t *testing.T) {
	start := time.Unix(1700000000, 0).UTC()
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		want    int
	}{
		{"same instant", 0, 2},
		{"inside", time.Hour - time.Nanosecond, 2},
		{"exact boundary", time.Hour, 1},
		{"expired", 2 * time.Hour, 1},
		{"clock rollback", -time.Nanosecond, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &counterLedger{}
			a, err := NewAccumulator[int](ledger, time.Hour, "counter")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Update(context.Background(), auth.Decision{}, start, increment); err != nil {
				t.Fatal(err)
			}
			// Reconstruction over the same ledger must preserve state.
			a, err = NewAccumulator[int](ledger, time.Hour, "counter")
			if err != nil {
				t.Fatal(err)
			}
			now := start.Add(tc.elapsed)
			got, err := a.Update(context.Background(), auth.Decision{}, now, increment)
			if err != nil || got != tc.want {
				t.Fatalf("state = %d, %v; want %d", got, err, tc.want)
			}
			wantStart := start
			if tc.want == 1 {
				wantStart = now
			}
			for _, record := range ledger.records {
				if !record.WindowStart.Equal(wantStart) {
					t.Fatalf("window = %v, want %v", record.WindowStart, wantStart)
				}
			}
		})
	}
}

func TestAccumulatorFailuresDoNotReleaseState(t *testing.T) {
	failure := errors.New("ledger unavailable")
	for _, phase := range []string{"load", "contribute", "store"} {
		t.Run(phase, func(t *testing.T) {
			ledger := &counterLedger{}
			a, err := NewAccumulator[int](ledger, time.Hour, "counter")
			if err != nil {
				t.Fatal(err)
			}
			if phase == "load" {
				ledger.loadErr = failure
			}
			if phase == "store" {
				ledger.storeErr = failure
			}
			called := false
			got, err := a.Update(context.Background(), auth.Decision{}, time.Now(), func(state int) (int, error) {
				called = true
				if phase == "contribute" {
					return 42, failure
				}
				return 42, nil
			})
			if got != 0 || !errors.Is(err, failure) {
				t.Fatalf("update = %d, %v", got, err)
			}
			if called != (phase != "load") {
				t.Fatalf("callback called = %v", called)
			}
			wantStores := 0
			if phase == "store" {
				wantStores = 1
			}
			if ledger.stores != wantStores || len(ledger.records) != 0 {
				t.Fatalf("unexpected stores/state: %+v", ledger)
			}
		})
	}
}

func TestAccumulatorSerializesConcurrentUpdates(t *testing.T) {
	ledger := &counterLedger{}
	a, err := NewAccumulator[int](ledger, time.Hour, "counter")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Update(context.Background(), auth.Decision{}, now, increment); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(ledger.records) != 1 {
		t.Fatalf("records = %d", len(ledger.records))
	}
	for _, record := range ledger.records {
		if record.State != 100 {
			t.Fatalf("state = %d, want 100", record.State)
		}
	}
}

func accumulatorDecision(t *testing.T, domain, subject string) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(subject), Actor: "actor", RequestID: "request",
		AuthorizationDomain: []byte(domain), AllowedOperations: []auth.Operation{auth.OperationRead},
		PermittedSourceIDs: [][]byte{[]byte("source")}, PermittedPolicyIDs: [][]byte{[]byte("policy")},
		PolicyGeneration: 1, AuthenticationExpires: time.Unix(1800000000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func TestAccumulatorIdentityIsolationAndLegacyFraming(t *testing.T) {
	decision := accumulatorDecision(t, "ab", "c")
	// Historical mosaic framing: uint64 big-endian byte lengths, domain, subject.
	framed := []byte{0, 0, 0, 0, 0, 0, 0, 2, 'a', 'b', 0, 0, 0, 0, 0, 0, 0, 1, 'c'}
	expected := auth.DigestBytes("explorer-mosaic-identity-v1", framed).String()
	if got := accumulatorIdentityKey("explorer-mosaic-identity-v1", decision); got != expected {
		t.Fatalf("legacy key changed: %s != %s", got, expected)
	}
	keys := map[string]bool{}
	for _, pair := range [][2]string{{"ab", "c"}, {"a", "bc"}, {"ab", "d"}, {"cd", "c"}} {
		for _, namespace := range []string{"explorer-mosaic-identity-v1", "other-control"} {
			key := accumulatorIdentityKey(namespace, accumulatorDecision(t, pair[0], pair[1]))
			if keys[key] {
				t.Fatalf("identity/namespace collision: %v %s", pair, namespace)
			}
			keys[key] = true
		}
	}
}

func TestMosaicAccumulatorReadsLegacyRecord(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	decision := accumulatorDecision(t, "ab", "c")
	key := auth.DigestBytes("explorer-mosaic-identity-v1", []byte{0, 0, 0, 0, 0, 0, 0, 2, 'a', 'b', 0, 0, 0, 0, 0, 0, 0, 1, 'c'}).String()
	// Seed the pre-extraction record directly through the historical row/kind.
	if err := store.writeRow([]byte("mosaic/"+key), byte(8), persistedCoOccurrence{
		Seq: 1, Key: key, WindowStartUnixNano: now.UnixNano(), Domains: []string{"z"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	budget := MosaicBudget{MaxDomains: 2, Window: time.Hour}
	a, err := budget.accumulator(store)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{mosaic: budget, accumulator: a}
	selection, err := client.applyMosaicBudget(context.Background(), decision, now.Add(time.Second),
		[]shoal.ID{"old", "new", "restricted", "repeat"},
		map[shoal.ID]string{"old": "z", "new": "a", "restricted": "b", "repeat": "a"})
	if err != nil {
		t.Fatal(err)
	}
	if selection.restricted != 1 || !reflect.DeepEqual(selection.allowed, map[shoal.ID]struct{}{"old": {}, "new": {}, "repeat": {}}) {
		t.Fatalf("selection = %+v", selection)
	}
	record, found, err := store.LoadCoOccurrence(context.Background(), key)
	if err != nil || !found || !record.WindowStart.Equal(now) || !reflect.DeepEqual(record.Domains, []string{"a", "z"}) {
		t.Fatalf("record = %+v, %v, %v", record, found, err)
	}
	// Pin the serialized schema, envelope kind and row prefix independently.
	typ := reflect.TypeOf(persistedCoOccurrence{})
	wantFields := []string{"Seq:uint64", "Key:string", "WindowStartUnixNano:int64", "Domains:[]string"}
	var fields []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		fields = append(fields, f.Name+":"+f.Type.String())
	}
	if !reflect.DeepEqual(fields, wantFields) || policyKindCoOccurrence != 8 || string(policyCoOccurrenceRow(key)) != "mosaic/"+key {
		t.Fatalf("persisted schema changed: %v", fields)
	}
}

// A failing policy store exercises the mosaic adapter and its disclosure path.
type failingMosaicStore struct {
	*MemoryPolicyStore
	loadErr, storeErr error
}

func (s failingMosaicStore) LoadCoOccurrence(ctx context.Context, key string) (CoOccurrenceRecord, bool, error) {
	if s.loadErr != nil {
		return CoOccurrenceRecord{}, false, s.loadErr
	}
	return s.MemoryPolicyStore.LoadCoOccurrence(ctx, key)
}
func (s failingMosaicStore) StoreCoOccurrence(ctx context.Context, key string, record CoOccurrenceRecord) error {
	if s.storeErr != nil {
		return s.storeErr
	}
	return s.MemoryPolicyStore.StoreCoOccurrence(ctx, key, record)
}

func TestMosaicAccumulatorFailureDiscardsProvisionalDisclosure(t *testing.T) {
	failure := errors.New("storage failed")
	for _, phase := range []string{"load", "store", "domain"} {
		t.Run(phase, func(t *testing.T) {
			store := failingMosaicStore{MemoryPolicyStore: NewMemoryPolicyStore()}
			if phase == "load" {
				store.loadErr = failure
			}
			if phase == "store" {
				store.storeErr = failure
			}
			budget := MosaicBudget{MaxDomains: 1, Window: time.Hour}
			a, err := budget.accumulator(store)
			if err != nil {
				t.Fatal(err)
			}
			client := &Client{mosaic: budget, accumulator: a}
			domains := map[shoal.ID]string{"allowed": "a", "withheld": "b", "last": "a"}
			if phase == "domain" {
				domains["last"] = ""
			}
			selection, err := client.applyMosaicBudget(context.Background(), auth.Decision{}, time.Now(),
				[]shoal.ID{"allowed", "withheld", "last"}, domains)
			if err == nil || selection.allowed != nil || selection.restricted != 0 {
				t.Fatalf("failed read returned selection: %+v, %v", selection, err)
			}
			if phase != "domain" && !errors.Is(err, failure) {
				t.Fatalf("error = %v", err)
			}
			if len(store.coOccurrence) != 0 {
				t.Fatalf("failed update stored domains: %v", store.coOccurrence)
			}
		})
	}
}

func TestMosaicDisabledBypassesAccumulatorAndDomainResolution(t *testing.T) {
	// An invalid window and absent ledger remain acceptable when disabled.
	budget := MosaicBudget{Window: -time.Hour}
	a, err := budget.accumulator(nil)
	if err != nil || a != nil {
		t.Fatalf("disabled construction = %v, %v", a, err)
	}
	client := &Client{mosaic: budget}
	selection, err := client.restrictCoOccurrence(context.Background(), auth.Decision{}, time.Now(),
		[]shoal.ID{"one", "two"}, func(shoal.ID) string { t.Fatal("disabled domain resolver called"); return "" })
	if err != nil || len(selection.allowed) != 2 || selection.restricted != 0 {
		t.Fatalf("disabled selection = %+v, %v", selection, err)
	}
}
