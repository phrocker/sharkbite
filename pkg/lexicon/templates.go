/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package lexicon

import (
	"sort"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Direction is the side of a relationship a lookup template asks about.
type Direction uint8

const (
	// DirectionOut asks for targets of a subject: "what does X <relation>".
	DirectionOut Direction = 1
	// DirectionIn asks for sources of a subject: "what <relation> X".
	DirectionIn Direction = 2
	// DirectionBoth is the single template of an undirected relationship,
	// whose endpoints are interchangeable.
	DirectionBoth Direction = 3
)

func (d Direction) String() string {
	switch d {
	case DirectionOut:
		return "out"
	case DirectionIn:
		return "in"
	case DirectionBoth:
		return "both"
	default:
		return "invalid"
	}
}

func (d Direction) valid() bool {
	return d >= DirectionOut && d <= DirectionBoth
}

// Template is one lookup question derived from the ontology schema. It is data
// only: PhraseKey names the wording, which lives in a renderer catalog, not
// in the bundle.
type Template struct {
	ID              string
	RelationKey     string
	Direction       Direction
	SubjectConcepts []shoal.ID
	AnswerConcepts  []shoal.ID
	PhraseKey       string
}

func templateID(relationKey string, direction Direction) string {
	return "lookup:" + relationKey + ":" + direction.String()
}

func phraseKey(relationKey string, direction Direction) string {
	return "lexicon.lookup." + relationKey + "." + direction.String()
}

// DeriveTemplates gives two templates per directed relationship (one per
// direction) and one per undirected relationship. The result is sorted by ID
// and independent of input order; duplicate relationship keys fail.
func DeriveTemplates(
	relationships []ontology.RelationshipDefinition,
) ([]Template, error) {
	templates := make([]Template, 0, 2*len(relationships))
	keys := make(map[string]struct{}, len(relationships))
	for _, relationship := range relationships {
		if err := relationship.Validate(); err != nil {
			return nil, shoal.WrapError(
				shoal.ErrorInvalidArgument, "lexicon relationship is invalid", err)
		}
		// Checked on the key itself: a directed and an undirected definition
		// with one key give template IDs that do not collide.
		if _, duplicate := keys[relationship.Key()]; duplicate {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "lexicon relationship keys must be unique")
		}
		keys[relationship.Key()] = struct{}{}
		key := relationship.Key()
		from := sortedIDs(relationship.FromConcepts())
		to := sortedIDs(relationship.ToConcepts())
		if !relationship.Directed() {
			both := sortedIDs(append(append([]shoal.ID(nil), from...), to...))
			templates = append(templates, Template{
				ID: templateID(key, DirectionBoth), RelationKey: key,
				Direction: DirectionBoth, SubjectConcepts: both,
				AnswerConcepts: append([]shoal.ID(nil), both...),
				PhraseKey:      phraseKey(key, DirectionBoth),
			})
			continue
		}
		templates = append(templates,
			Template{
				ID: templateID(key, DirectionOut), RelationKey: key,
				Direction: DirectionOut, SubjectConcepts: from,
				AnswerConcepts: to, PhraseKey: phraseKey(key, DirectionOut),
			},
			Template{
				ID: templateID(key, DirectionIn), RelationKey: key,
				Direction: DirectionIn, SubjectConcepts: append([]shoal.ID(nil), to...),
				AnswerConcepts: append([]shoal.ID(nil), from...),
				PhraseKey:      phraseKey(key, DirectionIn),
			})
	}
	sort.Slice(templates, func(i, j int) bool {
		return templates[i].ID < templates[j].ID
	})
	for index := 1; index < len(templates); index++ {
		if templates[index].ID == templates[index-1].ID {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "lexicon relationship keys must be unique")
		}
	}
	return templates, nil
}

// sortedIDs returns a sorted, de-duplicated copy.
func sortedIDs(ids []shoal.ID) []shoal.ID {
	out := append([]shoal.ID(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	deduped := out[:0]
	for index, id := range out {
		if index > 0 && id == out[index-1] {
			continue
		}
		deduped = append(deduped, id)
	}
	return deduped
}

func cloneTemplate(template Template) Template {
	template.SubjectConcepts = append([]shoal.ID(nil), template.SubjectConcepts...)
	template.AnswerConcepts = append([]shoal.ID(nil), template.AnswerConcepts...)
	return template
}
