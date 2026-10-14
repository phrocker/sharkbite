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

// Test-only views of AccessRule for the external label-migration tests.

// StripLabelPolicies returns rule without its label-namespace components:
// the rule a document labelled before #570 was registered under.
func StripLabelPolicies(rule AccessRule) (AccessRule, error) {
	bare, _, _, err := splitLabelledRule(rule)
	return bare, err
}

// RuleIncludes reports whether every component of inner is in outer.
func RuleIncludes(outer, inner AccessRule) bool { return ruleIncludes(outer, inner) }

// RulesEqual reports logical equality of two rules.
func RulesEqual(left, right AccessRule) bool { return left.equal(right) }
