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

package codematerializer

import (
	"testing"

	codeast "github.com/phrocker/shoal-oss/pkg/code"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestPreflightProjectedCountsEnforcesCanonicalGraphBounds(t *testing.T) {
	counts, err := preflightProjectedCounts(codeast.ParseResultCounts{
		Symbols: MaxGraphNodes - 3,
	})
	if err != nil {
		t.Fatalf("node boundary: %v", err)
	}
	if counts.nodes != MaxGraphNodes {
		t.Fatalf("node boundary count = %d", counts.nodes)
	}
	if _, err := preflightProjectedCounts(codeast.ParseResultCounts{
		Symbols: MaxGraphNodes - 2,
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("node overflow error = %v", err)
	}

	counts, err = preflightProjectedCounts(codeast.ParseResultCounts{
		Relationships: MaxGraphEdges - 2,
	})
	if err != nil {
		t.Fatalf("edge boundary: %v", err)
	}
	if counts.edges != MaxGraphEdges {
		t.Fatalf("edge boundary count = %d", counts.edges)
	}
	if _, err := preflightProjectedCounts(codeast.ParseResultCounts{
		Relationships: MaxGraphEdges - 1,
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("edge overflow error = %v", err)
	}
}

func TestPreflightProjectedCountsRejectsArithmeticOverflow(t *testing.T) {
	if _, err := preflightProjectedCounts(codeast.ParseResultCounts{
		SyntaxNodes: ^uint64(0),
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("arithmetic overflow error = %v", err)
	}
}
