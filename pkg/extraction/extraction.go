// Package extraction orchestrates bounded, ontology-guided model extraction.
//
// It produces validated contracts and a publication plan. It deliberately
// does not write storage so callers must publish through an atomic Explorer
// boundary.
package extraction

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	PromptTemplateID      = "shoal.ontology-extraction"
	PromptVersion         = "v1"
	ExtractorName         = "shoal-ontology-extractor"
	ExtractorVersion      = "v1"
	HeuristicProvider     = "heuristic"
	HeuristicModel        = "deterministic-capitalized-entities"
	DefaultMaxOutputBytes = 1 << 20
	DefaultMaxPromptBytes = 1 << 20
	DefaultMaxDepth       = 24
	DefaultMaxEntities    = 1024
	DefaultMaxRelations   = 2048
	DefaultMaxProperties  = 256
	DefaultMaxStringBytes = 64 << 10

	GraphPropertyOntologyConceptID  = "shoal.ontology.concept_id"
	GraphPropertyOntologyConceptKey = "shoal.ontology.concept_key"
	GraphPropertyEntityKey          = "shoal.ontology.entity_key"
	GraphPropertyEntityNamespace    = "shoal.ontology.entity_namespace"
)

var (
	entityKeyPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	heuristicKeySanitizer = regexp.MustCompile(`[^a-zA-Z0-9]+`)
)

type Limits struct {
	MaxOutputBytes  int
	MaxPromptBytes  int
	MaxDepth        int
	MaxEntities     int
	MaxRelations    int
	MaxProperties   int
	MaxStringBytes  int
	MaxOutputTokens int
}

func DefaultLimits() Limits {
	return Limits{
		MaxOutputBytes:  DefaultMaxOutputBytes,
		MaxPromptBytes:  DefaultMaxPromptBytes,
		MaxDepth:        DefaultMaxDepth,
		MaxEntities:     DefaultMaxEntities,
		MaxRelations:    DefaultMaxRelations,
		MaxProperties:   DefaultMaxProperties,
		MaxStringBytes:  DefaultMaxStringBytes,
		MaxOutputTokens: 4096,
	}
}

func (l Limits) normalized() (Limits, error) {
	d := DefaultLimits()
	if l.MaxOutputBytes == 0 {
		l.MaxOutputBytes = d.MaxOutputBytes
	}
	if l.MaxPromptBytes == 0 {
		l.MaxPromptBytes = d.MaxPromptBytes
	}
	if l.MaxDepth == 0 {
		l.MaxDepth = d.MaxDepth
	}
	if l.MaxEntities == 0 {
		l.MaxEntities = d.MaxEntities
	}
	if l.MaxRelations == 0 {
		l.MaxRelations = d.MaxRelations
	}
	if l.MaxProperties == 0 {
		l.MaxProperties = d.MaxProperties
	}
	if l.MaxStringBytes == 0 {
		l.MaxStringBytes = d.MaxStringBytes
	}
	if l.MaxOutputTokens == 0 {
		l.MaxOutputTokens = d.MaxOutputTokens
	}
	if l.MaxOutputBytes < 2 || l.MaxOutputBytes > 16<<20 ||
		l.MaxPromptBytes < 2 || l.MaxPromptBytes > 16<<20 ||
		l.MaxDepth < 1 || l.MaxDepth > 128 ||
		l.MaxEntities < 1 || l.MaxEntities > 16384 ||
		l.MaxRelations < 1 || l.MaxRelations > 32768 ||
		l.MaxProperties < 1 || l.MaxProperties > 4096 ||
		l.MaxStringBytes < 1 || l.MaxStringBytes > 1<<20 ||
		l.MaxOutputTokens < 1 || l.MaxOutputTokens > 1<<20 {
		return Limits{}, errors.New("extraction: limits are outside safety bounds")
	}
	return l, nil
}

type Request struct {
	Version         ontology.OntologyVersion
	Context         inference.ContextPack
	Instructions    string
	EntityNamespace string
	Limits          Limits
}

func (r Request) Validate() error {
	if err := r.Version.Validate(); err != nil {
		return fmt.Errorf("extraction: ontology: %w", err)
	}
	if err := r.Context.Validate(); err != nil {
		return fmt.Errorf("extraction: context: %w", err)
	}
	identity, ok := r.Context.Ontology()
	if !ok || identity.SchemaID() != r.Version.Schema().ID() || identity.VersionID() != r.Version.ID() {
		return errors.New("extraction: context ontology does not match the requested schema")
	}
	if strings.TrimSpace(r.Instructions) == "" || !utf8.ValidString(r.Instructions) {
		return errors.New("extraction: valid instructions are required")
	}
	if len(r.Instructions) > int(ontology.DefaultExtractionLimits().MaxInstructionBytes) {
		return errors.New("extraction: instructions exceed the byte limit")
	}
	if !utf8.ValidString(r.EntityNamespace) || len(r.EntityNamespace) > 4096 {
		return errors.New("extraction: entity namespace must be valid bounded UTF-8")
	}
	_, err := r.Limits.normalized()
	return err
}

type State string
type Origin string
type Action string

const (
	StateProposed   State  = "proposed"
	OriginInferred  Origin = "inferred"
	ActionCreate    Action = "create"
	ActionReference Action = "reference"
)

type Provenance struct {
	Provider         string
	Model            string
	PromptID         string
	PromptVersion    string
	PromptHash       string
	Extractor        string
	ExtractorVersion string
}

type Property struct {
	DefinitionID    shoal.ID
	Value           ontology.Value
	ReferenceNodeID shoal.ID
}

type Entity struct {
	ID             shoal.ID
	ContractID     shoal.ID
	Key            string
	TypeID         shoal.ID
	ExistingNodeID shoal.ID
	Action         Action
	State          State
	Origin         Origin
	Confidence     shoal.Score
	EvidenceIDs    []shoal.ID
	Properties     []Property
	Provenance     Provenance
}

type Edge struct {
	ID             shoal.ID
	TypeID         shoal.ID
	From           shoal.ID
	To             shoal.ID
	FromContractID shoal.ID
	ToContractID   shoal.ID
	State          State
	Origin         Origin
	Confidence     shoal.Score
	EvidenceIDs    []shoal.ID
	Properties     []Property
	Provenance     Provenance
}

type PublicationPlan struct {
	State      State
	Origin     Origin
	Entities   []Entity
	Edges      []Edge
	ClaimIDs   []shoal.ID
	Provenance Provenance
}

type Result struct {
	ontology    ontology.ExtractionResult
	inference   inference.InferenceResult
	publication PublicationPlan
	prompt      string
}

func (r Result) OntologyResult() ontology.ExtractionResult  { return r.ontology }
func (r Result) InferenceResult() inference.InferenceResult { return r.inference }
func (r Result) Prompt() string                             { return r.prompt }
func (r Result) PublicationPlan() PublicationPlan           { return clonePlan(r.publication) }

type Orchestrator struct {
	Generator model.TextGenerator
}

func (o Orchestrator) Extract(ctx context.Context, request Request) (Result, error) {
	if o.Generator == nil {
		return Result{}, errors.New("extraction: text generator is required")
	}
	if err := request.Validate(); err != nil {
		return Result{}, err
	}
	limits, _ := request.Limits.normalized()
	prompt, err := BuildPrompt(request)
	if err != nil {
		return Result{}, err
	}
	generated, err := o.Generator.Generate(ctx, model.GenerateRequest{
		Prompt: prompt, MaxOutputTokens: limits.MaxOutputTokens,
	})
	if err != nil {
		return Result{}, fmt.Errorf("extraction: generate: %w", err)
	}
	if strings.TrimSpace(generated.Provenance.Provider) == "" ||
		strings.TrimSpace(generated.Provenance.Model) == "" {
		return Result{}, errors.New("extraction: generator returned incomplete provenance")
	}
	raw, err := parseOutput(generated.Text, limits)
	if err != nil {
		return Result{}, err
	}
	return materialize(request, prompt, generated.Provenance, raw, "model")
}

func BuildPrompt(request Request) (string, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}
	type promptProperty struct {
		ID          string   `json:"id"`
		Key         string   `json:"key"`
		Type        string   `json:"type"`
		Constraints []string `json:"constraints,omitempty"`
	}
	type promptConcept struct {
		ID         string   `json:"id"`
		Key        string   `json:"key"`
		Properties []string `json:"properties,omitempty"`
	}
	type promptRelation struct {
		ID         string   `json:"id"`
		Key        string   `json:"key"`
		From       []string `json:"from"`
		To         []string `json:"to"`
		Directed   bool     `json:"directed"`
		Properties []string `json:"properties,omitempty"`
	}
	type promptAnchor struct {
		ID    string   `json:"id"`
		Kind  string   `json:"kind"`
		Quote string   `json:"quote,omitempty"`
		Nodes []string `json:"nodes,omitempty"`
		Edges []string `json:"edges,omitempty"`
	}
	type payload struct {
		SchemaID     string           `json:"schema_id"`
		VersionID    string           `json:"version_id"`
		Instructions string           `json:"instructions"`
		Concepts     []promptConcept  `json:"concepts"`
		Properties   []promptProperty `json:"properties"`
		Relations    []promptRelation `json:"relations"`
		Evidence     []promptAnchor   `json:"evidence"`
	}
	p := payload{
		SchemaID:     string(request.Version.Schema().ID()),
		VersionID:    string(request.Version.ID()),
		Instructions: request.Instructions,
	}
	for _, property := range request.Version.Properties() {
		pp := promptProperty{ID: string(property.ID()), Key: property.Key(), Type: string(property.ValueType())}
		for _, constraint := range property.Constraints() {
			pp.Constraints = append(pp.Constraints, constraintText(constraint))
		}
		p.Properties = append(p.Properties, pp)
	}
	for _, concept := range request.Version.Concepts() {
		pc := promptConcept{ID: string(concept.ID()), Key: concept.Key()}
		for _, id := range concept.Properties() {
			pc.Properties = append(pc.Properties, string(id))
		}
		p.Concepts = append(p.Concepts, pc)
	}
	for _, relation := range request.Version.Relationships() {
		pr := promptRelation{ID: string(relation.ID()), Key: relation.Key(), Directed: relation.Directed()}
		for _, id := range relation.FromConcepts() {
			pr.From = append(pr.From, string(id))
		}
		for _, id := range relation.ToConcepts() {
			pr.To = append(pr.To, string(id))
		}
		for _, id := range relation.Properties() {
			pr.Properties = append(pr.Properties, string(id))
		}
		p.Relations = append(p.Relations, pr)
	}
	for _, anchor := range request.Context.Evidence() {
		pa := promptAnchor{ID: string(anchor.ID()), Kind: string(anchor.Kind())}
		if _, quote, ok := anchor.Document(); ok {
			pa.Quote = quote
		} else if path, ok := anchor.Path(); ok {
			for _, node := range path.Nodes {
				pa.Nodes = append(pa.Nodes, graphNodeToken(node.ID))
			}
			for _, edge := range path.Edges {
				pa.Edges = append(pa.Edges,
					graphIDToken("edge", edge.ID)+":"+edge.Type+":"+
						graphNodeToken(edge.From)+"->"+graphNodeToken(edge.To))
			}
		}
		p.Evidence = append(p.Evidence, pa)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("extraction: encode prompt: %w", err)
	}
	prompt := "Return exactly one JSON object and no prose. Unknown fields are rejected. " +
		"Use only ontology IDs and evidence anchor IDs present below. Never invent node IDs. " +
		"Shape: {\"entities\":[{\"key\":\"stable-local-key\",\"type_id\":\"concept ID\",\"existing_node_id\":\"optional grounded graph node ID\",\"properties\":[{\"property_id\":\"property ID\",\"value\":{\"type\":\"string|integer|number|boolean|timestamp|reference\",\"value\":...}}],\"confidence\":0..1,\"evidence_anchor_ids\":[\"anchor ID\"]}],\"relations\":[{\"type_id\":\"relationship ID\",\"from_entity_key\":\"key\",\"to_entity_key\":\"key\",\"properties\":[],\"confidence\":0..1,\"evidence_anchor_ids\":[\"anchor ID\"]}]}. " +
		"Every entity and relation requires exact evidence anchors.\nINPUT=" + string(body)
	limits, _ := request.Limits.normalized()
	if len(prompt) > limits.MaxPromptBytes {
		return "", errors.New("extraction: prompt exceeds byte limit")
	}
	return prompt, nil
}

func constraintText(c ontology.Constraint) string {
	switch c.Kind() {
	case ontology.ConstraintMinimumCount, ontology.ConstraintMaximumCount:
		n, _ := c.Count()
		return string(c.Kind()) + "=" + strconv.FormatUint(uint64(n), 10)
	case ontology.ConstraintMinimumValue, ontology.ConstraintMaximumValue:
		v, _ := c.Value()
		return string(c.Kind()) + "=" + valueText(v)
	case ontology.ConstraintPattern:
		v, _ := c.Pattern()
		return string(c.Kind()) + "=" + v
	case ontology.ConstraintAllowedValues:
		values := c.AllowedValues()
		parts := make([]string, len(values))
		for i := range values {
			parts[i] = valueText(values[i])
		}
		return string(c.Kind()) + "=" + strings.Join(parts, ",")
	default:
		return string(c.Kind())
	}
}

type rawOutput struct {
	Entities  []rawEntity   `json:"entities"`
	Relations []rawRelation `json:"relations"`
}
type rawEntity struct {
	Key               string        `json:"key"`
	TypeID            string        `json:"type_id"`
	ExistingNodeID    string        `json:"existing_node_id,omitempty"`
	Properties        []rawProperty `json:"properties"`
	Confidence        *float64      `json:"confidence"`
	EvidenceAnchorIDs []string      `json:"evidence_anchor_ids"`
}
type rawRelation struct {
	TypeID            string        `json:"type_id"`
	FromKey           string        `json:"from_entity_key"`
	ToKey             string        `json:"to_entity_key"`
	Properties        []rawProperty `json:"properties"`
	Confidence        *float64      `json:"confidence"`
	EvidenceAnchorIDs []string      `json:"evidence_anchor_ids"`
}
type rawProperty struct {
	PropertyID string   `json:"property_id"`
	Value      rawValue `json:"value"`
}
type rawValue struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

func parseOutput(text string, limits Limits) (rawOutput, error) {
	if !utf8.ValidString(text) {
		return rawOutput{}, errors.New("extraction: output is not valid UTF-8")
	}
	if len(text) > limits.MaxOutputBytes {
		return rawOutput{}, errors.New("extraction: output exceeds byte limit")
	}
	if err := validateJSONDepth([]byte(text), limits.MaxDepth); err != nil {
		return rawOutput{}, err
	}
	if err := rejectDuplicateJSONFields([]byte(text)); err != nil {
		return rawOutput{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	var output rawOutput
	if err := decoder.Decode(&output); err != nil {
		return rawOutput{}, fmt.Errorf("extraction: malformed structured output: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return rawOutput{}, errors.New("extraction: output contains trailing JSON")
	}
	if len(output.Entities) > limits.MaxEntities || len(output.Relations) > limits.MaxRelations {
		return rawOutput{}, errors.New("extraction: output exceeds item count limit")
	}
	if output.Entities == nil || output.Relations == nil {
		return rawOutput{}, errors.New("extraction: entities and relations arrays are required")
	}
	for _, entity := range output.Entities {
		if entity.Properties == nil || entity.EvidenceAnchorIDs == nil || entity.Confidence == nil {
			return rawOutput{}, errors.New("extraction: entity is missing a required field")
		}
		if len(entity.Properties) > limits.MaxProperties {
			return rawOutput{}, errors.New("extraction: entity exceeds property count limit")
		}
	}
	for _, relation := range output.Relations {
		if relation.Properties == nil || relation.EvidenceAnchorIDs == nil || relation.Confidence == nil {
			return rawOutput{}, errors.New("extraction: relation is missing a required field")
		}
		if len(relation.Properties) > limits.MaxProperties {
			return rawOutput{}, errors.New("extraction: relation exceeds property count limit")
		}
	}
	if err := validateStrings(output, limits.MaxStringBytes); err != nil {
		return rawOutput{}, err
	}
	return output, nil
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("extraction: malformed object key")
				}
				if key != strings.ToLower(key) {
					return fmt.Errorf("extraction: JSON field %q does not use exact schema casing", key)
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("extraction: duplicate JSON field %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("extraction: malformed structured output")
		}
	}
	if err := walk(); err != nil {
		return fmt.Errorf("extraction: malformed structured output: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("extraction: output contains trailing JSON")
	}
	return nil
}

func validateJSONDepth(data []byte, maximum int) error {
	depth, inString, escaped := 0, false, false
	for _, b := range data {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if b == '\\' {
				escaped = true
				continue
			}
			if b == '"' {
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maximum {
				return errors.New("extraction: output exceeds nesting depth limit")
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return errors.New("extraction: malformed structured output")
			}
		}
	}
	if depth != 0 || inString {
		return errors.New("extraction: malformed structured output")
	}
	return nil
}

func validateStrings(output rawOutput, maximum int) error {
	check := func(values ...string) error {
		for _, value := range values {
			if !utf8.ValidString(value) || len(value) > maximum {
				return errors.New("extraction: output string exceeds safety limit")
			}
		}
		return nil
	}
	for _, entity := range output.Entities {
		if err := check(entity.Key, entity.TypeID, entity.ExistingNodeID); err != nil {
			return err
		}
		for _, id := range entity.EvidenceAnchorIDs {
			if err := check(id); err != nil {
				return err
			}
		}
		for _, property := range entity.Properties {
			if err := check(property.PropertyID, property.Value.Type, string(property.Value.Value)); err != nil {
				return err
			}
		}
	}
	for _, relation := range output.Relations {
		if err := check(relation.TypeID, relation.FromKey, relation.ToKey); err != nil {
			return err
		}
		for _, id := range relation.EvidenceAnchorIDs {
			if err := check(id); err != nil {
				return err
			}
		}
		for _, property := range relation.Properties {
			if err := check(property.PropertyID, property.Value.Type, string(property.Value.Value)); err != nil {
				return err
			}
		}
	}
	return nil
}

func materialize(request Request, prompt string, mp model.Provenance, raw rawOutput, mode string) (Result, error) {
	promptHash := sha256.Sum256([]byte(prompt))
	hash := "sha256:" + hex.EncodeToString(promptHash[:])
	provenance := Provenance{
		Provider: mp.Provider, Model: mp.Model, PromptID: PromptTemplateID,
		PromptVersion: PromptVersion, PromptHash: hash,
		Extractor: ExtractorName, ExtractorVersion: ExtractorVersion,
	}
	op, err := ontology.NewExtractionProvenance(
		mp.Provider, mp.Model, "unspecified", PromptTemplateID, PromptVersion,
		ExtractorName, ExtractorVersion, shoal.Metadata{"mode": mode, "prompt_hash": hash},
	)
	if err != nil {
		return Result{}, err
	}
	modelProv, err := inference.NewModelProvenance(mp.Provider, mp.Model, "unspecified", shoal.Metadata{"mode": mode}, nil)
	if err != nil {
		return Result{}, err
	}
	promptProv, err := inference.NewPromptProvenance(PromptTemplateID, PromptVersion, hash)
	if err != nil {
		return Result{}, err
	}
	evidenceByAnchor, ontologyEvidence, nodeAnchors, nodeTokens, err := evidenceMaps(request.Context)
	if err != nil {
		return Result{}, err
	}
	ontologyRequest, err := ontology.NewExtractionRequest(
		request.Version, ontologyEvidence, request.Instructions, op,
		extractionLimits(request), nil,
	)
	if err != nil {
		return Result{}, fmt.Errorf("extraction: ontology request: %w", err)
	}
	ontologyIdentity, err := ontology.NewOntologyIdentity(request.Version)
	if err != nil {
		return Result{}, fmt.Errorf("extraction: ontology identity: %w", err)
	}
	concepts := map[shoal.ID]ontology.ConceptDefinition{}
	properties := map[shoal.ID]ontology.PropertyDefinition{}
	relations := map[shoal.ID]ontology.RelationshipDefinition{}
	for _, v := range request.Version.Concepts() {
		concepts[v.ID()] = v
	}
	for _, v := range request.Version.Properties() {
		properties[v.ID()] = v
	}
	for _, v := range request.Version.Relationships() {
		relations[v.ID()] = v
	}

	seenKeys := map[string]struct{}{}
	seenEntityIDs := map[shoal.ID]string{}
	type entityBinding struct {
		entity     Entity
		contractID shoal.ID
	}
	entityByKey := map[string]entityBinding{}
	var assertions []ontology.Assertion
	var claims []inference.Claim
	for _, item := range raw.Entities {
		key := strings.ToLower(strings.TrimSpace(item.Key))
		if !entityKeyPattern.MatchString(item.Key) || item.Key != key {
			return Result{}, fmt.Errorf("extraction: invalid or non-canonical entity key %q", item.Key)
		}
		if _, duplicate := seenKeys[key]; duplicate {
			return Result{}, fmt.Errorf("extraction: duplicate entity key %q", key)
		}
		seenKeys[key] = struct{}{}
		typeID := shoal.ID(item.TypeID)
		if _, ok := concepts[typeID]; !ok {
			return Result{}, fmt.Errorf("extraction: unknown entity type %q", item.TypeID)
		}
		if err := validateEntityCardinality(concepts[typeID], item.Properties, properties); err != nil {
			return Result{}, fmt.Errorf("extraction: entity %q: %w", key, err)
		}
		if err := validateConfidence(*item.Confidence); err != nil {
			return Result{}, err
		}
		anchorIDs, refs, err := resolveEvidence(item.EvidenceAnchorIDs, evidenceByAnchor)
		if err != nil {
			return Result{}, fmt.Errorf("extraction: entity %q: %w", key, err)
		}
		var id shoal.ID
		var contractID shoal.ID
		action := ActionCreate
		var existing shoal.ID
		if item.ExistingNodeID != "" {
			existing, err = decodeGraphNodeToken(item.ExistingNodeID, nodeTokens)
			if err != nil {
				return Result{}, fmt.Errorf("extraction: entity %q: %w", key, err)
			}
			groundingAnchors, ok := nodeAnchors[existing]
			if !ok {
				return Result{}, fmt.Errorf("extraction: entity %q references an ungrounded node ID", key)
			}
			if !intersects(anchorIDs, groundingAnchors) {
				return Result{}, fmt.Errorf("extraction: entity %q omits the graph anchor grounding its node ID", key)
			}
			id, action = existing, ActionReference
			contractID, err = ontology.NewStableID("grounded-node", item.ExistingNodeID)
			if err != nil {
				return Result{}, err
			}
		} else {
			// This scoped type/key identity is load-bearing; TestAuthorizedExtractDocumentCrossTenantSharedEntityGetsDistinctNodes pins authorization-scope isolation while TestEntityIdentityIgnoresPromptScopeForResolution pins prompt-independent resolution.
			idParts := []string{string(typeID), key}
			if request.EntityNamespace != "" {
				idParts = append([]string{request.EntityNamespace}, idParts...)
			}
			id, err = ontology.NewStableID(
				"inferred-entity", idParts...,
			)
			if err != nil {
				return Result{}, err
			}
			contractID = id
		}
		if priorKey, duplicate := seenEntityIDs[id]; duplicate {
			return Result{}, fmt.Errorf(
				"extraction: entity %q duplicates entity ID already bound by %q",
				key, priorKey,
			)
		}
		seenEntityIDs[id] = key
		props, propAssertions, propClaims, err := makeProperties(
			contractID, typeID, item.Properties, *item.Confidence, anchorIDs, refs,
			properties, concepts[typeID].Properties(), nodeAnchors, nodeTokens,
			op, modelProv, promptProv, ontologyIdentity,
		)
		if err != nil {
			return Result{}, fmt.Errorf("extraction: entity %q: %w", key, err)
		}
		assertions = append(assertions, propAssertions...)
		claims = append(claims, propClaims...)
		typePredicate, err := ontology.NewStableID(
			"inferred-entity-type", string(request.Version.ID()),
		)
		if err != nil {
			return Result{}, err
		}
		typeValue, err := ontology.NewReferenceValue(typeID)
		if err != nil {
			return Result{}, err
		}
		typeClaim, err := inference.NewClaim(
			contractID, typePredicate, typeValue, shoal.Score(*item.Confidence),
			anchorIDs, inference.ClaimInferred, modelProv, promptProv,
			shoal.Metadata{"kind": "entity_type"},
		)
		if err != nil {
			return Result{}, err
		}
		claims = append(claims, typeClaim)
		entity := Entity{
			ID: id, ContractID: contractID, Key: key, TypeID: typeID, ExistingNodeID: existing,
			Action: action, State: StateProposed, Origin: OriginInferred,
			Confidence: shoal.Score(*item.Confidence), EvidenceIDs: anchorIDs,
			Properties: props, Provenance: provenance,
		}
		entityByKey[key] = entityBinding{entity: entity, contractID: contractID}
	}
	var edges []Edge
	edgeKeys := map[string]struct{}{}
	for _, item := range raw.Relations {
		relation, ok := relations[shoal.ID(item.TypeID)]
		if !ok {
			return Result{}, fmt.Errorf("extraction: unknown relation type %q", item.TypeID)
		}
		if !entityKeyPattern.MatchString(item.FromKey) ||
			item.FromKey != strings.ToLower(item.FromKey) ||
			!entityKeyPattern.MatchString(item.ToKey) ||
			item.ToKey != strings.ToLower(item.ToKey) {
			return Result{}, errors.New("extraction: relation entity keys must be canonical")
		}
		from, ok := entityByKey[strings.ToLower(item.FromKey)]
		if !ok {
			return Result{}, fmt.Errorf("extraction: relation references unknown source entity %q", item.FromKey)
		}
		to, ok := entityByKey[strings.ToLower(item.ToKey)]
		if !ok {
			return Result{}, fmt.Errorf("extraction: relation references unknown target entity %q", item.ToKey)
		}
		if !allowedEndpoints(relation, from.entity.TypeID, to.entity.TypeID) {
			return Result{}, fmt.Errorf("extraction: relation %q crosses disallowed ontology domains", item.TypeID)
		}
		if !relation.Directed() && string(from.entity.ID) > string(to.entity.ID) {
			from, to = to, from
		}
		if err := validateConfidence(*item.Confidence); err != nil {
			return Result{}, err
		}
		anchorIDs, refs, err := resolveEvidence(item.EvidenceAnchorIDs, evidenceByAnchor)
		if err != nil {
			return Result{}, fmt.Errorf("extraction: relation %q: %w", item.TypeID, err)
		}
		edgeID, err := ontology.NewStableID(
			"inferred-edge", string(request.Version.ID()), string(relation.ID()),
			string(from.contractID), string(to.contractID),
		)
		if err != nil {
			return Result{}, err
		}
		edgeKey := string(edgeID)
		if _, duplicate := edgeKeys[edgeKey]; duplicate {
			return Result{}, errors.New("extraction: duplicate canonical relation")
		}
		edgeKeys[edgeKey] = struct{}{}
		refValue, err := ontology.NewReferenceValue(to.contractID)
		if err != nil {
			return Result{}, err
		}
		assertion, err := ontology.NewAssertion(
			from.contractID, relation.ID(), refValue, ontology.AssertionInferred,
			shoal.Score(*item.Confidence), refs, op, nil,
			ontology.WithAssertionSubjectType(from.entity.TypeID),
			ontology.WithAssertionObjectType(to.entity.TypeID),
			ontology.WithAssertionOntology(ontologyIdentity),
		)
		if err != nil {
			return Result{}, err
		}
		claim, err := inference.NewClaim(
			from.contractID, relation.ID(), refValue, shoal.Score(*item.Confidence),
			anchorIDs, inference.ClaimInferred, modelProv, promptProv, nil,
		)
		if err != nil {
			return Result{}, err
		}
		assertions = append(assertions, assertion)
		claims = append(claims, claim)
		props, propAssertions, propClaims, err := makeProperties(
			assertion.ID(), relation.ID(), item.Properties, *item.Confidence,
			anchorIDs, refs, properties, relation.Properties(), nodeAnchors,
			nodeTokens, op, modelProv, promptProv, ontologyIdentity,
		)
		if err != nil {
			return Result{}, fmt.Errorf("extraction: relation %q: %w", item.TypeID, err)
		}
		assertions = append(assertions, propAssertions...)
		claims = append(claims, propClaims...)
		edges = append(edges, Edge{
			ID: edgeID, TypeID: relation.ID(), From: from.entity.ID, To: to.entity.ID,
			FromContractID: from.contractID, ToContractID: to.contractID,
			State: StateProposed, Origin: OriginInferred,
			Confidence: shoal.Score(*item.Confidence), EvidenceIDs: anchorIDs,
			Properties: props, Provenance: provenance,
		})
	}
	if len(claims) == 0 {
		return Result{}, errors.New("extraction: structured output produced no claims")
	}
	completedAt := request.Context.Snapshot().AsOf()
	ontologyResult, err := ontology.NewExtractionResult(ontologyRequest, assertions, nil, completedAt, shoal.Metadata{"state": "proposed"})
	if err != nil {
		return Result{}, fmt.Errorf("extraction: validate ontology result: %w", err)
	}
	inferenceResult, err := inference.NewInferenceResult(request.Context, claims, nil, completedAt, shoal.Metadata{"state": "proposed"})
	if err != nil {
		return Result{}, fmt.Errorf("extraction: validate inference result: %w", err)
	}
	entities := make([]Entity, 0, len(entityByKey))
	for _, binding := range entityByKey {
		entities = append(entities, binding.entity)
	}
	sort.Slice(entities, func(i, j int) bool { return string(entities[i].ID) < string(entities[j].ID) })
	sort.Slice(edges, func(i, j int) bool { return string(edges[i].ID) < string(edges[j].ID) })
	claimIDs := make([]shoal.ID, len(claims))
	for i := range claims {
		claimIDs[i] = claims[i].ID()
	}
	sort.Slice(claimIDs, func(i, j int) bool { return string(claimIDs[i]) < string(claimIDs[j]) })
	return Result{
		ontology: ontologyResult, inference: inferenceResult, prompt: prompt,
		publication: PublicationPlan{
			State: StateProposed, Origin: OriginInferred, Entities: entities,
			Edges: edges, ClaimIDs: claimIDs, Provenance: provenance,
		},
	}, nil
}

type evidencePair struct {
	anchor inference.EvidenceAnchor
	ref    ontology.EvidenceRef
}

func evidenceMaps(pack inference.ContextPack) (
	map[shoal.ID]evidencePair,
	[]ontology.EvidenceRef,
	map[shoal.ID]map[shoal.ID]struct{},
	map[string]shoal.ID,
	error,
) {
	pairs := map[shoal.ID]evidencePair{}
	var refs []ontology.EvidenceRef
	nodes := map[shoal.ID]map[shoal.ID]struct{}{}
	tokens := map[string]shoal.ID{}
	for _, anchor := range pack.Evidence() {
		if citation, quote, ok := anchor.Document(); ok {
			ref, err := ontology.NewEvidenceRef(citation, quote, nil)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			pairs[anchor.ID()] = evidencePair{anchor: anchor, ref: ref}
			refs = append(refs, ref)
		} else if path, ok := anchor.Path(); ok {
			for _, node := range path.Nodes {
				if nodes[node.ID] == nil {
					nodes[node.ID] = map[shoal.ID]struct{}{}
				}
				nodes[node.ID][anchor.ID()] = struct{}{}
				tokens[graphNodeToken(node.ID)] = node.ID
			}
			pairs[anchor.ID()] = evidencePair{anchor: anchor}
		}
	}
	if len(refs) == 0 {
		return nil, nil, nil, nil, errors.New("extraction: ontology extraction requires at least one document evidence anchor")
	}
	return pairs, refs, nodes, tokens, nil
}

func resolveEvidence(raw []string, available map[shoal.ID]evidencePair) ([]shoal.ID, []ontology.EvidenceRef, error) {
	if len(raw) == 0 {
		return nil, nil, errors.New("evidence anchors are required")
	}
	seen := map[shoal.ID]struct{}{}
	var ids []shoal.ID
	var refs []ontology.EvidenceRef
	for _, value := range raw {
		id := shoal.ID(value)
		pair, ok := available[id]
		if !ok {
			return nil, nil, fmt.Errorf("hallucinated evidence anchor %q", value)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, nil, fmt.Errorf("duplicate evidence anchor %q", value)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		if pair.ref.ID() != "" {
			refs = append(refs, pair.ref)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return string(ids[i]) < string(ids[j]) })
	sort.Slice(refs, func(i, j int) bool { return string(refs[i].ID()) < string(refs[j].ID()) })
	return ids, refs, nil
}

func makeProperties(
	subject, subjectType shoal.ID, raw []rawProperty, confidence float64,
	anchorIDs []shoal.ID, refs []ontology.EvidenceRef,
	definitions map[shoal.ID]ontology.PropertyDefinition, allowed []shoal.ID,
	groundedNodes map[shoal.ID]map[shoal.ID]struct{},
	nodeTokens map[string]shoal.ID,
	op ontology.ExtractionProvenance, mp inference.ModelProvenance,
	pp inference.PromptProvenance,
	ontologyIdentity ontology.OntologyIdentity,
) ([]Property, []ontology.Assertion, []inference.Claim, error) {
	allowedSet := map[shoal.ID]struct{}{}
	for _, id := range allowed {
		allowedSet[id] = struct{}{}
	}
	seen := map[string]struct{}{}
	var props []Property
	var assertions []ontology.Assertion
	var claims []inference.Claim
	for _, item := range raw {
		id := shoal.ID(item.PropertyID)
		definition, ok := definitions[id]
		if !ok {
			return nil, nil, nil, fmt.Errorf("unknown property %q", item.PropertyID)
		}
		if _, ok := allowedSet[id]; !ok {
			return nil, nil, nil, fmt.Errorf("property %q does not apply to type %q", item.PropertyID, subjectType)
		}
		value, referenceNodeID, err := decodeValue(item.Value, definition.ValueType(), nodeTokens)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("property %q: %w", item.PropertyID, err)
		}
		if referenceNodeID != "" {
			groundingAnchors, grounded := groundedNodes[referenceNodeID]
			if !grounded {
				return nil, nil, nil, fmt.Errorf("property %q references an ungrounded node ID", item.PropertyID)
			}
			if !intersects(anchorIDs, groundingAnchors) {
				return nil, nil, nil, fmt.Errorf("property %q omits the graph anchor grounding its node ID", item.PropertyID)
			}
		}
		duplicateKey := item.PropertyID + "\x00" + valueText(value)
		if _, duplicate := seen[duplicateKey]; duplicate {
			return nil, nil, nil, fmt.Errorf("duplicate property value for %q", item.PropertyID)
		}
		seen[duplicateKey] = struct{}{}
		assertion, err := ontology.NewAssertion(
			subject, id, value, ontology.AssertionInferred, shoal.Score(confidence),
			refs, op, nil, ontology.WithAssertionSubjectType(subjectType),
			ontology.WithAssertionOntology(ontologyIdentity),
		)
		if err != nil {
			return nil, nil, nil, err
		}
		claim, err := inference.NewClaim(
			subject, id, value, shoal.Score(confidence), anchorIDs,
			inference.ClaimInferred, mp, pp, nil,
		)
		if err != nil {
			return nil, nil, nil, err
		}
		props = append(props, Property{
			DefinitionID: id, Value: value, ReferenceNodeID: referenceNodeID,
		})
		assertions = append(assertions, assertion)
		claims = append(claims, claim)
	}
	sort.Slice(props, func(i, j int) bool {
		if props[i].DefinitionID != props[j].DefinitionID {
			return string(props[i].DefinitionID) < string(props[j].DefinitionID)
		}
		return valueText(props[i].Value) < valueText(props[j].Value)
	})
	return props, assertions, claims, nil
}

func validateEntityCardinality(
	concept ontology.ConceptDefinition,
	raw []rawProperty,
	definitions map[shoal.ID]ontology.PropertyDefinition,
) error {
	counts := map[shoal.ID]uint32{}
	for _, item := range raw {
		counts[shoal.ID(item.PropertyID)]++
	}
	for _, id := range concept.Properties() {
		definition := definitions[id]
		count := counts[id]
		for _, constraint := range definition.Constraints() {
			switch constraint.Kind() {
			case ontology.ConstraintRequired:
				if count == 0 {
					return fmt.Errorf("required property %q is missing", definition.Key())
				}
			case ontology.ConstraintMinimumCount:
				minimum, _ := constraint.Count()
				if count < minimum {
					return fmt.Errorf("property %q is below its minimum count", definition.Key())
				}
			case ontology.ConstraintMaximumCount:
				maximum, _ := constraint.Count()
				if count > maximum {
					return fmt.Errorf("property %q exceeds its maximum count", definition.Key())
				}
			}
		}
	}
	return nil
}

func intersects(ids []shoal.ID, available map[shoal.ID]struct{}) bool {
	for _, id := range ids {
		if _, ok := available[id]; ok {
			return true
		}
	}
	return false
}

func decodeValue(
	raw rawValue, expected ontology.ValueType, nodeTokens map[string]shoal.ID,
) (ontology.Value, shoal.ID, error) {
	if ontology.ValueType(raw.Type) != expected && !(expected == ontology.ValueNumber && raw.Type == string(ontology.ValueInteger)) {
		return ontology.Value{}, "", fmt.Errorf("value type %q does not match %q", raw.Type, expected)
	}
	decode := func(target any) error {
		d := json.NewDecoder(bytes.NewReader(raw.Value))
		d.DisallowUnknownFields()
		if err := d.Decode(target); err != nil {
			return err
		}
		if d.Decode(new(any)) != io.EOF {
			return errors.New("trailing value")
		}
		return nil
	}
	switch ontology.ValueType(raw.Type) {
	case ontology.ValueString:
		var v string
		if err := decode(&v); err != nil {
			return ontology.Value{}, "", err
		}
		value, err := ontology.NewStringValue(v)
		return value, "", err
	case ontology.ValueInteger:
		var v int64
		if err := decode(&v); err != nil {
			return ontology.Value{}, "", err
		}
		return ontology.NewIntegerValue(v), "", nil
	case ontology.ValueNumber:
		var v float64
		if err := decode(&v); err != nil {
			return ontology.Value{}, "", err
		}
		value, err := ontology.NewNumberValue(v)
		return value, "", err
	case ontology.ValueBoolean:
		var v bool
		if err := decode(&v); err != nil {
			return ontology.Value{}, "", err
		}
		return ontology.NewBooleanValue(v), "", nil
	case ontology.ValueTimestamp:
		var v string
		if err := decode(&v); err != nil {
			return ontology.Value{}, "", err
		}
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return ontology.Value{}, "", err
		}
		value, err := ontology.NewTimestampValue(t)
		return value, "", err
	case ontology.ValueReference:
		var v string
		if err := decode(&v); err != nil {
			return ontology.Value{}, "", err
		}
		id, err := decodeGraphNodeToken(v, nodeTokens)
		if err != nil {
			return ontology.Value{}, "", err
		}
		contractID, err := ontology.NewStableID("grounded-node", v)
		if err != nil {
			return ontology.Value{}, "", err
		}
		value, err := ontology.NewReferenceValue(contractID)
		return value, id, err
	default:
		return ontology.Value{}, "", fmt.Errorf("unsupported value type %q", raw.Type)
	}
}

func validateConfidence(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return errors.New("extraction: confidence must be finite and between zero and one")
	}
	return nil
}

func allowedEndpoints(r ontology.RelationshipDefinition, from, to shoal.ID) bool {
	contains := func(values []shoal.ID, target shoal.ID) bool {
		for _, value := range values {
			if value == target {
				return true
			}
		}
		return false
	}
	if contains(r.FromConcepts(), from) && contains(r.ToConcepts(), to) {
		return true
	}
	return !r.Directed() && contains(r.FromConcepts(), to) && contains(r.ToConcepts(), from)
}

func extractionLimits(request Request) ontology.ExtractionLimits {
	limits := ontology.DefaultExtractionLimits()
	if count := len(request.Context.Evidence()); count > int(limits.MaxEvidence) {
		limits.MaxEvidence = uint32(count)
	}
	return limits
}

func graphNodeToken(id shoal.ID) string {
	return graphIDToken("node", id)
}

func graphIDToken(kind string, id shoal.ID) string {
	return kind + "-base64:" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeGraphNodeToken(value string, available map[string]shoal.ID) (shoal.ID, error) {
	id, ok := available[value]
	if !ok {
		return "", errors.New("node token is not grounded in the context")
	}
	encoded := strings.TrimPrefix(value, "node-base64:")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || string(decoded) != string(id) {
		return "", errors.New("node token is not canonical")
	}
	return id, nil
}

func clonePlan(plan PublicationPlan) PublicationPlan {
	out := plan
	out.Entities = make([]Entity, len(plan.Entities))
	for i, entity := range plan.Entities {
		out.Entities[i] = entity
		out.Entities[i].EvidenceIDs = append([]shoal.ID(nil), entity.EvidenceIDs...)
		out.Entities[i].Properties = append([]Property(nil), entity.Properties...)
	}
	out.Edges = make([]Edge, len(plan.Edges))
	for i, edge := range plan.Edges {
		out.Edges[i] = edge
		out.Edges[i].EvidenceIDs = append([]shoal.ID(nil), edge.EvidenceIDs...)
		out.Edges[i].Properties = append([]Property(nil), edge.Properties...)
	}
	out.ClaimIDs = append([]shoal.ID(nil), plan.ClaimIDs...)
	return out
}

func valueText(value ontology.Value) string {
	switch value.Type() {
	case ontology.ValueString:
		v, _ := value.StringValue()
		return "string:" + v
	case ontology.ValueInteger:
		v, _ := value.IntegerValue()
		return "integer:" + strconv.FormatInt(v, 10)
	case ontology.ValueNumber:
		v, _ := value.NumberValue()
		return "number:" + strconv.FormatFloat(v, 'g', -1, 64)
	case ontology.ValueBoolean:
		v, _ := value.BooleanValue()
		return "boolean:" + strconv.FormatBool(v)
	case ontology.ValueTimestamp:
		v, _ := value.TimestampValue()
		return "timestamp:" + v.UTC().Format(time.RFC3339Nano)
	case ontology.ValueReference:
		v, _ := value.ReferenceValue()
		return "reference:" + string(v)
	default:
		return ""
	}
}

// HeuristicExtractor is an explicit no-model extractor. It only proposes
// capitalized phrases as entities of the configured concept type and never
// runs automatically after a model error.
type HeuristicExtractor struct {
	ConceptType shoal.ID
}

func (h HeuristicExtractor) Extract(_ context.Context, request Request) (Result, error) {
	if err := request.Validate(); err != nil {
		return Result{}, err
	}
	if ids, ok := skillOntologyIDs(request.Version); ok {
		return h.extractSkillOntology(request, ids)
	}
	if _, ok := conceptByID(request.Version, h.ConceptType); !ok {
		return Result{}, errors.New("extraction: heuristic concept type is not in the ontology")
	}
	limits, _ := request.Limits.normalized()
	seen := map[string]struct{}{}
	var entities []rawEntity
	for _, anchor := range request.Context.Evidence() {
		_, quote, ok := anchor.Document()
		if !ok {
			continue
		}
		for _, token := range strings.Fields(quote) {
			clean := strings.Trim(token, ".,;:!?()[]{}\"'")
			if clean == "" || !utf8.ValidString(clean) {
				continue
			}
			first, _ := utf8.DecodeRuneInString(clean)
			if first < 'A' || first > 'Z' {
				continue
			}
			key := strings.ToLower(heuristicKeySanitizer.ReplaceAllString(clean, "_"))
			key = strings.Trim(key, "_")
			if !entityKeyPattern.MatchString(key) {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			if len(entities) >= limits.MaxEntities {
				return Result{}, errors.New("extraction: output exceeds item count limit")
			}
			seen[key] = struct{}{}
			entities = append(entities, rawEntity{
				Key: key, TypeID: string(h.ConceptType), Confidence: floatPointer(0.5),
				EvidenceAnchorIDs: []string{string(anchor.ID())},
			})
		}
	}
	if len(entities) == 0 {
		return Result{}, errors.New("extraction: heuristic found no grounded entities")
	}
	prompt, err := BuildPrompt(request)
	if err != nil {
		return Result{}, err
	}
	return materialize(request, prompt, model.Provenance{Provider: HeuristicProvider, Model: HeuristicModel}, rawOutput{Entities: entities}, "heuristic")
}

type skillIDs struct {
	skill, tool, capability          shoal.ID
	name, description                shoal.ID
	providesTool, providesCapability shoal.ID
	dependsOn                        shoal.ID
}

type skillCandidate struct {
	key, name, typeName string
	typeID              shoal.ID
	anchorID            shoal.ID
	existingToken       string
	existingAnchorID    shoal.ID
}

type relationCandidate struct {
	typeID       shoal.ID
	fromKey      string
	toKey        string
	anchorID     shoal.ID
	sortableType string
}

func skillOntologyIDs(version ontology.OntologyVersion) (skillIDs, bool) {
	var ids skillIDs
	for _, concept := range version.Concepts() {
		switch concept.Key() {
		case "skill":
			ids.skill = concept.ID()
		case "tool":
			ids.tool = concept.ID()
		case "capability", "feature":
			ids.capability = concept.ID()
		}
	}
	for _, property := range version.Properties() {
		switch property.Key() {
		case "name":
			ids.name = property.ID()
		case "description":
			ids.description = property.ID()
		}
	}
	for _, relationship := range version.Relationships() {
		switch relationship.Key() {
		case "provides_tool":
			ids.providesTool = relationship.ID()
		case "provides_capability", "provides_feature":
			ids.providesCapability = relationship.ID()
		case "depends_on":
			ids.dependsOn = relationship.ID()
		}
	}
	return ids, ids.skill != "" && ids.tool != "" && ids.capability != "" && ids.name != ""
}

func (h HeuristicExtractor) extractSkillOntology(request Request, ids skillIDs) (Result, error) {
	limits, _ := request.Limits.normalized()
	existing := existingOntologyNodes(request.Context)
	var skills, tools, capabilities []skillCandidate
	var dependencies []skillCandidate
	for _, anchor := range request.Context.Evidence() {
		_, quote, ok := anchor.Document()
		if !ok {
			continue
		}
		skillName := markdownTitle(quote)
		if skillName == "" {
			skillName = firstNamedValue(quote, "name")
		}
		if skillName != "" {
			skills = append(skills, newSkillCandidate(
				skillName, "skill", ids.skill, anchor.ID(), existing))
		}
		tools = append(tools, extractNamedList(
			quote, []string{"tools", "tooling"}, "tool", ids.tool, anchor.ID(), existing)...)
		capabilities = append(capabilities, extractNamedList(
			quote, []string{"capabilities", "features", "feature"}, "capability", ids.capability, anchor.ID(), existing)...)
		dependencies = append(dependencies, extractNamedList(
			quote, []string{"dependencies", "depends on", "requires", "prerequisites"}, "skill", ids.skill, anchor.ID(), existing)...)
	}
	skills = uniqueCandidates(skills)
	tools = uniqueCandidates(tools)
	capabilities = uniqueCandidates(capabilities)
	dependencies = uniqueCandidates(dependencies)
	if len(skills) == 0 {
		return Result{}, errors.New("extraction: heuristic found no skill entity")
	}
	candidates := uniqueCandidates(append(append(append(skills, tools...), capabilities...), dependencies...))
	if len(candidates) > limits.MaxEntities {
		return Result{}, errors.New("extraction: output exceeds item count limit")
	}
	entities := make([]rawEntity, 0, len(candidates))
	for _, candidate := range candidates {
		entity := rawEntity{
			Key: candidate.key, TypeID: string(candidate.typeID),
			Properties: []rawProperty{stringProperty(ids.name, candidate.name)},
			Confidence: floatPointer(0.6), EvidenceAnchorIDs: []string{string(candidate.anchorID)},
		}
		if candidate.existingToken != "" {
			entity.ExistingNodeID = candidate.existingToken
			entity.EvidenceAnchorIDs = append(entity.EvidenceAnchorIDs, string(candidate.existingAnchorID))
			sort.Strings(entity.EvidenceAnchorIDs)
		}
		entities = append(entities, entity)
	}
	relations := make([]rawRelation, 0, len(tools)+len(capabilities)+len(dependencies))
	skillKey := skills[0].key
	if ids.providesTool != "" {
		for _, tool := range tools {
			relations = append(relations, rawRelation{
				TypeID: string(ids.providesTool), FromKey: skillKey, ToKey: tool.key,
				Properties: []rawProperty{}, Confidence: floatPointer(0.55),
				EvidenceAnchorIDs: []string{string(tool.anchorID)},
			})
		}
	}
	if ids.providesCapability != "" {
		for _, capability := range capabilities {
			relations = append(relations, rawRelation{
				TypeID: string(ids.providesCapability), FromKey: skillKey, ToKey: capability.key,
				Properties: []rawProperty{}, Confidence: floatPointer(0.55),
				EvidenceAnchorIDs: []string{string(capability.anchorID)},
			})
		}
	}
	if ids.dependsOn != "" {
		for _, dependency := range dependencies {
			if dependency.key == skillKey {
				continue
			}
			relations = append(relations, rawRelation{
				TypeID: string(ids.dependsOn), FromKey: skillKey, ToKey: dependency.key,
				Properties: []rawProperty{}, Confidence: floatPointer(0.5),
				EvidenceAnchorIDs: []string{string(dependency.anchorID)},
			})
		}
	}
	sort.Slice(relations, func(i, j int) bool {
		if relations[i].TypeID != relations[j].TypeID {
			return relations[i].TypeID < relations[j].TypeID
		}
		if relations[i].FromKey != relations[j].FromKey {
			return relations[i].FromKey < relations[j].FromKey
		}
		return relations[i].ToKey < relations[j].ToKey
	})
	if len(entities) == 0 && len(relations) == 0 {
		return Result{}, errors.New("extraction: heuristic found no grounded entities")
	}
	prompt, err := BuildPrompt(request)
	if err != nil {
		return Result{}, err
	}
	return materialize(
		request, prompt,
		model.Provenance{Provider: HeuristicProvider, Model: "deterministic-skill-markdown"},
		rawOutput{Entities: entities, Relations: relations},
		"heuristic",
	)
}

func newSkillCandidate(
	name, typeName string,
	typeID shoal.ID,
	anchorID shoal.ID,
	existing map[string]existingNode,
) skillCandidate {
	key := canonicalEntityKey(name)
	candidate := skillCandidate{
		key: key, name: strings.TrimSpace(name), typeName: typeName,
		typeID: typeID, anchorID: anchorID,
	}
	if match, ok := existing[string(typeID)+"\x00"+key]; ok {
		candidate.existingToken = graphNodeToken(match.id)
		candidate.existingAnchorID = match.anchorID
	}
	return candidate
}

func uniqueCandidates(values []skillCandidate) []skillCandidate {
	sort.Slice(values, func(i, j int) bool {
		if values[i].typeName != values[j].typeName {
			return values[i].typeName < values[j].typeName
		}
		return values[i].key < values[j].key
	})
	unique := values[:0]
	seen := map[string]struct{}{}
	for _, value := range values {
		if value.key == "" || !entityKeyPattern.MatchString(value.key) {
			continue
		}
		k := string(value.typeID) + "\x00" + value.key
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}

func stringProperty(id shoal.ID, value string) rawProperty {
	encoded, _ := json.Marshal(value)
	return rawProperty{
		PropertyID: string(id),
		Value:      rawValue{Type: string(ontology.ValueString), Value: encoded},
	}
}

type existingNode struct {
	id       shoal.ID
	anchorID shoal.ID
}

func existingOntologyNodes(pack inference.ContextPack) map[string]existingNode {
	result := map[string]existingNode{}
	for _, anchor := range pack.Evidence() {
		path, ok := anchor.Path()
		if !ok || len(path.Nodes) != 1 {
			continue
		}
		node := path.Nodes[0]
		typeID := node.Properties[GraphPropertyOntologyConceptID]
		key := node.Properties[GraphPropertyEntityKey]
		if typeID == "" || key == "" {
			continue
		}
		mapKey := typeID + "\x00" + key
		if previous, ok := result[mapKey]; ok && string(previous.id) <= string(node.ID) {
			continue
		}
		result[mapKey] = existingNode{id: node.ID, anchorID: anchor.ID()}
	}
	return result
}

func markdownTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			return cleanListItem(strings.TrimSpace(strings.TrimPrefix(trimmed, "# ")))
		}
	}
	return ""
}

func firstNamedValue(text, name string) string {
	prefix := strings.ToLower(name) + ":"
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), prefix) {
			return cleanListItem(strings.TrimSpace(trimmed[len(prefix):]))
		}
	}
	return ""
}

func extractNamedList(
	text string,
	headings []string,
	typeName string,
	typeID shoal.ID,
	anchorID shoal.ID,
	existing map[string]existingNode,
) []skillCandidate {
	headingSet := map[string]struct{}{}
	for _, heading := range headings {
		headingSet[heading] = struct{}{}
	}
	var out []skillCandidate
	inSection := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		// This heading normalization is load-bearing; TestExtractDocumentPublishesSkillGraph pins colon-headed skill sections such as "Tools:".
		lower := strings.ToLower(strings.Trim(trimmed, "# "))
		if strings.HasPrefix(trimmed, "#") {
			_, inSection = headingSet[strings.TrimSuffix(lower, ":")]
			continue
		}
		if strings.HasSuffix(lower, ":") {
			_, inSection = headingSet[strings.TrimSuffix(lower, ":")]
			if inSection {
				continue
			}
		}
		if !inSection {
			for heading := range headingSet {
				prefix := heading + ":"
				if strings.HasPrefix(strings.ToLower(trimmed), prefix) {
					for _, item := range splitInlineList(trimmed[len(prefix):]) {
						out = append(out, newSkillCandidate(item, typeName, typeID, anchorID, existing))
					}
				}
			}
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			item := cleanListItem(trimmed[2:])
			if item != "" {
				out = append(out, newSkillCandidate(item, typeName, typeID, anchorID, existing))
			}
		}
	}
	return out
}

func splitInlineList(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if cleaned := cleanListItem(field); cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return out
}

func cleanListItem(text string) string {
	value := strings.TrimSpace(text)
	value = strings.Trim(value, "`*_[]()")
	if i := strings.Index(value, " - "); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	if i := strings.Index(value, ": "); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return strings.Trim(value, " .,\t\r\n")
}

func canonicalEntityKey(name string) string {
	key := strings.ToLower(heuristicKeySanitizer.ReplaceAllString(strings.TrimSpace(name), "_"))
	return strings.Trim(key, "_")
}

func conceptByID(version ontology.OntologyVersion, id shoal.ID) (ontology.ConceptDefinition, bool) {
	for _, concept := range version.Concepts() {
		if concept.ID() == id {
			return concept, true
		}
	}
	return ontology.ConceptDefinition{}, false
}

func floatPointer(value float64) *float64 { return &value }
