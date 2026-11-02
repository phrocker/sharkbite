package harness

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ActionSchema constrains the next model action to the evidence and tool
// budget visible in this prompt. Semantic checks remain in the host parser.
func ActionSchema(envelope promptEnvelope) (json.RawMessage, error) {
	correlation := newProtocolID(shoal.ID(fmt.Sprintf("turn:%d", len(envelope.Transcript))))
	base := func(action ActionKind, extra map[string]any) map[string]any {
		properties := map[string]any{
			"action":         map[string]any{"const": action},
			"correlation_id": map[string]any{"const": correlation},
		}
		required := []string{"action", "correlation_id"}
		for name, field := range extra {
			properties[name] = field
			required = append(required, name)
		}
		sort.Strings(required)
		return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
	}
	positiveText := func(maxBytes int) map[string]any {
		return map[string]any{"type": "string", "minLength": 1, "maxLength": maxBytes / 4, "pattern": "\\S"}
	}
	positiveInt := func(maximum int) map[string]any {
		return map[string]any{"type": "integer", "minimum": 1, "maximum": maximum}
	}
	// Opaque claim IDs need only be valid base64. Tool IDs below are drawn
	// from the current evidence, never generated from a pattern.
	id := map[string]any{"type": "string", "minLength": 4, "maxLength": 4*MaxLogicalIDBytes/3 + 4,
		"pattern": "^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$"}
	evidenceIDs := make([]protocolID, 0, len(envelope.Evidence))
	sections := make([]map[string]any, 0)
	sectionSet := make(map[[3]protocolID]struct{})
	nodes := make(map[protocolID]struct{})
	for _, item := range envelope.Evidence {
		evidenceIDs = append(evidenceIDs, item.ID)
		if item.Citation != nil && item.Citation.DocumentID != "" && item.Citation.RevisionID != "" && item.Citation.SectionID != "" {
			triple := [3]protocolID{item.Citation.DocumentID, item.Citation.RevisionID, item.Citation.SectionID}
			if _, seen := sectionSet[triple]; !seen {
				sectionSet[triple] = struct{}{}
				sections = append(sections, map[string]any{"properties": map[string]any{
					"document_id": map[string]any{"const": item.Citation.DocumentID},
					"revision_id": map[string]any{"const": item.Citation.RevisionID},
					"section_id":  map[string]any{"const": item.Citation.SectionID},
				}})
			}
		}
		if item.Path != nil {
			for _, node := range item.Path.Nodes {
				nodes[node.ID] = struct{}{}
			}
		}
	}
	variants := []any{}
	if envelope.Remaining.ModelCalls > 1 && envelope.Remaining.Evidence > 0 && envelope.Budgets.MaxFanout > 0 {
		variants = append(variants, base(ActionRetrieve, map[string]any{
			"query": positiveText(MaxToolQueryBytes),
			"limit": positiveInt(min(envelope.Remaining.Evidence, envelope.Budgets.MaxFanout)),
		}))
		if len(sections) > 0 {
			variant := base(ActionOpenSection, map[string]any{
				"document_id": id, "revision_id": id, "section_id": id,
			})
			variant["oneOf"] = sections
			variants = append(variants, variant)
		}
		if len(nodes) > 0 && envelope.Remaining.GraphHops > 0 && envelope.Remaining.GraphNodes > 0 {
			visibleNodes := make([]protocolID, 0, len(nodes))
			for node := range nodes {
				visibleNodes = append(visibleNodes, node)
			}
			sort.Slice(visibleNodes, func(i, j int) bool { return visibleNodes[i] < visibleNodes[j] })
			variants = append(variants, base(ActionNeighbors, map[string]any{
				"node_id": map[string]any{"enum": visibleNodes},
				"hops":    positiveInt(envelope.Remaining.GraphHops),
				"fanout":  positiveInt(min(envelope.Remaining.GraphNodes, envelope.Budgets.MaxFanout)),
			}))
		}
	}
	evidenceRef := map[string]any{"type": "array", "items": map[string]any{"enum": evidenceIDs}, "maxItems": 1}
	issue := map[string]any{"type": "object", "additionalProperties": false,
		"required": []string{"input", "reason", "evidence_ids"}, "properties": map[string]any{
			"input":        positiveText(inference.MaxIssueInputBytes),
			"reason":       positiveText(inference.MaxIssueReasonBytes),
			"evidence_ids": evidenceRef,
		}}
	noItems := map[string]any{"type": "array", "maxItems": 0}
	variants = append(variants,
		base(ActionStop, map[string]any{"claims": noItems, "unresolved": map[string]any{"type": "array", "items": issue, "minItems": 1, "maxItems": 1}, "unsupported": noItems}),
		base(ActionStop, map[string]any{"claims": noItems, "unresolved": noItems, "unsupported": map[string]any{"type": "array", "items": issue, "minItems": 1, "maxItems": 1}}),
	)
	if len(evidenceIDs) > 0 {
		claim := map[string]any{"type": "object", "additionalProperties": false,
			"required": []string{"subject", "predicate", "object", "confidence", "evidence_ids"},
			"properties": map[string]any{
				"subject": id, "predicate": id,
				"object": map[string]any{"type": "object", "additionalProperties": false,
					"required": []string{"type", "value"}, "properties": map[string]any{
						"type": map[string]any{"const": "string"}, "value": positiveText(4096),
					}},
				"confidence":   map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				"evidence_ids": map[string]any{"type": "array", "items": map[string]any{"enum": evidenceIDs}, "minItems": 1, "maxItems": 1},
			}}
		variants = append(variants, base(ActionStop, map[string]any{
			"claims":     map[string]any{"type": "array", "items": claim, "minItems": 1, "maxItems": 1},
			"unresolved": noItems, "unsupported": noItems,
		}))
	}
	encoded, err := json.Marshal(map[string]any{"oneOf": variants})
	if err != nil {
		return nil, err
	}
	if len(encoded) > 256<<10 {
		return nil, invalid("action schema exceeds the provider byte bound")
	}
	return encoded, nil
}

// ActionSchemaForPrompt builds the turn-specific schema from a serialized
// prompt. It is useful to applications that extend the stop action.
func ActionSchemaForPrompt(prompt string) (json.RawMessage, error) {
	var envelope promptEnvelope
	if err := json.Unmarshal([]byte(prompt), &envelope); err != nil {
		return nil, err
	}
	return ActionSchema(envelope)
}
