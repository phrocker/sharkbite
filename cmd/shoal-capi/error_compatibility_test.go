package main

import (
	"errors"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
	publichdfs "github.com/phrocker/shoal-oss/hdfs"
	"github.com/phrocker/shoal-oss/internal/storage"
)

func TestCompatibilityErrorClasses(t *testing.T) {
	tests := []struct {
		name   string
		status int32
		source int32
		compat int32
	}{
		{"client", 9, errorSourceClientException, errorCompatibilityClientException},
		{"closed", 6, errorSourceIllegalStateException, errorCompatibilityRuntimeError},
		{"cancelled", 7, errorSourceIterationInterruptedException, errorCompatibilityRuntimeError},
		{"runtime", 1, errorSourceRuntime, errorCompatibilityRuntimeError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, compat := compatibilityClassesForStatus(test.status)
			if source != test.source {
				t.Fatalf("source = %d, want %d", source, test.source)
			}
			if compat != test.compat {
				t.Fatalf("compatibility = %d, want %d", compat, test.compat)
			}
		})
	}
}

func TestCompatibilityErrorContextAndCodes(t *testing.T) {
	source, compat := compatibilityClassesForError(
		9,
		&publichdfs.Error{Op: "open", Path: "/missing", Err: storage.ErrNotFound},
	)
	if source != errorSourceHDFSException || compat != errorCompatibilityRuntimeError {
		t.Fatalf("HDFS classification = (%d, %d)", source, compat)
	}
	source, compat = compatibilityClassesForError(
		1,
		errors.New("invalid"),
	)
	if source != errorSourceIllegalArgumentException || compat != errorCompatibilityRuntimeError {
		t.Fatalf("argument classification = (%d, %d)", source, compat)
	}
	if got := compatibilityCodeForError(accumulo.ErrTableNotFound); got != 9 {
		t.Fatalf("table code = %d", got)
	}
	if got := compatibilityCodeForError(accumulo.ErrBadCredentials); got != 5 {
		t.Fatalf("credentials code = %d", got)
	}
}

func TestCompatibilityErrorClassificationIsConcurrent(t *testing.T) {
	const goroutines = 32
	const iterations = 1024
	var wait sync.WaitGroup
	wait.Add(goroutines)
	for range goroutines {
		go func() {
			defer wait.Done()
			for range iterations {
				source, compat := compatibilityClassesForStatus(7)
				if source != errorSourceIterationInterruptedException ||
					compat != errorCompatibilityRuntimeError {
					t.Error("classification changed")
					return
				}
			}
		}()
	}
	wait.Wait()
}
