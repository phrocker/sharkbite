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

// Package authorizedtest builds a real Explorer corpus behind a real
// authorized client, with sessions and folds recorded under structured
// output-policy labels and read by a principal holding them and by one that
// does not. The label tests of every surface that serves interaction records
// (#564, #567, #568) run against the same fixture, so each of them exercises
// the one evaluator in the authorized client rather than a fake of it.
package authorizedtest

import (
	"bytes"
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Principal subjects. Both are authorized on the corpus source, so both pass
// every touched-source check; only the holder holds the labels.
const (
	HolderSubject   = "label-holder"
	OutsiderSubject = "label-outsider"
)

var (
	Domain   = []byte("label-domain")
	SourceID = []byte("label-source")
	PolicyID = []byte("label-policy")
	// OutputSourceID and OutputPolicyID name the output policy a labelled
	// session requires of its reader, as a workspace output policy does.
	OutputSourceID = []byte("label-output-source")
	OutputPolicyID = []byte("label-project-x")
	// DocumentLabel is the free-form ingest label on the labelled document.
	DocumentLabel = "secret"
)

// SecretLabels is the structured output-policy label set the labelled
// records carry: the grant labels (d:, s:, g:) of the output policy.
var SecretLabels = mustTerms(OutputSourceID, OutputPolicyID)

// DocumentLabelTerms is the structured form of DocumentLabel on the labelled
// document: the grant labels of its (source, label) policy (#570).
var DocumentLabelTerms = mustTerms(SourceID, mustLabelPolicyID())

func mustLabelPolicyID() []byte {
	id, err := authorized.LabelPolicyID(SourceID, DocumentLabel)
	if err != nil {
		panic(err)
	}
	return id
}

func mustTerms(source, grant []byte) []string {
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: Domain, SourceID: source,
		GrantPolicyID: grant, Epoch: 1,
	})
	if err != nil {
		panic(err)
	}
	terms, err := policy.VisibilityTerms()
	if err != nil {
		panic(err)
	}
	labels, err := interaction.Conjoin(terms)
	if err != nil {
		panic(err)
	}
	return labels
}

// Operations is the grant both principals hold.
var Operations = []auth.Operation{
	auth.OperationIngest, auth.OperationList, auth.OperationRead,
	auth.OperationConnect, auth.OperationNeighborhood,
	auth.OperationRetrieve, auth.OperationValidate,
	auth.OperationTeamOverviewRead,
}

type generationReader struct{}

func (generationReader) CurrentPolicyGeneration(
	ctx context.Context, domain []byte,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !bytes.Equal(domain, Domain) {
		return 0, nil
	}
	return 1, nil
}

// Fixture is a corpus, its authorized client, one unlabelled source span and
// one span of a document labelled DocumentLabel.
type Fixture struct {
	Corpus    *explorer.Explorer
	Client    *authorized.Client
	Authority *auth.Authority
	// NodeID is a span of the unlabelled document.
	NodeID shoal.ID
	// SecretNodeID is a span of the document labelled DocumentLabel.
	SecretNodeID shoal.ID
	Snapshot     explorer.Snapshot
	expires      time.Time
	sequence     int
}

// New opens a fresh corpus and ingests the two documents as the holder. The
// client resolves trusted-service ceilings from ceilings; none configured
// means every trusted service is refused labelled records.
func New(t testing.TB, ceilings ...auth.ServiceCeilingConfig) *Fixture {
	t.Helper()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	authority := auth.NewAuthority()
	selector, err := authorized.NewStaticPolicySelector(SourceID, PolicyID)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := authorized.NewStaticCeilingResolver(ceilings...)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base: corpus, Resolver: authority.Resolver(), PolicySelector: selector,
		InteractionWriter: corpus, InteractionReader: corpus,
		SnapshotValidator: corpus, FoldStore: corpus,
		PolicyStore:      authorized.NewMemoryPolicyStore(),
		GenerationReader: generationReader{}, Clock: time.Now,
		CeilingResolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixture{
		Corpus: corpus, Client: client, Authority: authority,
		expires: time.Now().Add(time.Hour).Truncate(time.Second),
	}
	ctx := f.Context(t, HolderSubject)
	f.NodeID = f.ingest(t, ctx, "file:///label-open.md", nil)
	f.SecretNodeID = f.ingest(t, ctx, "file:///label-closed.md", shoal.Metadata{
		interaction.PropertyVisibility: DocumentLabel,
	})
	f.Snapshot, err = client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.EnsureInteractionSink(ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *Fixture) ingest(
	t testing.TB, ctx context.Context, uri string, metadata shoal.Metadata,
) shoal.ID {
	t.Helper()
	receipt, err := f.Client.Ingest(ctx, explorer.Source{
		URI:       uri,
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Labels\n\nEvidence a session touches: " + uri + "\n",
		Metadata:  metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.Client.Document(ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	span := firstSpan(view.Root)
	if span == "" {
		t.Fatal("ingested document has no span")
	}
	return span
}

func firstSpan(section explorer.SectionView) shoal.ID {
	if len(section.Spans) > 0 {
		return section.Spans[0].ID
	}
	for _, child := range section.Children {
		if id := firstSpan(child); id != "" {
			return id
		}
	}
	return ""
}

// Decision is subject's decision. Every call returns a new request ID but the
// same authorization projection, so the fingerprint is stable per subject.
// The holder additionally holds the output policy and the document label's
// policy; any other subject holds only the corpus source.
func (f *Fixture) Decision(t testing.TB, subject string) auth.Decision {
	t.Helper()
	f.sequence++
	sources := [][]byte{SourceID}
	policies := [][]byte{PolicyID}
	if subject == HolderSubject {
		sources = append(sources, OutputSourceID)
		policies = append(policies, OutputPolicyID, mustLabelPolicyID())
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(subject), Actor: shoal.ID(subject + "-actor"),
		AuthorizationDomain:   Domain,
		AllowedOperations:     append([]auth.Operation(nil), Operations...),
		PermittedSourceIDs:    sources,
		PermittedPolicyIDs:    policies,
		PolicyGeneration:      1,
		AuthenticationExpires: f.expires,
		RequestID: shoal.ID(subject + "-request-" +
			strconv.Itoa(f.sequence)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// ServiceContext binds a trusted data_read service decision holding the
// holder's grants, bounded by the ceiling named ceilingIdentity.
func (f *Fixture) ServiceContext(t testing.TB, ceilingIdentity shoal.ID) context.Context {
	t.Helper()
	f.sequence++
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "label-service", Actor: "label-service-actor",
		AuthorizationDomain: Domain,
		AllowedOperations: []auth.Operation{
			auth.OperationList, auth.OperationRead, auth.OperationRetrieve,
		},
		PermittedSourceIDs: [][]byte{SourceID, OutputSourceID},
		PermittedPolicyIDs: [][]byte{
			PolicyID, OutputPolicyID, mustLabelPolicyID(),
		},
		PolicyGeneration:       1,
		AuthenticationExpires:  f.expires,
		RequestID:              shoal.ID("label-service-request-" + strconv.Itoa(f.sequence)),
		ServiceRole:            auth.ServiceRoleDataRead,
		ServiceCeilingIdentity: ceilingIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := f.Authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// Context binds subject's decision.
func (f *Fixture) Context(t testing.TB, subject string) context.Context {
	t.Helper()
	ctx, err := f.Authority.Binder().Bind(
		context.Background(), f.Decision(t, subject))
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// Record writes one session as subject that touches the unlabelled span,
// under the given output labels (none for an unlabelled session).
func (f *Fixture) Record(
	t testing.TB, subject string, id shoal.ID, labels []string,
) interaction.Session {
	t.Helper()
	return f.RecordOn(t, subject, id, f.NodeID, labels)
}

// RecordOn writes one session as subject that touches node, under the given
// output labels. The trusted sink stamps its own RecordedAt.
func (f *Fixture) RecordOn(
	t testing.TB, subject string, id, node shoal.ID, labels []string,
) interaction.Session {
	t.Helper()
	return f.RecordWith(t, subject, id, node, labels, labels)
}

// RecordWith writes one session whose context carries contextLabels (stamped
// on the derived graph and its expression) and whose own output restriction
// is sessionLabels. A producer that keeps an output label off the session is
// what makes the expression wider than the restriction.
func (f *Fixture) RecordWith(
	t testing.TB, subject string, id, node shoal.ID,
	contextLabels, sessionLabels []string,
) interaction.Session {
	t.Helper()
	labels := contextLabels
	decision := f.Decision(t, subject)
	ctx, err := f.Authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) > 0 {
		ctx, err = interaction.WithRequiredVisibility(ctx, labels)
		if err != nil {
			t.Fatal(err)
		}
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       id,
		RecordedAt:               f.Snapshot.AsOf.Add(time.Second).UTC(),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(f.Snapshot.ID),
		SnapshotAsOf:             f.Snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{node},
		// The chat path stamps the workspace output policy on the session
		// itself as well as on the context (webapi chat_service.go), so a
		// labelled record carries the expression in both places.
		RequiredVisibility: append([]string(nil), sessionLabels...),
	}
	persisted, err := f.Client.RecordInteractionResult(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	return persisted
}

// Fold folds sessions as subject.
func (f *Fixture) Fold(
	t testing.TB, subject string, sessions ...shoal.ID,
) explorer.FoldResult {
	t.Helper()
	result, err := f.Client.FoldInteractions(
		f.Context(t, subject), explorer.FoldRequest{SessionIDs: sessions})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Markers is every label string a labelled record carries, structured or
// free-form. None may reach a reader that does not hold the labels.
func Markers() []string {
	markers := append([]string(nil), SecretLabels...)
	markers = append(markers, DocumentLabelTerms...)
	return append(markers, DocumentLabel)
}

// Leaks reports which markers body contains.
func Leaks(body []byte) []string {
	var leaked []string
	for _, marker := range Markers() {
		if bytes.Contains(body, []byte(marker)) {
			leaked = append(leaked, marker)
		}
	}
	return leaked
}
