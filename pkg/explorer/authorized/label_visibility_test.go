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

package authorized_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized/authorizedtest"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var labelNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func labelDecision(t *testing.T, policies [][]byte, role auth.ServiceRole, ceiling shoal.ID) auth.Decision {
	t.Helper()
	operations := []auth.Operation{auth.OperationRead}
	binding := ""
	if role == auth.ServiceRoleActionExecution {
		operations = []auth.Operation{auth.OperationExecute}
		binding = "exec"
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "reader", Actor: "reader-actor",
		AuthorizationDomain:    authorizedtest.Domain,
		AllowedOperations:      operations,
		PermittedSourceIDs:     [][]byte{authorizedtest.SourceID, authorizedtest.OutputSourceID},
		PermittedPolicyIDs:     policies,
		PolicyGeneration:       1,
		AuthenticationExpires:  labelNow.Add(time.Hour),
		RequestID:              "request",
		ServiceRole:            role,
		ServiceCeilingIdentity: ceiling,
		ExecutorBinding:        binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

type fixedResolver struct {
	decision auth.Decision
	err      error
}

func (r fixedResolver) Resolve(context.Context) (auth.Decision, error) {
	return r.decision, r.err
}

type failingCeilings struct{}

func (failingCeilings) ResolveServiceCeiling(
	context.Context, auth.Decision,
) (auth.ServiceCeiling, error) {
	return auth.ServiceCeiling{}, errors.New("ceiling catalog down")
}

func evaluator(t *testing.T, resolver auth.Resolver, ceilings authorized.CeilingResolver) *authorized.LabelVisibility {
	t.Helper()
	visibility, err := authorized.NewLabelVisibility(authorized.LabelVisibilityConfig{
		Resolver: resolver, Ceilings: ceilings,
		Clock: func() time.Time { return labelNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return visibility
}

func labelPolicyID(t *testing.T) []byte {
	t.Helper()
	id, err := authorized.LabelPolicyID(authorizedtest.SourceID, authorizedtest.DocumentLabel)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestLabelVisibilityDecidesForAUser: a user may see a stored label set
// exactly when its grants cover every structured term. A free-form or
// malformed term is held by nobody and is false, never an error.
func TestLabelVisibilityDecidesForAUser(t *testing.T) {
	holder := labelDecision(t, [][]byte{authorizedtest.PolicyID, labelPolicyID(t)}, "", "")
	outsider := labelDecision(t, [][]byte{authorizedtest.PolicyID}, "", "")
	for _, probe := range []struct {
		name     string
		decision auth.Decision
		labels   []string
		want     bool
	}{
		{"holder, structured label", holder, authorizedtest.DocumentLabelTerms, true},
		{"outsider, structured label", outsider, authorizedtest.DocumentLabelTerms, false},
		{"holder, free-form label", holder, []string{authorizedtest.DocumentLabel}, false},
		{"holder, structured and free-form", holder,
			append(append([]string(nil), authorizedtest.DocumentLabelTerms...), "secret"), false},
		{"holder, malformed term", holder, []string{"g:not-base32:e:1"}, false},
		{"holder, unlabelled", holder, nil, true},
		{"outsider, unlabelled", outsider, nil, true},
	} {
		t.Run(probe.name, func(t *testing.T) {
			got, err := evaluator(t, fixedResolver{decision: probe.decision}, nil).
				VisibleToReader(context.Background(), probe.labels)
			if err != nil {
				t.Fatalf("VisibleToReader = %v; a label answer is never an error", err)
			}
			if got != probe.want {
				t.Fatalf("visible = %v, want %v", got, probe.want)
			}
		})
	}
}

// TestLabelVisibilityFailuresAreErrors: a resolver or ceiling-resolver
// failure is the question failing, not a refusal.
func TestLabelVisibilityFailuresAreErrors(t *testing.T) {
	_, err := evaluator(t, fixedResolver{
		err: shoal.NewError(shoal.ErrorUnavailable, "resolver down"),
	}, nil).VisibleToReader(context.Background(), authorizedtest.DocumentLabelTerms)
	if err == nil {
		t.Fatal("a resolver failure was answered")
	}
	service := labelDecision(t, [][]byte{authorizedtest.PolicyID, labelPolicyID(t)},
		auth.ServiceRoleDataRead, "reader-ceiling")
	_, err = evaluator(t, fixedResolver{decision: service}, failingCeilings{}).
		VisibleToReader(context.Background(), authorizedtest.DocumentLabelTerms)
	if err == nil {
		t.Fatal("a ceiling-resolver failure was answered")
	}
	if _, err := authorized.NewLabelVisibility(authorized.LabelVisibilityConfig{
		Clock: time.Now,
	}); err == nil {
		t.Fatal("an evaluator without a resolver was constructed")
	}
}

func ceilingConfig(identity shoal.ID, role auth.ServiceRole, terms ...string) auth.ServiceCeilingConfig {
	labels := [][]byte{[]byte("svc:" + string(role))}
	for _, term := range terms {
		labels = append(labels, []byte(term))
	}
	return auth.ServiceCeilingConfig{
		Identity: identity, Role: role,
		Authorizations: accumulo.NewAuthorizations(labels...),
	}
}

// TestAnExecutorIsBoundedByItsCeiling: an executor service holding every
// grant is refused a label its configured ceiling lacks, and a service with
// no ceiling configured is refused every labelled set. Within its ceiling it
// sees a set carrying its own svc:<role> term, as at the tablet.
func TestAnExecutorIsBoundedByItsCeiling(t *testing.T) {
	role := auth.ServiceRoleActionExecution
	labels := append(append([]string(nil), authorizedtest.DocumentLabelTerms...),
		"svc:"+string(role))
	var withoutGrant []string
	for _, term := range authorizedtest.DocumentLabelTerms {
		if term[:2] != "g:" {
			withoutGrant = append(withoutGrant, term)
		}
	}
	ceilings, err := authorized.NewStaticCeilingResolver(
		ceilingConfig("full", role, authorizedtest.DocumentLabelTerms...),
		ceilingConfig("lacking", role, withoutGrant...),
	)
	if err != nil {
		t.Fatal(err)
	}
	policies := [][]byte{authorizedtest.PolicyID, labelPolicyID(t)}
	for _, probe := range []struct {
		ceiling shoal.ID
		want    bool
	}{
		{"full", true},
		{"lacking", false},
		{"unconfigured", false},
	} {
		t.Run(string(probe.ceiling), func(t *testing.T) {
			decision := labelDecision(t, policies, role, probe.ceiling)
			got, err := evaluator(t, fixedResolver{decision: decision}, ceilings).
				VisibleToReader(context.Background(), labels)
			if err != nil {
				t.Fatal(err)
			}
			if got != probe.want {
				t.Fatalf("visible = %v, want %v", got, probe.want)
			}
		})
	}
}

func TestStaticCeilingResolverRefusesADuplicateIdentity(t *testing.T) {
	role := auth.ServiceRoleDataRead
	if _, err := authorized.NewStaticCeilingResolver(
		ceilingConfig("same", role), ceilingConfig("same", role),
	); err == nil {
		t.Fatal("two ceilings with one identity were accepted")
	}
	if _, err := authorized.NewStaticCeilingResolver(auth.ServiceCeilingConfig{
		Identity: "bad", Role: role,
		Authorizations: accumulo.NewAuthorizations([]byte("secret")),
	}); err == nil {
		t.Fatal("a ceiling with a free-form label was accepted")
	}
}

// TestTheTranslatorUsesTheNodesOwnLabelPolicy: a free-form label on a known
// node becomes that node's label policy, exactly; structured terms pass
// through; a label no node's rule enforces, or a node the catalog does not
// know, leaves the label as it was, which no reader holds.
func TestTheTranslatorUsesTheNodesOwnLabelPolicy(t *testing.T) {
	f := authorizedtest.New(t)
	translator := f.Client.LabelTranslator()
	ctx := context.Background()
	for _, probe := range []struct {
		name  string
		nodes []shoal.ID
		in    []string
		want  []string
	}{
		{"labelled node", []shoal.ID{f.SecretNodeID},
			[]string{authorizedtest.DocumentLabel}, authorizedtest.DocumentLabelTerms},
		{"structured terms pass through", []shoal.ID{f.NodeID},
			authorizedtest.SecretLabels, authorizedtest.SecretLabels},
		{"no rule enforces the label", []shoal.ID{f.NodeID},
			[]string{authorizedtest.DocumentLabel}, []string{authorizedtest.DocumentLabel}},
		{"an unknown node", []shoal.ID{f.SecretNodeID, "node-unknown"},
			[]string{authorizedtest.DocumentLabel}, []string{authorizedtest.DocumentLabel}},
		{"no nodes", nil,
			[]string{authorizedtest.DocumentLabel}, []string{authorizedtest.DocumentLabel}},
		{"unlabelled", []shoal.ID{f.SecretNodeID}, nil, nil},
	} {
		t.Run(probe.name, func(t *testing.T) {
			got, err := translator.StructuredVisibility(ctx, probe.nodes, nil, probe.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, probe.want) {
				t.Fatalf("translated %v to %v, want %v", probe.in, got, probe.want)
			}
		})
	}
}
