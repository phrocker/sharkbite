// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package routerwire

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionlinear"
	"github.com/phrocker/shoal-oss/internal/routershadow"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/extraction"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/router"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The test world composes the real components: an explorer corpus, the
// authorized client over a memory or durable policy store, a fleet registry
// and dispatch service, and real auth decisions. Visible things live under
// source/policy A; hidden things under B, which alice cannot see and bob can.
// A world built with hidden=false has none of the hidden things at all.

var (
	at       = time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	domain   = []byte("router-domain")
	sourceA  = []byte("source-a")
	policyA  = []byte("policy-a")
	sourceB  = []byte("source-b")
	policyB  = []byte("policy-b")
	hostKey  = []byte("router-shadow-test-host-key:0123456789abcdef")
	modelRaw = func() []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "pkg", "router", "testdata", "model", "router-pair-v1.json"))
		if err != nil {
			panic(err)
		}
		return b
	}()
)

type generations struct{}

func (generations) CurrentPolicyGeneration(ctx context.Context, _ []byte) (int64, error) {
	return 1, ctx.Err()
}

var readerOps = []auth.Operation{
	auth.OperationNeighborhood, auth.OperationAgentResolve, auth.OperationInvoke,
	auth.OperationDispatch, auth.OperationRead, auth.OperationList,
}

var adminOps = []auth.Operation{
	auth.OperationIngest, auth.OperationList, auth.OperationRead, auth.OperationConnect,
	auth.OperationGraphMaterialize, auth.OperationNeighborhood, auth.OperationRetrieve,
	auth.OperationAgentRegister, auth.OperationAgentResolve, auth.OperationInvoke, auth.OperationDispatch,
}

type world struct {
	t          testing.TB
	hidden     bool
	authority  *auth.Authority
	base       *explorer.Explorer
	store      authorized.PolicyStore
	reader     *authorized.Client
	fleet      *fleet.Service
	dispatch   *fleet.DispatchService
	dispatched *memoryDispatchStore
	registry   *memoryStore
	bundle     *lexicon.Bundle
	ids        map[string]shoal.ID
	nodes      []graph.Node
	recorder   *routershadow.MemoryRecorder
	service    *routershadow.Service
	config     routershadow.Config
	grammars   *router.GrammarSet
}

func (w *world) ctx(subject string, sources, policies [][]byte, ops []auth.Operation) context.Context {
	w.t.Helper()
	d, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(subject), Actor: shoal.ID(subject + "-actor"),
		AuthorizationDomain: domain, AllowedOperations: ops,
		PermittedSourceIDs: sources, PermittedPolicyIDs: policies,
		PolicyGeneration: 1, AuthenticationExpires: at.Add(time.Hour),
		RequestID: shoal.ID(subject + "-request"), CorrelationID: shoal.ID(subject + "-correlation"),
	})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, err := w.authority.Binder().Bind(context.Background(), d)
	if err != nil {
		w.t.Fatal(err)
	}
	return ctx
}

func (w *world) admin() context.Context {
	return w.ctx("admin", [][]byte{sourceA, sourceB}, [][]byte{policyA, policyB}, adminOps)
}
func (w *world) alice() context.Context {
	return w.ctx("alice", [][]byte{sourceA}, [][]byte{policyA}, readerOps)
}
func (w *world) bob() context.Context {
	return w.ctx("bob", [][]byte{sourceA, sourceB}, [][]byte{policyA, policyB}, readerOps)
}

func (w *world) client(source, policy []byte) *authorized.Client {
	w.t.Helper()
	selector, err := authorized.NewStaticPolicySelector(source, policy)
	if err != nil {
		w.t.Fatal(err)
	}
	var vector authorized.VectorScorer = w.base
	var snapshots authorized.SnapshotValidator = w.base
	c, err := authorized.NewClient(authorized.Config{
		Base: w.base, VectorScorer: vector, InteractionWriter: w.base, InteractionReader: w.base,
		SnapshotValidator: snapshots, DerivedAssertionReader: w.base, FoldStore: w.base,
		OntologyProposalStore: w.base,
		Resolver:              w.authority.Resolver(), PolicySelector: selector,
		PolicyStore: w.store, GenerationReader: generations{}, Clock: func() time.Time { return at },
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return c
}

type nodeSpec struct {
	key, kind, name, alias, concept string
}

var (
	conceptService     = mustConcept("service")
	conceptTeam        = mustConcept("team")
	conceptEnvironment = mustConcept("environment")
	conceptContractor  = mustConcept("contractor")
)

func mustConcept(key string) ontology.ConceptDefinition {
	c, err := ontology.NewConceptDefinition(key, key, "", nil, nil)
	if err != nil {
		panic(err)
	}
	return c
}

var visibleNodes = []nodeSpec{
	{key: "payments", kind: "service", name: "Payments", concept: "service"},
	{key: "checkout", kind: "service", name: "Checkout", concept: "service"},
	{key: "core-api", kind: "service", name: "Core API", alias: "core", concept: "service"},
	{key: "core-team", kind: "team", name: "Core Team", alias: "core", concept: "team"},
	{key: "platform", kind: "team", name: "Platform Team", concept: "team"},
	{key: "prod", kind: "environment", name: "Production", alias: "prod", concept: "environment"},
}

// Hidden nodes: one with a fresh name, and one that shares visible aliases,
// which must neither appear nor make a visible mention ambiguous.
var hiddenNodes = []nodeSpec{
	{key: "nightjar", kind: "service", name: "Project Nightjar", alias: "nightjar", concept: "service"},
	{key: "payments-shadow", kind: "service", name: "Payments", alias: "prod", concept: "service"},
}

func conceptID(key string) shoal.ID {
	return map[string]shoal.ID{"service": conceptService.ID(), "team": conceptTeam.ID(), "environment": conceptEnvironment.ID()}[key]
}

func (w *world) materialize(c *authorized.Client, namespace string, source, policy []byte, specs []nodeSpec) {
	w.t.Helper()
	snapshot, err := w.base.Snapshot(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	request := explorer.GraphMaterializationRequest{
		Namespace: []byte(namespace), MutationID: shoal.ID("router-" + namespace),
		SourceID: source, PolicyID: policy, ExpectedSnapshot: snapshot,
	}
	for _, s := range specs {
		props := shoal.Metadata{"name": s.name, extraction.GraphPropertyOntologyConceptID: string(conceptID(s.concept))}
		if s.alias != "" {
			props[lexicon.PropertyAliasPrefix+"0"] = s.alias
		}
		request.Nodes = append(request.Nodes, explorer.GraphNodeSpec{Key: []byte(s.key), Kind: s.kind, Properties: props})
	}
	result, err := c.MaterializeGraph(w.admin(), request)
	if err != nil {
		w.t.Fatal(err)
	}
	w.nodes = append(w.nodes, result.GraphNodes...)
	for _, id := range result.Nodes {
		w.ids[string(id.Key)] = id.ID
	}
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

var (
	restartSchema = schema(`{"type":"object","required":["service","environment"],"additionalProperties":false,"properties":{"service":{"type":"string"},"environment":{"type":"string"}}}`)
	flushSchema   = schema(`{"type":"object","required":["service"],"additionalProperties":false,"properties":{"service":{"type":"string"},"mode":{"type":"string","enum":["soft","hard"]}}}`)
	drainSchema   = schema(`{"type":"object","required":["environment"],"additionalProperties":false,"properties":{"environment":{"type":"string"}}}`)
	rotateSchema  = schema(`{"type":"object","required":["service"],"additionalProperties":false,"properties":{"service":{"type":"string"}}}`)
	objectSchema  = schema(`{"type":"object"}`)
)

func action(name string, input json.RawMessage, approval bool) fleet.Action {
	return fleet.Action{Name: name, InputSchema: input, OutputSchema: objectSchema, RequiresApproval: approval}
}

func (w *world) register(id string, source, policy []byte, capabilities []fleet.Capability) {
	w.t.Helper()
	ctx := w.admin()
	_, err := w.fleet.Register(ctx, fleet.RegisterRequest{
		Context:         fleet.RequestContext{RequestID: "admin-request", CorrelationID: "admin-correlation", ReasonCode: "operator_request", Deadline: at.Add(time.Minute)},
		RegistrationKey: shoal.ID("key-" + id),
		Spec: fleet.Spec{
			ID: shoal.ID(id), AuthorizationDomain: domain,
			Scopes:      []fleet.Scope{{SourceID: source, PolicyID: policy}},
			ExecutorRef: "exec", Capabilities: capabilities, LeaseExpiresAt: at.Add(30 * time.Minute),
		},
	})
	if err != nil {
		w.t.Fatalf("register %s: %v", id, err)
	}
}

const (
	grammarRestart = `{"schema":"shoal.router.grammar/v1","target":{"kind":"action","capability":"ops","action":"restart_service"},
"cues":["restart","bounce"],"patterns":["[please] restart {service} in {environment}","bounce {service} in {environment}","restart {service}"],
"slots":[{"name":"service","node_kinds":["service"]},{"name":"environment","node_kinds":["environment"]}],
"input":{"service":"$service","environment":"$environment"}}`
	// The flush template repeats "mode": canonicalization keeps the last.
	grammarFlush = `{"schema":"shoal.router.grammar/v1","target":{"kind":"action","capability":"ops","action":"flush_cache"},
"cues":["flush","cache"],"patterns":["flush [the] {service} cache"],
"slots":[{"name":"service","node_kinds":["service"]}],
"input":{"mode":"hard","service":"$service","mode":"soft"}}`
	grammarDrain = `{"schema":"shoal.router.grammar/v1","target":{"kind":"action","capability":"maintenance","action":"drain"},
"cues":["drain"],"patterns":["drain {environment}"],
"slots":[{"name":"environment","node_kinds":["environment"]}],"input":{"environment":"$environment"}}`
	grammarRotate = `{"schema":"shoal.router.grammar/v1","target":{"kind":"action","capability":"secops","action":"rotate_credentials"},
"cues":["rotate","credentials"],"patterns":["rotate credentials for {service}"],
"slots":[{"name":"service","node_kinds":["service"]}],"input":{"service":"$service"}}`
	grammarRisk = `{"schema":"shoal.router.grammar/v1","target":{"kind":"decision","profile":"service-operation-risk"},
"cues":["risky","risk"],"patterns":["how risky is {operation} {service}"],
"slots":[{"name":"service","node_kinds":["service"]},{"name":"operation","enum":{"restart":["restarting"],"rollback":["rolling back"]}}],
"input":{"service":"$service","operation":"$operation"}}`
	grammarBreach = `{"schema":"shoal.router.grammar/v1","target":{"kind":"decision","profile":"breach-exposure"},
"cues":["breach","exposure"],"patterns":["what is the breach exposure of {service}"],
"slots":[{"name":"service","node_kinds":["service"]}],"input":{"service":"$service"}}`
	grammarOwner = `{"schema":"shoal.router.grammar/v1","target":{"kind":"lookup","template":"lookup:owned_by:out"},
"cues":["owns","owner"],"patterns":["who owns {subject}"],"slots":[{"name":"subject"}]}`
)

func (w *world) decisionTargets() []DecisionProfile {
	risk := DecisionProfile{
		Target: routershadow.DecisionTarget{
			ProfileID: "service-operation-risk", ProfileRevisionID: "r1", TaskID: "task:service-operation-risk",
			Name:       "service operation risk",
			SlotSchema: schema(`{"type":"object","required":["service","operation"],"additionalProperties":false,"properties":{"service":{"type":"string"},"operation":{"type":"string","enum":["restart","rollback"]}}}`),
		},
		TaskResource: auth.ResourceRequest{AuthorizationDomain: domain, SourceID: sourceA, PolicyID: policyA, ObjectID: "task:service-operation-risk"},
	}
	targets := []DecisionProfile{risk}
	if w.hidden {
		targets = append(targets, DecisionProfile{
			Target: routershadow.DecisionTarget{
				ProfileID: "breach-exposure", ProfileRevisionID: "r1", TaskID: "task:breach-exposure",
				Name:       "breach exposure",
				SlotSchema: schema(`{"type":"object","required":["service"],"properties":{"service":{"type":"string"}}}`),
			},
			TaskResource: auth.ResourceRequest{AuthorizationDomain: domain, SourceID: sourceB, PolicyID: policyB, ObjectID: "task:breach-exposure"},
		})
	}
	return targets
}

// ontologyVersions: v1 relates services to teams; v2 widens it to
// contractors, on evidence from a document.
func ontologyVersions(t testing.TB) (ontology.OntologySchema, ontology.OntologyVersion, ontology.OntologyVersion, ontology.RelationshipDefinition, ontology.RelationshipDefinition) {
	t.Helper()
	s, err := ontology.NewOntologySchema("router-world", "Router World", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := ontology.NewRelationshipDefinition("owned_by", "Owned by", "",
		[]shoal.ID{conceptService.ID()}, []shoal.ID{conceptTeam.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := ontology.NewOntologyVersion(s, "1", at.Add(-2*time.Hour),
		[]ontology.ConceptDefinition{conceptService, conceptTeam, conceptEnvironment}, []ontology.RelationshipDefinition{owned}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	widened, err := ontology.NewRelationshipDefinition("owned_by", "Owned by", "",
		[]shoal.ID{conceptService.ID()}, []shoal.ID{conceptTeam.ID(), conceptContractor.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := ontology.NewOntologyVersion(s, "2", at.Add(-2*time.Hour+time.Second),
		[]ontology.ConceptDefinition{conceptService, conceptTeam, conceptEnvironment, conceptContractor},
		[]ontology.RelationshipDefinition{widened}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, base, target, owned, widened
}

// publishOntology publishes v2 on evidence from a document under source B,
// so only a caller who can see that document can see the published version.
func (w *world) publishOntology(writerB *authorized.Client) {
	w.t.Helper()
	ctx := context.Background()
	s, base, target, owned, widened := ontologyVersions(w.t)
	ingested, err := writerB.Ingest(w.admin(), explorer.Source{
		URI: "memory://ownership-review", Title: "Ownership review", MediaType: explorer.MediaTypeText,
		Content: "Contractors may own services.",
	})
	if err != nil {
		w.t.Fatal(err)
	}
	view, err := writerB.Document(w.admin(), ingested.Document.ID, ingested.Revision.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	citation := document.Citation{
		DocumentID: view.Document.ID, RevisionID: view.Revision.ID,
		SectionID: view.Root.Section.ID, Range: view.Root.Section.Range,
	}
	quote, err := w.base.ResolveOntologyEvidenceCitation(ctx, citation)
	if err != nil {
		w.t.Fatal(err)
	}
	evidence, err := ontology.NewEvidenceRef(citation, quote, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	morphism, err := ontology.NewOntologyMorphism(ontology.MorphismConfig{
		Kind: ontology.MorphismWiden, SourceVersion: base, TargetVersion: target,
		Sources: []shoal.ID{owned.ID()}, Targets: []shoal.ID{widened.ID()},
		Evidence: []ontology.EvidenceRef{evidence}, Rationale: "evidence-backed widening",
	})
	if err != nil {
		w.t.Fatal(err)
	}
	proposal, err := ontology.NewGovernedProposalWithMorphisms(s, base, target,
		[]ontology.OntologyMorphism{morphism}, "author", "proposal", at.Add(-time.Hour), nil)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.base.CreateOntologyProposal(ctx, proposal, base); err != nil {
		w.t.Fatal(err)
	}
	for i, state := range []ontology.ProposalState{ontology.ProposalSubmitted, ontology.ProposalApproved, ontology.ProposalPublished} {
		if proposal, err = w.base.TransitionOntologyProposal(ctx, proposal.ID(), state, "governor", "publish",
			at.Add(-time.Hour+time.Duration(i+1)*time.Minute)); err != nil {
			w.t.Fatal(err)
		}
	}
}

func newWorld(t testing.TB, store authorized.PolicyStore, hidden bool) *world {
	t.Helper()
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	base, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	w := &world{t: t, hidden: hidden, authority: authority, base: base, store: store, ids: map[string]shoal.ID{}, recorder: &routershadow.MemoryRecorder{}}
	writerA := w.client(sourceA, policyA)
	writerB := w.client(sourceB, policyB)
	w.materialize(writerA, "visible", sourceA, policyA, visibleNodes)
	if hidden {
		w.materialize(writerB, "hidden", sourceB, policyB, hiddenNodes)
		w.publishOntology(writerB)
	}
	w.reader = w.client(sourceA, policyA)

	_, _, target, _, widened := ontologyVersions(t)
	snapshot, err := base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w.bundle, err = lexicon.Build(lexicon.Input{
		Snapshot: lexicon.Snapshot{ID: snapshot.ID, AsOf: snapshot.AsOf, Frontier: snapshot.Frontier},
		Nodes:    w.nodes, Relationships: []ontology.RelationshipDefinition{widened},
	}, lexicon.Limits{})
	if err != nil {
		t.Fatal(err)
	}

	w.registry = newMemoryStore()
	w.fleet, err = fleet.NewService(fleet.Config{
		Store: w.registry, Resolver: authority.Resolver(), Recorder: &lifecycleRecorder{},
		Snapshots: fixedSnapshot{}, Executors: executors{}, Clock: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	w.register("agent-ops", sourceA, policyA, []fleet.Capability{
		{Name: "ops", Actions: []fleet.Action{
			action("restart_service", restartSchema, true),
			action("flush_cache", flushSchema, false),
		}},
		{Name: "maintenance", Actions: []fleet.Action{action("drain", drainSchema, false)}},
	})
	if hidden {
		w.register("agent-hidden", sourceB, policyB, []fleet.Capability{
			{Name: "secops", Actions: []fleet.Action{action("rotate_credentials", rotateSchema, true)}},
			{Name: "maintenance", Actions: []fleet.Action{action("drain", drainSchema, false)}},
		})
	}
	w.dispatched = newMemoryDispatchStore()
	w.dispatch, err = fleet.NewDispatchService(fleet.DispatchConfig{
		Store: w.dispatched, Registry: w.fleet, Resolver: authority.Resolver(),
		Recorder: actionRecorder{}, Events: events{}, Clock: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}

	docs := [][]byte{}
	for _, g := range []string{grammarRestart, grammarFlush, grammarDrain, grammarRotate, grammarRisk, grammarBreach, grammarOwner} {
		docs = append(docs, []byte(g))
	}
	w.grammars, err = router.ParseGrammarSet(docs)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ontology.NewOntologyIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	_, baseVersion, _, _, _ := ontologyVersions(t)
	provider, err := newProvider()
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return at }
	lookups, err := Lookups(w.reader, OntologyBinding{Configured: baseVersion, Identity: identity, Published: target}, w.bundle)
	if err != nil {
		t.Fatal(err)
	}
	w.config = routershadow.Config{
		Caller:    Caller(authority.Resolver()),
		Targets:   Targets(w.fleet, authority.Resolver(), clock),
		Decisions: Decisions(authority.Resolver(), clock, w.decisionTargets()),
		Lookups:   lookups,
		Mentions:  Mentions(w.reader),
		Concepts:  Concepts(w.reader),
		Validator: Validator(),
		Lexicon:   w.bundle, Grammars: w.grammars,
		Decider:  &routershadow.Decider{Predictor: Predictor(provider, "router-pair-v1:test"), Clock: clock},
		Recorder: w.recorder, HostKey: hostKey, Clock: clock,
	}
	w.service, err = routershadow.New(w.config)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func newProvider() (*decisionlinear.Provider, error) {
	sum := sha256.Sum256(modelRaw)
	return decisionlinear.New(decisionlinear.Config{ModelBytes: modelRaw, ExpectedSHA256: hex.EncodeToString(sum[:]), ReleaseID: "router-pair-v1:test"})
}

func withPolicyStores(t *testing.T, run func(*testing.T, func() authorized.PolicyStore)) {
	t.Run("memory", func(t *testing.T) {
		run(t, func() authorized.PolicyStore { return authorized.NewMemoryPolicyStore() })
	})
	t.Run("durable", func(t *testing.T) {
		run(t, func() authorized.PolicyStore {
			s, err := authorized.OpenDurablePolicyStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		})
	})
}

// --- fleet doubles: in-memory storage and recorders only; the services are real.

type fixedSnapshot struct{}

func (fixedSnapshot) InteractionSnapshot(context.Context) (explorer.Snapshot, error) {
	return explorer.Snapshot{ID: "snapshot", AsOf: at, Frontier: 1}, nil
}

type executors struct{}

func (executors) ResolveExecutor(string) (fleet.Executor, bool) { return struct{}{}, true }

type lifecycleRecorder struct{}

func (*lifecycleRecorder) RecordLifecycle(context.Context, fleet.Lifecycle) error { return nil }

type actionRecorder struct{}

func (actionRecorder) RecordAction(_ context.Context, audit fleet.ActionAudit) error {
	return audit.Operation.Validate()
}

type events struct{}

func (events) PublishActionEvent(context.Context, string, fleet.ActionRecord) error { return nil }

func (events) MayPublishActionEvent(
	context.Context, string, fleet.ActionRecord,
) (bool, error) {
	return true, nil
}

// memoryStore keeps descriptors in ID order, so a listing is a binary search
// and the many-hidden-descriptors test stays fast.
type memoryStore struct {
	mu      sync.Mutex
	records map[shoal.ID]fleet.Stored
	order   []shoal.ID
	scans   int
}

func newMemoryStore() *memoryStore { return &memoryStore{records: map[shoal.ID]fleet.Stored{}} }

func (s *memoryStore) Apply(_ context.Context, m fleet.Mutation) (fleet.Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.records[m.Descriptor.ID]
	if (!exists && m.ExpectedGeneration != 0) || (exists && current.Descriptor.Generation != m.ExpectedGeneration) {
		return fleet.Stored{}, shoal.NewError(shoal.ErrorConflict, "generation conflict")
	}
	stored := fleet.Stored{Descriptor: m.Descriptor, RegistrationDigest: sha256.Sum256([]byte(m.RegistrationKey)), Epoch: m.Descriptor.Generation}
	s.put([]fleet.Stored{stored})
	return stored, nil
}

// put stores descriptors directly, as a registry holding them would.
func (s *memoryStore) put(stored []fleet.Stored) {
	for _, st := range stored {
		if _, exists := s.records[st.Descriptor.ID]; !exists {
			s.order = append(s.order, st.Descriptor.ID)
		}
		s.records[st.Descriptor.ID] = st
	}
	sort.Slice(s.order, func(i, j int) bool { return s.order[i] < s.order[j] })
}

func (s *memoryStore) Get(_ context.Context, id shoal.ID) (fleet.Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.records[id]
	if !ok {
		return fleet.Stored{}, shoal.NewError(shoal.ErrorNotFound, "not found")
	}
	return stored, nil
}

func (s *memoryStore) List(_ context.Context, cursor []byte, limit int) (fleet.StoredPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scans++
	i := sort.Search(len(s.order), func(i int) bool { return s.order[i] > shoal.ID(cursor) })
	page := fleet.StoredPage{}
	for ; i < len(s.order); i++ {
		if len(page.Entries) == limit {
			page.Next = []byte(page.Entries[len(page.Entries)-1].Descriptor.ID)
			break
		}
		page.Entries = append(page.Entries, s.records[s.order[i]])
	}
	return page, nil
}

func (s *memoryStore) scanCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scans
}

type memoryDispatchStore struct {
	mu          sync.Mutex
	records     map[string]fleet.ActionRecord
	transitions map[string]fleet.ActionTransition
}

func newMemoryDispatchStore() *memoryDispatchStore {
	return &memoryDispatchStore{records: map[string]fleet.ActionRecord{}, transitions: map[string]fleet.ActionTransition{}}
}

func (s *memoryDispatchStore) GetAction(_ context.Context, id []byte) (fleet.ActionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[string(id)]
	if !ok {
		return fleet.ActionRecord{}, fleet.ErrActionNotFound
	}
	return r, nil
}

func (s *memoryDispatchStore) ApplyAction(_ context.Context, m fleet.DispatchMutation) (fleet.ActionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[string(m.Record.ID)]; exists && m.ExpectedVersion == 0 {
		return fleet.ActionRecord{}, fleet.ErrActionConflict
	}
	s.records[string(m.Record.ID)] = m.Record
	if m.TransitionKind != "" {
		transition, err := fleet.NewActionTransition(m.Token, m.TransitionKind, m.Record)
		if err != nil {
			return fleet.ActionRecord{}, err
		}
		s.transitions[string(transition.ID)] = transition
	}
	return m.Record, nil
}

func (s *memoryDispatchStore) ScanActions(_ context.Context, after []byte, limit int) (fleet.ActionPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.records {
		if k > string(after) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	page := fleet.ActionPage{}
	for _, k := range keys {
		page.Actions = append(page.Actions, s.records[k])
		if len(page.Actions) == limit {
			page.Next = []byte(k)
			break
		}
	}
	return page, nil
}

func (s *memoryDispatchStore) PendingActionTransitions(_ context.Context, id, after []byte, limit int) (fleet.ActionTransitionPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	page := fleet.ActionTransitionPage{}
	var keys []string
	for k, tr := range s.transitions {
		if bytes.Equal(tr.Record.ID, id) && k > string(after) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(page.Transitions) == limit {
			page.Next = []byte(k)
			break
		}
		page.Transitions = append(page.Transitions, s.transitions[k])
	}
	return page, nil
}

func (s *memoryDispatchStore) CompleteActionTransition(_ context.Context, tr fleet.ActionTransition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.transitions[string(tr.ID)]; !ok {
		return fleet.ErrActionConflict
	}
	delete(s.transitions, string(tr.ID))
	return nil
}

func (s *memoryDispatchStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// outcome is everything a caller observes from one routing.
type outcome struct {
	Proposal []byte
	Err      string
}

func (w *world) route(ctx context.Context, text string) outcome {
	p, err := w.service.Route(ctx, text)
	o := outcome{}
	if err != nil {
		o.Err = err.Error()
		return o
	}
	b, merr := json.Marshal(p)
	if merr != nil {
		w.t.Fatal(merr)
	}
	o.Proposal = b
	return o
}

func decodeProposal(t testing.TB, o outcome) router.Proposal {
	t.Helper()
	if o.Err != "" {
		t.Fatalf("route failed: %s", o.Err)
	}
	var p router.Proposal
	if err := json.Unmarshal(o.Proposal, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

var _ = errors.New
var _ = reflect.DeepEqual
