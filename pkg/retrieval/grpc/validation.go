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

package retrievalgrpc

import (
	"fmt"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/knowledgepb"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func validateRequestWire(request retrieval.Request) error {
	if err := validateWireString("retrieval text", request.Text); err != nil {
		return shoal.WrapError(shoal.ErrorInvalidArgument, err.Error(), err)
	}
	for _, id := range request.Scope.DocumentIDs {
		if err := validateWireString("retrieval document ID", string(id)); err != nil {
			return shoal.WrapError(shoal.ErrorInvalidArgument, err.Error(), err)
		}
	}
	for _, id := range request.Scope.NodeIDs {
		if err := validateWireString("retrieval node ID", string(id)); err != nil {
			return shoal.WrapError(shoal.ErrorInvalidArgument, err.Error(), err)
		}
	}
	return nil
}

func validateResponse(request retrieval.Request, response retrieval.Response) error {
	if err := response.ValidateFor(request); err != nil {
		return err
	}
	if err := validateWireString("request ID", string(response.RequestID)); err != nil {
		return err
	}
	if err := validateWireString(
		"embedding space ID", string(response.EmbeddingSpaceID),
	); err != nil {
		return err
	}
	for index, id := range response.EmbeddingSpaceIDs {
		if err := validateWireString(
			fmt.Sprintf("embedding space constituent ID %d", index),
			string(id),
		); err != nil {
			return err
		}
	}
	for resultIndex, result := range response.Results {
		resultName := fmt.Sprintf("result %d", resultIndex)
		if err := validateWireString(resultName+" ID", string(result.ID)); err != nil {
			return err
		}
		for evidenceIndex, evidence := range result.Evidence {
			evidenceName := fmt.Sprintf(
				"result %d evidence %d", resultIndex, evidenceIndex)
			if err := validateCitationStrings(
				evidenceName+" citation", evidence.Citation,
			); err != nil {
				return err
			}
			if err := validateWireString(evidenceName+" quote", evidence.Quote); err != nil {
				return err
			}
			if pathPresent(evidence.Path) {
				if err := validatePathStrings(evidenceName+" path", evidence.Path); err != nil {
					return err
				}
			}
		}
		if result.Explanation != nil {
			if err := validateWireString(
				resultName+" explanation summary", result.Explanation.Summary,
			); err != nil {
				return err
			}
			for name := range result.Explanation.Scores {
				if err := validateWireString(
					resultName+" explanation score name", name,
				); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateCitationStrings(name string, citation document.Citation) error {
	fields := []struct {
		name  string
		value shoal.ID
	}{
		{name: "document ID", value: citation.DocumentID},
		{name: "revision ID", value: citation.RevisionID},
		{name: "section ID", value: citation.SectionID},
		{name: "span ID", value: citation.SpanID},
	}
	for _, field := range fields {
		if err := validateWireString(name+" "+field.name, string(field.value)); err != nil {
			return err
		}
	}
	return nil
}

func validatePathStrings(name string, path graph.Path) error {
	for nodeIndex, node := range path.Nodes {
		nodeName := fmt.Sprintf("%s node %d", name, nodeIndex)
		if err := validateWireString(nodeName+" ID", string(node.ID)); err != nil {
			return err
		}
		if err := validateWireString(nodeName+" kind", node.Kind); err != nil {
			return err
		}
		for labelIndex, label := range node.Labels {
			if err := validateWireString(
				fmt.Sprintf("%s label %d", nodeName, labelIndex), label,
			); err != nil {
				return err
			}
		}
		if err := validateMetadataStrings(nodeName+" property", node.Properties); err != nil {
			return err
		}
	}
	for edgeIndex, edge := range path.Edges {
		edgeName := fmt.Sprintf("%s edge %d", name, edgeIndex)
		fields := []struct {
			name  string
			value string
		}{
			{name: "ID", value: string(edge.ID)},
			{name: "from", value: string(edge.From)},
			{name: "to", value: string(edge.To)},
			{name: "type", value: edge.Type},
		}
		for _, field := range fields {
			if err := validateWireString(edgeName+" "+field.name, field.value); err != nil {
				return err
			}
		}
		if err := validateMetadataStrings(edgeName+" property", edge.Properties); err != nil {
			return err
		}
	}
	return nil
}

func validateMetadataStrings(name string, metadata shoal.Metadata) error {
	for key, value := range metadata {
		if err := validateWireString(name+" key", key); err != nil {
			return err
		}
		if err := validateWireString(name+" value", value); err != nil {
			return err
		}
	}
	return nil
}

func validateWireString(name, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", name)
	}
	return nil
}

func validateProtoResponse(response *knowledgepb.RetrieveResponse) error {
	if response == nil {
		return fmt.Errorf("knowledge retrieval response is required")
	}
	if err := validateWireString("request ID", response.GetRequestId()); err != nil {
		return err
	}
	if err := validateWireString(
		"embedding space ID", response.GetEmbeddingSpaceId(),
	); err != nil {
		return err
	}
	for index, id := range response.GetEmbeddingSpaceIds() {
		if err := validateWireString(
			fmt.Sprintf("embedding space constituent ID %d", index), id,
		); err != nil {
			return err
		}
	}
	for resultIndex, result := range response.GetResults() {
		if result == nil {
			return fmt.Errorf("result %d is required", resultIndex)
		}
		if err := validateWireString(
			fmt.Sprintf("result %d ID", resultIndex), result.GetId(),
		); err != nil {
			return err
		}
		for evidenceIndex, evidence := range result.GetEvidence() {
			if evidence == nil {
				return fmt.Errorf(
					"result %d evidence %d is required", resultIndex, evidenceIndex)
			}
			if evidence.GetCitation() == nil {
				return fmt.Errorf(
					"result %d evidence %d citation is required",
					resultIndex, evidenceIndex)
			}
			if evidence.GetPath() != nil &&
				len(evidence.GetPath().GetNodes()) == 0 &&
				len(evidence.GetPath().GetEdges()) == 0 {
				return fmt.Errorf(
					"result %d evidence %d path cannot be empty",
					resultIndex, evidenceIndex)
			}
		}
		if explanation := result.GetExplanation(); explanation != nil {
			for modeIndex, mode := range explanation.GetModes() {
				if _, err := modeFromProto(mode); err != nil {
					return fmt.Errorf(
						"result %d explanation mode %d: %w",
						resultIndex, modeIndex, err)
				}
			}
		}
	}
	return nil
}

func pathPresent(path graph.Path) bool {
	return len(path.Nodes) > 0 || len(path.Edges) > 0
}
