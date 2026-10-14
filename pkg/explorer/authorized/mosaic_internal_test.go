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

package authorized

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestEmptySensitivityDomainIsProducible pins the precondition for the
// fail-closed guard below: a keyless AccessRule digests to the empty
// sensitivity domain. mustCloneRule swallows a clone failure and returns a
// zero AccessRule, so an ungoverned rule is reachable rather than statically
// impossible, and the guard is not dead code.
func TestEmptySensitivityDomainIsProducible(t *testing.T) {
	if domain := (AccessRule{}).sensitivityDomain(); domain != "" {
		t.Fatalf("keyless rule sensitivity domain = %q, want empty", domain)
	}
	if domain := mustCloneRule(AccessRule{}).sensitivityDomain(); domain != "" {
		t.Fatalf("cloned keyless rule domain = %q, want empty", domain)
	}
}

// TestMosaicBudgetFailsClosedOnEmptySensitivityDomain pins the internal
// consistency guard in applyMosaicBudget. An empty sensitivity domain means a
// document is not governed by any rule key; admitting it would collapse every
// such document into one shared compartment and silently bypass the budget.
// The guard must reject the read instead of returning a partial selection.
func TestMosaicBudgetFailsClosedOnEmptySensitivityDomain(t *testing.T) {
	budget := MosaicBudget{MaxDomains: 4, Window: time.Hour}
	accumulator, err := budget.accumulator(NewMemoryPolicyStore())
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{mosaic: budget, accumulator: accumulator}

	ungoverned := shoal.ID("doc-ungoverned")
	selection, err := client.applyMosaicBudget(
		context.Background(),
		auth.Decision{},
		time.Now().UTC(),
		[]shoal.ID{ungoverned},
		map[shoal.ID]string{},
	)
	if err == nil {
		t.Fatal("ungoverned document admitted; want fail-closed error")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("error code = %v, want ErrorInternal", err)
	}
	if len(selection.allowed) != 0 || selection.restricted != 0 {
		t.Fatalf(
			"failed selection leaked state: allowed=%d restricted=%d",
			len(selection.allowed), selection.restricted,
		)
	}
}
