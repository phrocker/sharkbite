package main

import (
	"fmt"
	"sync"
	"testing"
)

const (
	testABIVersionCompatibility = 1
	testABIVersionMajor         = 1
	testABIVersionMinor         = 19
	testABIVersionPatch         = 0
	testABIVersionPacked        = 0x00011300
	testABICapabilityCount      = 32
	testABICapabilityWord0      = 0x00000000ffffffff
)

func TestABIDiscoveryValues(t *testing.T) {
	if got := abiVersionCompatibility(); got != testABIVersionCompatibility {
		t.Fatalf("compatibility version = %d, want %d", got, testABIVersionCompatibility)
	}
	if got := abiVersionMajor(); got != testABIVersionMajor {
		t.Fatalf("major version = %d, want %d", got, testABIVersionMajor)
	}
	if got := abiVersionMinor(); got != testABIVersionMinor {
		t.Fatalf("minor version = %d, want %d", got, testABIVersionMinor)
	}
	if got := abiVersionPatch(); got != testABIVersionPatch {
		t.Fatalf("patch version = %d, want %d", got, testABIVersionPatch)
	}
	if got := abiVersionPacked(); got != testABIVersionPacked {
		t.Fatalf("packed version = %#x, want %#x", got, testABIVersionPacked)
	}
	if got := abiCapabilityCount(); got != testABICapabilityCount {
		t.Fatalf("capability count = %d, want %d", got, testABICapabilityCount)
	}
	if got := abiCapabilityWordCount(); got != 1 {
		t.Fatalf("capability word count = %d, want 1", got)
	}
	if got := abiCapabilityWord(0); got != testABICapabilityWord0 {
		t.Fatalf("capability word 0 = %#x, want %#x", got, uint64(testABICapabilityWord0))
	}
	if got := abiCapabilityWord(1); got != 0 {
		t.Fatalf("capability word 1 = %#x, want 0", got)
	}

	for capabilityID := uint32(0); capabilityID < testABICapabilityCount; capabilityID++ {
		if !abiHasCapability(capabilityID) {
			t.Fatalf("capability %d reported unsupported", capabilityID)
		}
	}
	for _, capabilityID := range []uint32{
		testABICapabilityCount,
		63,
		64,
		127,
		128,
	} {
		if abiHasCapability(capabilityID) {
			t.Fatalf("capability %d reported supported", capabilityID)
		}
	}
}

func TestABIDiscoveryQueriesAreConcurrentAndStable(t *testing.T) {
	const goroutines = 32
	const iterations = 1024

	errs := make(chan string, 1)
	report := func(format string, args ...any) {
		select {
		case errs <- fmt.Sprintf(format, args...):
		default:
		}
	}

	var wait sync.WaitGroup
	wait.Add(goroutines)
	for range goroutines {
		go func() {
			defer wait.Done()
			for range iterations {
				if abiVersionCompatibility() != testABIVersionCompatibility {
					report("compatibility version changed")
					return
				}
				if abiVersionMajor() != testABIVersionMajor ||
					abiVersionMinor() != testABIVersionMinor ||
					abiVersionPatch() != testABIVersionPatch ||
					abiVersionPacked() != testABIVersionPacked {
					report("version tuple changed")
					return
				}
				if abiCapabilityCount() != testABICapabilityCount ||
					abiCapabilityWordCount() != 1 ||
					abiCapabilityWord(0) != testABICapabilityWord0 ||
					abiCapabilityWord(1) != 0 {
					report("capability word changed")
					return
				}
				if !abiHasCapability(0) ||
					!abiHasCapability(15) ||
					!abiHasCapability(16) ||
					!abiHasCapability(17) ||
					!abiHasCapability(18) ||
					!abiHasCapability(19) ||
					!abiHasCapability(20) ||
					!abiHasCapability(21) ||
					!abiHasCapability(22) ||
					!abiHasCapability(23) ||
					!abiHasCapability(24) ||
					!abiHasCapability(25) ||
					!abiHasCapability(26) ||
					!abiHasCapability(27) ||
					!abiHasCapability(28) ||
					!abiHasCapability(29) ||
					!abiHasCapability(30) ||
					!abiHasCapability(31) ||
					abiHasCapability(32) ||
					abiHasCapability(64) {
					report("capability support changed")
					return
				}
			}
		}()
	}
	wait.Wait()

	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
}
