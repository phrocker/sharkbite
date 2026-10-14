package agentmem

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	modelio "github.com/phrocker/shoal-oss/pkg/model"
)

type FakeEmbedder struct{ Dim int }

func (f FakeEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	dim := f.Dim
	if dim <= 0 {
		dim = DefaultDim
	}
	result, err := (modelio.FakeEmbedder{Dimensions: dim}).Embed(ctx, modelio.EmbedRequest{Text: text})
	return result.Vector, err
}

func (f FakeEmbedder) EmbeddingSpaceIdentity() (string, error) {
	dim := f.Dim
	if dim <= 0 {
		dim = DefaultDim
	}
	return (modelio.FakeEmbedder{Dimensions: dim}).EmbeddingSpaceIdentity()
}

type FakeLLM struct{}

func (FakeLLM) Infer(ctx context.Context, prompt string) (string, error) {
	result, err := (modelio.FakeGenerator{}).Generate(ctx, modelio.GenerateRequest{Prompt: prompt})
	return result.Text, err
}

type RuleClassifier struct{}

func (RuleClassifier) Classify(q string) Intent {
	q = strings.ToLower(q)
	if hasWord(q, "why", "because", "cause", "caused", "reason") {
		return IntentWhy
	}
	if hasWord(q, "when", "before", "after", "date", "time", "timeline", "yesterday", "today") {
		return IntentWhen
	}
	if hasWord(q, "who", "entity", "entities", "person", "people", "project", "service") {
		return IntentEntity
	}
	return IntentGeneral
}

func hasWord(s string, words ...string) bool {
	fields := strings.FieldsFunc(s, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
	set := map[string]struct{}{}
	for _, f := range fields {
		set[f] = struct{}{}
	}
	for _, w := range words {
		if _, ok := set[w]; ok {
			return true
		}
	}
	return false
}

var keywordRE = regexp.MustCompile(`[a-zA-Z0-9]+`)
var stopWords = map[string]bool{"the": true, "and": true, "for": true, "with": true, "that": true, "this": true, "what": true, "when": true, "why": true, "who": true, "how": true, "are": true, "was": true, "were": true, "did": true, "does": true, "from": true, "into": true, "about": true, "before": true, "after": true, "because": true}

func Keywords(text string) []string {
	matches := keywordRE.FindAllString(strings.ToLower(text), -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		if len(m) < 3 || stopWords[m] || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Strings(out)
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// IDGenerator emits lexicographically sortable event ids: UTC milliseconds
// formatted as yyyyMMddHHmmssmmm followed by a process-local monotonic counter.
// Therefore evt:<id> row order is temporal order without engine schema knowledge.
type IDGenerator struct {
	last atomic.Int64
	mu   sync.Mutex
	seq  uint32
}

func NewIDGenerator() *IDGenerator { return &IDGenerator{} }
func (g *IDGenerator) New(t time.Time) string {
	ms := unixMillis(t)
	for {
		prev := g.last.Load()
		next := ms
		if next < prev {
			next = prev
		}
		if g.last.CompareAndSwap(prev, next) {
			ms = next
			break
		}
	}
	g.mu.Lock()
	g.seq++
	seq := g.seq
	g.mu.Unlock()
	stamp := time.UnixMilli(ms).UTC().Format("20060102150405.000")
	return fmt.Sprintf("%s-%08x", stamp, seq)
}

func normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x * x)
	}
	if sum == 0 {
		return
	}
	n := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= n
	}
}

func cosine(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, aa, bb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i] * b[i])
		aa += float64(a[i] * a[i])
		bb += float64(b[i] * b[i])
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return float32(dot / math.Sqrt(aa*bb))
}

func unpackScore(raw []byte) float32 {
	if f, ok := unpackF32(raw); ok {
		return f
	}
	return 0
}
func unpackF32(raw []byte) (float32, bool) {
	if len(raw) != 4 {
		return 0, false
	}
	return math.Float32frombits(binary.BigEndian.Uint32(raw)), true
}
