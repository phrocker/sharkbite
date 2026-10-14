// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package fleet

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestValidateActionInputIsWhatEnqueueStores: the exported wrapper returns,
// byte for byte, the input Enqueue stores, including for a document that
// repeats a key, and refuses what Enqueue refuses with the same error.
func TestValidateActionInputIsWhatEnqueueStores(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	descriptor := dispatchDescriptor(now)
	action := descriptor.Capabilities[0].Actions[0]
	for _, input := range []string{`{"value":1}`, `{"value":2,"value":1}`, ` {"value" : 7 } `} {
		authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
		registryStore := newMemoryStore()
		registry, _ := NewService(Config{
			Store: registryStore, Resolver: authority.Resolver(), Recorder: &memoryRecorder{},
			Snapshots: fixedSnapshot{now}, Executors: executorMap{"exec": &dispatchExecutor{}},
			Clock: func() time.Time { return now },
		})
		registryStore.records["agent"] = Stored{Descriptor: descriptor}
		store := newMemoryDispatchStore()
		service, _ := NewDispatchService(DispatchConfig{
			Store: store, Registry: registry, Resolver: authority.Resolver(),
			Recorder: &dispatchRecorder{}, Events: dispatchEvents{}, Clock: func() time.Time { return now },
		})
		ctx := bindDecision(t, authority, dispatchDecision(t, "owner", "actor", "request", auth.OperationDispatch))
		request := dispatchEnqueue(now, "request")
		request.Input = json.RawMessage(input)
		record, err := service.Enqueue(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := ValidateActionInput(action, json.RawMessage(input))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(record.Input, canonical) {
			t.Fatalf("%s: enqueue stored %s, wrapper returned %s", input, record.Input, canonical)
		}
	}
	_, wrapperErr := ValidateActionInput(action, json.RawMessage(`{"value":"x"}`))
	_, directErr := validateAgainstSchema(action.InputSchema, json.RawMessage(`{"value":"x"}`), "action input", MaxActionPayloadBytes)
	if wrapperErr == nil || wrapperErr.Error() != directErr.Error() || !shoal.IsErrorCode(wrapperErr, shoal.ErrorInvalidArgument) {
		t.Fatalf("wrapper error %v, enqueue validator error %v", wrapperErr, directErr)
	}
}
