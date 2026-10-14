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

package code

import (
	"sort"
	"strconv"
)

func parseResultPayloadDigest(result ParseResult) (ID, error) {
	parts := make([]string, 0)

	roots := result.Roots()
	parts = appendCollection(parts, "roots", len(roots))
	for _, root := range roots {
		parts = append(parts, root.String())
	}

	nodes := result.Nodes()
	parts = appendCollection(parts, "nodes", len(nodes))
	for _, node := range nodes {
		parts = append(parts,
			node.ID().String(),
			node.SourceID().String(),
			node.Kind(),
			strconv.FormatUint(uint64(node.Occurrence()), 10),
		)
		parts = append(parts, rangeIdentityParts(node.Range())...)
		children := node.Children()
		parts = appendCollection(parts, "children", len(children))
		for _, child := range children {
			parts = append(parts, child.String())
		}
		parts = appendAttributes(parts, node.Attributes())
	}

	symbols := result.Symbols()
	parts = appendCollection(parts, "symbols", len(symbols))
	for _, symbol := range symbols {
		parts = append(parts,
			symbol.ID().String(),
			symbol.SourceID().String(),
			symbol.Kind(),
			symbol.Name(),
			symbol.QualifiedName(),
			strconv.FormatUint(uint64(symbol.Occurrence()), 10),
		)
		parts = append(parts, rangeIdentityParts(symbol.Definition())...)
		syntaxNodeID, hasSyntaxNodeID := symbol.SyntaxNodeID()
		parts = append(parts, strconv.FormatBool(hasSyntaxNodeID))
		if hasSyntaxNodeID {
			parts = append(parts, syntaxNodeID.String())
		}
		parts = appendAttributes(parts, symbol.Attributes())
	}

	externals := result.Externals()
	parts = appendCollection(parts, "externals", len(externals))
	for _, external := range externals {
		parts = append(parts,
			external.ID().String(),
			external.Kind(),
			external.CanonicalName(),
		)
		parts = appendAttributes(parts, external.Attributes())
	}

	relationships := result.Relationships()
	parts = appendCollection(parts, "relationships", len(relationships))
	for _, relationship := range relationships {
		parts = append(parts,
			relationship.ID().String(),
			string(relationship.Kind()),
			string(relationship.From().Kind()),
			relationship.From().ID().String(),
			string(relationship.To().Kind()),
			relationship.To().ID().String(),
		)
		sourceRange, hasRange := relationship.Range()
		parts = append(parts, strconv.FormatBool(hasRange))
		if hasRange {
			parts = append(parts, rangeIdentityParts(sourceRange)...)
		}
		parts = appendAttributes(parts, relationship.Attributes())
	}

	diagnostics := result.Diagnostics()
	parts = appendCollection(parts, "diagnostics", len(diagnostics))
	for _, diagnostic := range diagnostics {
		parts = append(parts,
			string(diagnostic.Severity()),
			diagnostic.Code(),
			diagnostic.Message(),
		)
		sourceRange, hasRange := diagnostic.Range()
		parts = append(parts, strconv.FormatBool(hasRange))
		if hasRange {
			parts = append(parts, rangeIdentityParts(sourceRange)...)
		}
	}

	return deriveID("parse-result", parts...)
}

func appendCollection(parts []string, name string, count int) []string {
	return append(parts, name, strconv.Itoa(count))
}

func appendAttributes(parts []string, attributes map[string]string) []string {
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts = appendCollection(parts, "attributes", len(keys))
	for _, key := range keys {
		parts = append(parts, key, attributes[key])
	}
	return parts
}
