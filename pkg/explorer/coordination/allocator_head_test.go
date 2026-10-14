package coordination

import (
	"reflect"
	"testing"
	"time"
)

func fixtureAllocatorHead() AllocatorHeadV1 {
	return AllocatorHeadV1{
		HeadGeneration: 23, NextEpoch: 18, RetiredThrough: 12, Frontier: 17,
		VisibleAt:        time.Date(2026, 8, 27, 14, 30, 0, 123, time.UTC),
		CheckpointDigest: testDigest("checkpoint"), HistoryFloor: 3,
		RetentionGeneration: 8, WriterAuthorityGeneration: 9,
		WriterMode: WriterModeAccumuloPrimary, WriterHolder: OwnerID{0, 0xff, 'w'},
		WriterFence: 11, ActiveWindowStart: 13, ActiveReservations: 5,
		MaxActiveReservations: 128, ImportPlanDigest: testDigest("import"), ImportMaxEpoch: 100,
	}
}

func TestAllocatorHeadGoldenRoundTripAndCorruption(t *testing.T) {
	want := fixtureAllocatorHead()
	encoded, err := MarshalAllocatorHeadV1(want)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalAllocatorHeadV1(golden(t, "allocator_head_v1.bin", encoded))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("round trip differs: got %#v want %#v", decoded, want)
	}
	for name, value := range map[string][]byte{
		"truncated": encoded[:len(encoded)-1],
		"trailing":  append(append([]byte(nil), encoded...), 0),
		"checksum":  corruptByte(encoded, envelopeHeaderSize),
		"version":   corruptHeader(encoded, len(envelopeMagic)+2, 99),
	} {
		if _, err := UnmarshalAllocatorHeadV1(value); err == nil {
			t.Fatalf("%s fixture accepted", name)
		}
	}
}

func TestAllocatorHeadRejectsNoncanonicalState(t *testing.T) {
	tests := []func(*AllocatorHeadV1){
		func(h *AllocatorHeadV1) { h.HeadGeneration = 0 },
		func(h *AllocatorHeadV1) { h.NextEpoch = 0 },
		func(h *AllocatorHeadV1) { h.Frontier = h.NextEpoch },
		func(h *AllocatorHeadV1) { h.ActiveReservations = h.MaxActiveReservations + 1 },
		func(h *AllocatorHeadV1) { h.ActiveWindowStart = 0 },
		func(h *AllocatorHeadV1) { h.VisibleAt = h.VisibleAt.In(time.FixedZone("local", 3600)) },
		func(h *AllocatorHeadV1) { h.WriterMode = 99 },
	}
	for i, mutate := range tests {
		value := fixtureAllocatorHead()
		mutate(&value)
		if _, err := MarshalAllocatorHeadV1(value); err == nil {
			t.Fatalf("invalid fixture %d accepted", i)
		}
	}
}

func TestAllocatorHeadSuccessorGeneration(t *testing.T) {
	previous := fixtureAllocatorHead()
	next := previous
	next.HeadGeneration++
	if err := ValidateAllocatorHeadSuccessor(previous, next); err != nil {
		t.Fatal(err)
	}
	next.HeadGeneration++
	if err := ValidateAllocatorHeadSuccessor(previous, next); err == nil {
		t.Fatal("skipped head generation accepted")
	}
	previous.HeadGeneration = Generation(^uint64(0) >> 1)
	next = previous
	if err := ValidateAllocatorHeadSuccessor(previous, next); err == nil {
		t.Fatal("exhausted head generation accepted")
	}
}
