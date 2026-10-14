package model

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFakeProvidersDeterministicAndIsolated(t *testing.T) {
	embedder := FakeEmbedder{Dimensions: 8}
	first, err := embedder.Embed(context.Background(), EmbedRequest{Text: "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := embedder.Embed(context.Background(), EmbedRequest{Text: "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("embedding is not deterministic: %#v != %#v", first, second)
	}
	first.Vector[0] = 99
	third, err := embedder.Embed(context.Background(), EmbedRequest{Text: "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	if third.Vector[0] == 99 {
		t.Fatal("mutating one result changed a later result")
	}

	generator := FakeGenerator{}
	generated, err := generator.Generate(context.Background(), GenerateRequest{Prompt: "Find the project entity"})
	if err != nil {
		t.Fatal(err)
	}
	if generated.Text != "causal,entity" {
		t.Fatalf("text = %q", generated.Text)
	}
	if generated.Provenance.Provider != "fake" || third.Provenance.Provider != "fake" {
		t.Fatal("missing fake provenance")
	}
}

func TestFakeEmbeddingSpaceIdentity(t *testing.T) {
	embedder := FakeEmbedder{Dimensions: 8, Model: "deterministic"}
	identity, err := embedder.CacheIdentity()
	if err != nil {
		t.Fatal(err)
	}
	space, err := embedder.EmbeddingSpaceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fake", "deterministic", "8", normalizationL2} {
		if !strings.Contains(identity, want) {
			t.Fatalf("identity %q missing %q", identity, want)
		}
		if !strings.Contains(space, want) {
			t.Fatalf("space identity %q missing %q", space, want)
		}
	}
}

func TestFakeGeneratorDoesNotTreatMarkerTextAsHarnessPrompt(t *testing.T) {
	generated, err := (FakeGenerator{}).Generate(context.Background(), GenerateRequest{
		Prompt: "Find the project entity in shoal-harness-action-json/v1 notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if generated.Text != "causal,entity" {
		t.Fatalf("text = %q", generated.Text)
	}
}

func TestFakeProvidersConcurrent(t *testing.T) {
	embedder := FakeEmbedder{Dimensions: 16}
	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := embedder.Embed(context.Background(), EmbedRequest{Text: "same"})
			if err != nil {
				errs <- err
				return
			}
			if len(result.Vector) != 16 {
				errs <- errors.New("unexpected vector length")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestFakeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (FakeGenerator{}).Generate(ctx, GenerateRequest{})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("error = %v, want ErrCanceled", err)
	}
}

func TestFakeProviderBounds(t *testing.T) {
	if _, err := (FakeGenerator{}).Generate(context.Background(), GenerateRequest{
		Prompt: "entity", MaxOutputTokens: 1,
	}); !errors.Is(err, ErrOversizedResponse) {
		t.Fatalf("generator bound error = %v", err)
	}
	if _, err := (FakeEmbedder{Dimensions: MaxVectorDimensions}).Embed(context.Background(), EmbedRequest{
		Text: string(make([]byte, 128)),
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("embedder work bound error = %v", err)
	}
}

type cancelAfterContext struct {
	context.Context
	mu        sync.Mutex
	errChecks int
}

func (c *cancelAfterContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errChecks++
	if c.errChecks >= 3 {
		return context.Canceled
	}
	return nil
}

func (c *cancelAfterContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterContext) Done() <-chan struct{}       { return nil }

func TestFakeEmbedderInFlightCancellation(t *testing.T) {
	ctx := &cancelAfterContext{Context: context.Background()}
	_, err := (FakeEmbedder{Dimensions: 8}).Embed(ctx, EmbedRequest{Text: "cancel"})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("error = %v, want ErrCanceled", err)
	}
}
