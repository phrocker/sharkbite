package agentmem

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type Entity struct {
	ID    string
	Label string
	Type  string
}

type Enricher interface {
	Entities(ctx context.Context, text string) ([]Entity, error)
	Summarize(ctx context.Context, text string) (string, error)
}

type HeuristicEnricher struct{}

// LLMEnricher is retained for compatibility with the legacy flat entity API.
// Deprecated: use Client.PlanEnrichment for ontology-guided extraction.
type LLMEnricher struct {
	LLM      LLM
	Fallback Enricher
}

var entityStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "after": true, "because": true, "before": true,
	"but": true, "for": true, "from": true, "how": true, "in": true, "into": true,
	"of": true, "on": true, "or": true, "the": true, "then": true, "this": true,
	"that": true, "to": true, "was": true, "were": true, "what": true, "when": true,
	"who": true, "why": true, "with": true,
}

var entityGazetteer = map[string]string{
	"github":        "ORGANIZATION",
	"google":        "ORGANIZATION",
	"microsoft":     "ORGANIZATION",
	"openai":        "ORGANIZATION",
	"amazon":        "ORGANIZATION",
	"london":        "LOCATION",
	"new york":      "LOCATION",
	"paris":         "LOCATION",
	"san francisco": "LOCATION",
}

var personNameGazetteer = map[string]bool{
	"alice": true, "bob": true, "carol": true, "dave": true, "eve": true,
}

var nonAlnumRE = regexp.MustCompile(`[^a-z0-9]+`)
var underscoreRE = regexp.MustCompile(`_+`)

func (HeuristicEnricher) Entities(_ context.Context, text string) ([]Entity, error) {
	toks := entityTokens(text)
	seen := map[string]Entity{}
	for i := 0; i < len(toks); {
		if !isCapitalized(toks[i].text) {
			i++
			continue
		}
		if toks[i].sentenceStart && entityStopwords[strings.ToLower(toks[i].text)] {
			i++
			continue
		}
		j := i + 1
		for j < len(toks) && isCapitalized(toks[j].text) && !entityStopwords[strings.ToLower(toks[j].text)] {
			j++
		}
		label := joinEntityTokens(toks[i:j])
		typ := entityType(label)
		ent := Entity{ID: canonicalEntityID(typ, label), Label: label, Type: typ}
		if ent.ID != strings.ToLower(typ)+":" {
			seen[ent.ID] = ent
		}
		i = j
	}
	out := make([]Entity, 0, len(seen))
	for _, ent := range seen {
		out = append(out, ent)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (HeuristicEnricher) Summarize(_ context.Context, text string) (string, error) {
	s := firstSentence(strings.TrimSpace(text))
	if len(s) <= 200 {
		return s, nil
	}
	return truncateWordBoundary(s, 200), nil
}

func (l LLMEnricher) Entities(ctx context.Context, text string) ([]Entity, error) {
	if l.LLM == nil {
		return fallbackEnricher(l).Entities(ctx, text)
	}
	resp, err := l.LLM.Infer(ctx, "Extract named entities from the text as a JSON array. Each object must have id, label, and type. Types must be PERSON, ORGANIZATION, LOCATION, or CONCEPT. Use canonical ids like person:john_doe. Return only JSON.\n\nText:\n"+text)
	if err != nil {
		return nil, err
	}
	ents, err := parseLLMEntities(resp)
	if err != nil {
		return nil, err
	}
	return ents, nil
}

func (l LLMEnricher) Summarize(ctx context.Context, text string) (string, error) {
	if l.LLM == nil {
		return fallbackEnricher(l).Summarize(ctx, text)
	}
	resp, err := l.LLM.Infer(ctx, "Write a concise 1-2 sentence summary of the following text. Return only the summary.\n\nText:\n"+text)
	if err != nil {
		return fallbackEnricher(l).Summarize(ctx, text)
	}
	resp = strings.TrimSpace(resp)
	if resp == "" {
		return fallbackEnricher(l).Summarize(ctx, text)
	}
	return resp, nil
}

type entityToken struct {
	text          string
	sentenceStart bool
}

func entityTokens(text string) []entityToken {
	var toks []entityToken
	var b strings.Builder
	sentenceStart := true
	wordStart := true
	flush := func() {
		if b.Len() == 0 {
			return
		}
		toks = append(toks, entityToken{text: b.String(), sentenceStart: wordStart})
		b.Reset()
		sentenceStart = false
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' {
			if b.Len() == 0 {
				wordStart = sentenceStart
			}
			b.WriteRune(r)
			continue
		}
		flush()
		if r == '.' || r == '!' || r == '?' {
			sentenceStart = true
		}
	}
	flush()
	return toks
}

func isCapitalized(s string) bool {
	for _, r := range s {
		return unicode.IsUpper(r)
	}
	return false
}

func joinEntityTokens(toks []entityToken) string {
	parts := make([]string, len(toks))
	for i, tok := range toks {
		parts[i] = tok.text
	}
	return strings.Join(parts, " ")
}

func entityType(label string) string {
	lower := strings.ToLower(label)
	if typ, ok := entityGazetteer[lower]; ok {
		return typ
	}
	parts := strings.Fields(lower)
	for _, part := range parts {
		if typ, ok := entityGazetteer[part]; ok {
			return typ
		}
	}
	if len(parts) > 0 && personNameGazetteer[parts[0]] {
		return "PERSON"
	}
	if strings.HasSuffix(label, " Inc") || strings.HasSuffix(label, " Corp") || strings.HasSuffix(label, " LLC") {
		return "ORGANIZATION"
	}
	return "CONCEPT"
}

func canonicalEntityID(typ, label string) string {
	slug := strings.ToLower(strings.TrimSpace(label))
	slug = nonAlnumRE.ReplaceAllString(slug, "_")
	slug = underscoreRE.ReplaceAllString(slug, "_")
	slug = strings.Trim(slug, "_")
	return strings.ToLower(strings.TrimSpace(typ)) + ":" + slug
}

func firstSentence(text string) string {
	for i, r := range text {
		if r == '.' || r == '!' || r == '?' {
			return strings.TrimSpace(text[:i+len(string(r))])
		}
	}
	return text
}

func truncateWordBoundary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut] != ' ' && s[cut] != '\t' && s[cut] != '\n' {
		cut--
	}
	if cut < max/2 {
		cut = max
	}
	return strings.TrimSpace(s[:cut])
}

func fallbackEnricher(l LLMEnricher) Enricher {
	if l.Fallback != nil {
		return l.Fallback
	}
	return HeuristicEnricher{}
}

func parseLLMEntities(resp string) ([]Entity, error) {
	if len(resp) > 1<<20 {
		return nil, errors.New("agentmem: entity response exceeds byte limit")
	}
	var raw []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Type  string `json:"type"`
	}
	decoder := json.NewDecoder(strings.NewReader(resp))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("agentmem: entity response contains trailing JSON")
	}
	seen := map[string]Entity{}
	for _, r := range raw {
		label := strings.TrimSpace(r.Label)
		if label == "" {
			continue
		}
		typ := normalizeEntityType(r.Type)
		id := strings.TrimSpace(r.ID)
		if id == "" || !strings.Contains(id, ":") {
			id = canonicalEntityID(typ, label)
		}
		seen[id] = Entity{ID: id, Label: label, Type: typ}
	}
	out := make([]Entity, 0, len(seen))
	for _, ent := range seen {
		out = append(out, ent)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func normalizeEntityType(typ string) string {
	switch strings.ToUpper(strings.TrimSpace(typ)) {
	case "PERSON":
		return "PERSON"
	case "ORG", "ORGANIZATION", "ORGANISATION":
		return "ORGANIZATION"
	case "LOCATION", "PLACE":
		return "LOCATION"
	case "CONCEPT", "":
		return "CONCEPT"
	default:
		return "CONCEPT"
	}
}
