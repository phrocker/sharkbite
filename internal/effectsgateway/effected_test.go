// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package effectsgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

// committedWithVolume is committed() with the record's effected volume.
func committedWithVolume(t *testing.T, code string, volume *dispatchapi.Effected) string {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal([]byte(committed(t, 3, dispatchapi.DispatchFailed, code, "")), &record); err != nil {
		t.Fatal(err)
	}
	if volume != nil {
		record["effected"] = volume
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func effectedClient(t *testing.T, replies ...func() (*http.Response, error)) (*DispatchClient, *scriptedTransport) {
	t.Helper()
	transport := &scriptedTransport{replies: replies}
	base, _ := url.Parse("https://explorer.invalid")
	client, err := NewDispatchClient(base, &http.Client{Transport: transport},
		func() (string, error) { return "token", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client, transport
}

func partialFailure(volume dispatchapi.Effected) Completion {
	return Completion{
		Context: RequestContext{CorrelationID: []byte("trace"), RequestID: []byte("r"), ReasonCode: "gateway_complete",
			Deadline: time.Now().Add(time.Minute)},
		ExpectedVersion: 2, ClaimID: []byte("claim"), ClaimFence: 1, Failed: true,
		ErrorCode: ErrorOutcomeUnknown, Effected: volume,
	}
}

// TestCompleteCarriesTheVolumeAndReadsItBack: a failure after a partial send
// reports its volume on /complete; a lost answer is resent as the identical
// body; and the record the resend reads back is compared on the volume too,
// so a record that kept a different number is recorded otherwise rather than
// taken as this report.
func TestCompleteCarriesTheVolumeAndReadsItBack(t *testing.T) {
	volume := dispatchapi.Effected{Bytes: 4096, Chunks: 2}
	wire := &dispatchapi.Effected{Bytes: 4096, Chunks: 2}

	t.Run("lost then the recorded volume", func(t *testing.T) {
		client, transport := effectedClient(t, lost,
			reply(200, committedWithVolume(t, ErrorOutcomeUnknown, wire)))
		action, err := client.Complete(context.Background(), []byte("action"), partialFailure(volume))
		if err != nil {
			t.Fatalf("Complete = %v", err)
		}
		if action.Effected != volume {
			t.Fatalf("read back %+v, want %+v", action.Effected, volume)
		}
		if len(transport.bodies) != 2 || !bytes.Equal(transport.bodies[0], transport.bodies[1]) {
			t.Fatalf("the resend was not the identical body: %q", transport.bodies)
		}
		if !strings.Contains(string(transport.bodies[0]), `"effected":{"bytes":4096,"chunks":2}`) {
			t.Fatalf("the body carried no volume: %s", transport.bodies[0])
		}
	})
	for _, row := range []struct {
		name   string
		record *dispatchapi.Effected
	}{
		{"a different volume", &dispatchapi.Effected{Bytes: 4097, Chunks: 2}},
		// What an adjudicated invalid_executor_effected looks like, apart
		// from its code: the number was dropped.
		{"no volume", nil},
	} {
		t.Run("recorded with "+row.name, func(t *testing.T) {
			client, _ := effectedClient(t,
				reply(200, committedWithVolume(t, ErrorOutcomeUnknown, row.record)))
			_, err := client.Complete(context.Background(), []byte("action"), partialFailure(volume))
			if DispatchKind(err) != DispatchRecordedOtherwise {
				t.Fatalf("Complete = %v, want recorded_otherwise", err)
			}
		})
	}
	t.Run("nothing left omits the field", func(t *testing.T) {
		client, transport := effectedClient(t,
			reply(200, committedWithVolume(t, ErrorOutcomeUnknown, nil)))
		if _, err := client.Complete(context.Background(), []byte("action"),
			partialFailure(dispatchapi.Effected{})); err != nil {
			t.Fatalf("Complete = %v", err)
		}
		if strings.Contains(string(transport.bodies[0]), "effected") {
			t.Fatalf("a zero volume reached the wire: %s", transport.bodies[0])
		}
	})
}

// TestCompleteRefusesAVolumeThePlaneWouldNot: each is refused before anything
// is sent. On /complete the explorer adjudicates rather than refuses, so a
// malformed volume sent would become a terminal record saying the volume is
// unknown — worse than the worker learning its own mistake here.
func TestCompleteRefusesAVolumeThePlaneWouldNot(t *testing.T) {
	success := partialFailure(dispatchapi.Effected{Bytes: 10})
	success.Failed, success.ErrorCode, success.Output = false, "", json.RawMessage(`{"status":200}`)
	for name, completion := range map[string]Completion{
		"on a success":          success,
		"chunks without bytes":  partialFailure(dispatchapi.Effected{Chunks: 1}),
		"negative bytes":        partialFailure(dispatchapi.Effected{Bytes: -1}),
		"negative chunks":       partialFailure(dispatchapi.Effected{Bytes: 1, Chunks: -1}),
		"bytes past the bound":  partialFailure(dispatchapi.Effected{Bytes: dispatchapi.MaxEffectedBytes + 1}),
		"chunks past the bound": partialFailure(dispatchapi.Effected{Bytes: 1, Chunks: dispatchapi.MaxEffectedChunks + 1}),
	} {
		client, transport := effectedClient(t)
		_, err := client.Complete(context.Background(), []byte("action"), completion)
		if DispatchKind(err) != DispatchRefusedLocal {
			t.Errorf("%s: %v, want refused locally", name, err)
		}
		if len(transport.bodies) != 0 {
			t.Errorf("%s: a request left", name)
		}
	}
}
