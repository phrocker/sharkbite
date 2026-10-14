package model

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"strings"
)

const (
	DefaultLexicalDimensions = 256
	DefaultLexicalModel      = "hashing-lexical-v1"
	lexicalProvider          = "local-lexical"
)

type LexicalConfig struct {
	Dimensions   int
	Model        string
	MaxTextBytes int64
}

// LexicalEmbedder is a pure-Go, cgo-free, zero-dependency offline and CI
// fallback. It hashes lexical tokens and byte n-grams into deterministic
// statistical vectors. It makes no semantic quality claim and is not a
// recommended replacement for a real embedding model.
type LexicalEmbedder struct {
	cfg LexicalConfig
}

func NewLexicalEmbedder(cfg LexicalConfig) (*LexicalEmbedder, error) {
	normalized, err := validateLexicalConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &LexicalEmbedder{cfg: normalized}, nil
}

func (l *LexicalEmbedder) CacheIdentity() (string, error) {
	if l == nil {
		return "", ErrInvalidConfig
	}
	space, err := l.EmbeddingSpaceIdentity()
	if err != nil {
		return "", err
	}
	cfg, err := validateLexicalConfig(l.cfg)
	if err != nil {
		return "", err
	}
	return framedModelIdentity(
		"model-local-lexical-embedder-v1",
		space,
		strconv.FormatInt(cfg.MaxTextBytes, 10),
	), nil
}

func (l *LexicalEmbedder) EmbeddingSpaceIdentity() (string, error) {
	if l == nil {
		return "", ErrInvalidConfig
	}
	cfg, err := validateLexicalConfig(l.cfg)
	if err != nil {
		return "", err
	}
	return embeddingSpaceIdentity(lexicalProvider, cfg.Model, cfg.Dimensions, normalizationL2)
}

func (l *LexicalEmbedder) Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error) {
	const op = "local lexical embed"
	if l == nil {
		return EmbedResult{}, &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	if err := ctx.Err(); err != nil {
		return EmbedResult{}, contextError(op, err)
	}
	cfg, err := validateLexicalConfig(l.cfg)
	if err != nil {
		return EmbedResult{}, err
	}
	if err := validateTextRequest(op, req.Text, 0, cfg.MaxTextBytes); err != nil {
		return EmbedResult{}, err
	}
	vec := make([]float32, cfg.Dimensions)
	addLexicalFeature(vec, "bias", "document", 1)
	var token []byte
	featureChecks := 0
	checkCanceled := func() error {
		featureChecks++
		if featureChecks&255 == 0 {
			if err := ctx.Err(); err != nil {
				return contextError(op, err)
			}
		}
		return nil
	}
	flush := func() error {
		if len(token) == 0 {
			return nil
		}
		term := string(token)
		addLexicalFeature(vec, "token", term, 1)
		if err := checkCanceled(); err != nil {
			return err
		}
		framed := "#" + term + "#"
		if len(framed) <= 3 {
			addLexicalFeature(vec, "ngram", framed, 0.5)
			if err := checkCanceled(); err != nil {
				return err
			}
		} else {
			for i := 0; i+3 <= len(framed); i++ {
				addLexicalFeature(vec, "ngram", framed[i:i+3], 0.5)
				if err := checkCanceled(); err != nil {
					return err
				}
			}
		}
		token = token[:0]
		return nil
	}
	for i := 0; i < len(req.Text); i++ {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return EmbedResult{}, contextError(op, err)
			}
		}
		b := req.Text[i]
		switch {
		case b >= 'A' && b <= 'Z':
			token = append(token, b+'a'-'A')
		case (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b >= 0x80:
			token = append(token, b)
		default:
			if err := flush(); err != nil {
				return EmbedResult{}, err
			}
		}
	}
	if err := flush(); err != nil {
		return EmbedResult{}, err
	}
	normalize(vec)
	return EmbedResult{
		Vector:     append([]float32(nil), vec...),
		Provenance: Provenance{Provider: lexicalProvider, Model: cfg.Model},
		Usage:      Usage{InputTokens: tokenEstimate(req.Text), TotalTokens: tokenEstimate(req.Text)},
	}, nil
}

func validateLexicalConfig(cfg LexicalConfig) (LexicalConfig, error) {
	const op = "configure local lexical"
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.Model == "" {
		cfg.Model = DefaultLexicalModel
	}
	if cfg.Dimensions == 0 {
		cfg.Dimensions = DefaultLexicalDimensions
	}
	if cfg.MaxTextBytes == 0 {
		cfg.MaxTextBytes = DefaultMaxTextBytes
	}
	if !validConfigValue(cfg.Model, maxModelBytes) ||
		cfg.Dimensions < 1 || cfg.Dimensions > MaxVectorDimensions ||
		cfg.MaxTextBytes < 1 || cfg.MaxTextBytes > maxConfiguredTextBytes {
		return LexicalConfig{}, &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	return cfg, nil
}

func addLexicalFeature(vec []float32, namespace, feature string, weight float32) {
	h := sha256.New()
	_, _ = h.Write([]byte(namespace))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(feature))
	sum := h.Sum(nil)
	idx := int(binary.BigEndian.Uint64(sum[:8]) % uint64(len(vec)))
	sign := float32(1)
	if sum[8]&1 != 0 {
		sign = -1
	}
	vec[idx] += sign * weight
}

var (
	_ Embedder                       = (*LexicalEmbedder)(nil)
	_ EmbeddingSpaceIdentityProvider = (*LexicalEmbedder)(nil)
)

// EgressesOffHost is false: the lexical embedder is a pure function of its
// input and reaches no endpoint.
func (*LexicalEmbedder) EgressesOffHost() bool { return false }
