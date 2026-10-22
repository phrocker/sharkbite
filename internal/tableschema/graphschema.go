// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package tableschema

import (
	"github.com/phrocker/shoal-oss/internal/graphschema"
	"github.com/phrocker/shoal-oss/internal/schemapb"
)

// GraphSchemaID is the schema id of the agent knowledge graph layout that
// internal/graphschema defines by convention.
const GraphSchemaID = "shoal.graphschema"

// ForGraphSchema returns the descriptor of the internal/graphschema layout
// for a table: event rows with their content and attribute families, entity
// rows with the attribute family, the four edge views with the neighbor id
// in the qualifier and an optional float32 weight, the embedding family on
// both node namespaces, and term postings. It is the descriptor a
// reader needs to traverse a graphschema table without the Go library, and
// the basis for checking that both readings agree.
func ForGraphSchema(table, embeddingSpace string, version int32) *schemapb.TableSchema {
	edge := func(name string, cf []byte) *schemapb.EdgeSchema {
		return &schemapb.EdgeSchema{
			Name:          name,
			EdgeCf:        string(cf),
			EdgeField:     "qualifier",
			ValueEncoding: schemapb.ValueEncoding_VALUE_ENCODING_FLOAT32_BE,
		}
	}
	schema := &schemapb.TableSchema{
		Table:       table,
		SchemaId:    GraphSchemaID,
		Version:     version,
		Description: "agent knowledge graph laid out by internal/graphschema (docs/graph-schema.md)",
		Nodes: []*schemapb.NodeSchema{
			{
				Name:               "event",
				RowPrefix:          graphschema.EventRowPrefix,
				AttributeCfs:       []string{graphschema.ContentCFName, graphschema.AttributeCFName},
				AttributeQualifier: schemapb.QualifierRole_QUALIFIER_ROLE_FIELD,
				AttributeEncoding:  schemapb.ValueEncoding_VALUE_ENCODING_TEXT,
			},
			{
				Name:               "entity",
				RowPrefix:          graphschema.EntityRowPrefix,
				AttributeCfs:       []string{graphschema.AttributeCFName},
				AttributeQualifier: schemapb.QualifierRole_QUALIFIER_ROLE_FIELD,
				AttributeEncoding:  schemapb.ValueEncoding_VALUE_ENCODING_TEXT,
			},
		},
		Edges: []*schemapb.EdgeSchema{
			edge("temporal", graphschema.TemporalEdgeCF()),
			edge("causal", graphschema.CausalEdgeCF()),
			edge("semantic", graphschema.SemanticEdgeCF()),
			edge("entity_edge", graphschema.EntityEdgeCF()),
		},
		Postings: []*schemapb.PostingSchema{{
			Name:          "term",
			TermRowPrefix: graphschema.TermRowPrefix,
			IdSource:      "qualifier",
			PrimaryPrefix: graphschema.EventRowPrefix,
		}},
	}
	if embeddingSpace != "" {
		schema.Vectors = []*schemapb.VectorSchema{{
			Name:           "embedding",
			EmbeddingCf:    graphschema.VectorCFName,
			EmbeddingSpace: embeddingSpace,
			Metric:         "cosine",
		}}
	}
	return schema
}
