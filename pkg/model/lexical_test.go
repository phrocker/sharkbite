package model

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLexicalEmbedderDeterministicAndIsolated(t *testing.T) {
	embedder, err := NewLexicalEmbedder(LexicalConfig{Dimensions: 32, Model: "lex-v1"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := embedder.Embed(context.Background(), EmbedRequest{Text: "The QUICK brown fox"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := embedder.Embed(context.Background(), EmbedRequest{Text: "The QUICK brown fox"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("embedding is not deterministic: %#v != %#v", first, second)
	}
	if len(first.Vector) != 32 || first.Provenance != (Provenance{Provider: lexicalProvider, Model: "lex-v1"}) {
		t.Fatalf("result = %#v", first)
	}
	first.Vector[0] = 99
	third, err := embedder.Embed(context.Background(), EmbedRequest{Text: "The QUICK brown fox"})
	if err != nil {
		t.Fatal(err)
	}
	if third.Vector[0] == 99 {
		t.Fatal("mutating one result changed a later result")
	}
}

func TestLexicalConfigAndRequestValidation(t *testing.T) {
	for _, cfg := range []LexicalConfig{
		{Dimensions: -1},
		{Dimensions: MaxVectorDimensions + 1},
		{Model: "bad\nmodel"},
		{MaxTextBytes: maxConfiguredTextBytes + 1},
	} {
		if _, err := NewLexicalEmbedder(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config %#v error = %v, want ErrInvalidConfig", cfg, err)
		}
	}
	embedder, err := NewLexicalEmbedder(LexicalConfig{Dimensions: 8, MaxTextBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context.Background(), EmbedRequest{Text: "four"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("request error = %v, want ErrInvalidRequest", err)
	}
	if _, err := embedder.Embed(context.Background(), EmbedRequest{Text: string([]byte{0xff})}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid UTF-8 request error = %v", err)
	}
}

func TestLexicalCacheIdentityIncludesDimensionAndNoSecrets(t *testing.T) {
	embedder, err := NewLexicalEmbedder(LexicalConfig{Dimensions: 17, Model: "offline"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := embedder.CacheIdentity()
	if err != nil {
		t.Fatal(err)
	}
	space, err := embedder.EmbeddingSpaceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{lexicalProvider, "offline", "17", normalizationL2} {
		if !strings.Contains(identity, want) {
			t.Fatalf("identity %q missing %q", identity, want)
		}
		if !strings.Contains(space, want) {
			t.Fatalf("space identity %q missing %q", space, want)
		}
	}
}

func TestLexicalCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	embedder, err := NewLexicalEmbedder(LexicalConfig{Dimensions: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(ctx, EmbedRequest{Text: "cancel"}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestLexicalInFlightHashingCancellation(t *testing.T) {
	ctx := &cancelAfterNContext{Context: context.Background(), threshold: 40}
	embedder, err := NewLexicalEmbedder(LexicalConfig{Dimensions: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(ctx, EmbedRequest{Text: strings.Repeat("a", 8192)}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

type cancelAfterNContext struct {
	context.Context
	errChecks int
	threshold int
}

func (c *cancelAfterNContext) Err() error {
	c.errChecks++
	if c.errChecks >= c.threshold {
		return context.Canceled
	}
	return nil
}
