package accumulo

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func mustVisibility(t *testing.T, expression string) *ColumnVisibility {
	t.Helper()
	visibility, err := NewColumnVisibility([]byte(expression))
	if err != nil {
		t.Fatalf("NewColumnVisibility(%q) = %v", expression, err)
	}
	return visibility
}

func TestColumnVisibilityParsesThePinnedGrammar(t *testing.T) {
	cases := []struct {
		expression string
		nodeType   VisibilityNodeType
		children   int
	}{
		{"a", VisibilityTerm, 0},
		{`"a b"`, VisibilityTerm, 0},
		{"a&b", VisibilityAnd, 2},
		{"a&b&c", VisibilityAnd, 3},
		{"a|b", VisibilityOr, 2},
		{"(a|b)&c", VisibilityAnd, 2},
		{"a&(b|c)", VisibilityAnd, 2},
		{"(a)", VisibilityTerm, 0},
		{"((a|b))", VisibilityOr, 2},
	}
	for _, tc := range cases {
		visibility := mustVisibility(t, tc.expression)
		root := visibility.Tree()
		if root.Type() != tc.nodeType || root.Size() != tc.children {
			t.Fatalf(
				"%q parsed to %s with %d children, want %s with %d",
				tc.expression,
				root.Type(),
				root.Size(),
				tc.nodeType,
				tc.children,
			)
		}
		if string(visibility.Expression()) != tc.expression {
			t.Fatalf("Expression() = %q", visibility.Expression())
		}
	}
}

func TestColumnVisibilityRejectsMalformedExpressions(t *testing.T) {
	cases := []struct {
		expression string
		reason     string
		offset     int
	}{
		{"a&b|c", "cannot mix & and |", 3},
		{"a&", "empty term", 2},
		{"&a", "empty term", 0},
		{"a(b)", "expression needs & or |", 1},
		{"(a", "unclosed parenthesis", 2},
		{"a)", "unbalanced closing parenthesis", 1},
		{"a&b)&c", "unbalanced closing parenthesis", 3},
		{"a|b)&c", "unbalanced closing parenthesis", 3},
		{"()", "empty term", 1},
		{`"a`, "unclosed quote", 2},
		{`""`, "empty term", 0},
		{`"a"b`, "expression needs & or |", 3},
		{`"a\z"`, "invalid escaping within quotes", 3},
		{"a b", "bad character ( )", 1},
		{"a,b", "bad character (,)", 1},
	}
	for _, tc := range cases {
		_, err := NewColumnVisibility([]byte(tc.expression))
		if err == nil {
			t.Fatalf("%q parsed without error", tc.expression)
		}
		if !errors.Is(err, ErrVisibilityParse) {
			t.Fatalf("%q error %v does not match ErrVisibilityParse", tc.expression, err)
		}
		var parseErr *VisibilityParseError
		if !errors.As(err, &parseErr) {
			t.Fatalf("%q error %v is not a *VisibilityParseError", tc.expression, err)
		}
		if parseErr.Reason != tc.reason {
			t.Fatalf("%q reason = %q, want %q", tc.expression, parseErr.Reason, tc.reason)
		}
		if parseErr.Offset != tc.offset {
			t.Fatalf("%q offset = %d, want %d", tc.expression, parseErr.Offset, tc.offset)
		}
		if string(parseErr.Terms) != tc.expression {
			t.Fatalf("%q terms = %q", tc.expression, parseErr.Terms)
		}
		if !strings.Contains(err.Error(), tc.reason) {
			t.Fatalf("%q message = %q", tc.expression, err.Error())
		}
	}
}

func TestColumnVisibilityRejectsAnUnbalancedCloseInsteadOfTruncating(t *testing.T) {
	// The pinned parser returns early on a stray ')', so "a&b)&c" silently
	// becomes "a&b" - an expression more permissive than the one written.
	for _, expression := range []string{"a&b)&c", "a|b)&c"} {
		if _, err := NewColumnVisibility([]byte(expression)); !errors.Is(err, ErrVisibilityParse) {
			t.Fatalf("NewColumnVisibility(%q) = %v", expression, err)
		}
		evaluator := NewVisibilityEvaluator(NewAuthorizationStrings("a", "b"))
		if _, err := evaluator.Evaluate([]byte(expression)); !errors.Is(err, ErrVisibilityParse) {
			t.Fatalf("Evaluate(%q) = %v", expression, err)
		}
	}
}

func TestVisibilityNodeAccessorsDescribeTheTree(t *testing.T) {
	visibility := mustVisibility(t, "a&bb")
	root := visibility.Tree()
	if root.Type() != VisibilityAnd || root.Empty() || root.Size() != 2 {
		t.Fatalf("root = %s empty=%v size=%d", root.Type(), root.Empty(), root.Size())
	}
	if string(root.Expression()) != "a&bb" {
		t.Fatalf("Expression() = %q", root.Expression())
	}
	children := root.Children()
	if len(children) != 2 {
		t.Fatalf("children = %d", len(children))
	}
	first, second := children[0], children[1]
	if first.TermStart() != 0 || first.TermEnd() != 1 || first.Len() != 1 {
		t.Fatalf("first term span = [%d,%d) len=%d", first.TermStart(), first.TermEnd(), first.Len())
	}
	if second.TermStart() != 2 || second.TermEnd() != 4 || second.Len() != 2 {
		t.Fatalf("second term span = [%d,%d) len=%d", second.TermStart(), second.TermEnd(), second.Len())
	}
	if first.String() != "a" || second.String() != "bb" {
		t.Fatalf("String() = %q, %q", first.String(), second.String())
	}
	term, err := second.Term([]byte("a&bb"))
	if err != nil {
		t.Fatal(err)
	}
	if string(term.Term()) != "bb" || term.Size() != 2 || string(term.Buffer()) != "bb" {
		t.Fatalf("term = %q size=%d buffer=%q", term.Term(), term.Size(), term.Buffer())
	}
	if term.String() != "bb" {
		t.Fatalf("term String() = %q", term.String())
	}
	if _, err := root.Term([]byte("a&bb")); err == nil {
		t.Fatal("Term on an AND node returned no error")
	}
	var empty VisibilityNode
	if !empty.Empty() || empty.String() != "" || empty.Len() != 0 || empty.Children() != nil {
		t.Fatal("zero node is not empty")
	}
}

func TestVisibilityNodeTermUnquotesAndUnescapes(t *testing.T) {
	cases := []struct {
		expression string
		want       string
	}{
		{`"a b"`, "a b"},
		{`"a\"b"`, `a"b`},
		{`"a\\b"`, `a\b`},
		{`a`, "a"},
	}
	for _, tc := range cases {
		visibility := mustVisibility(t, tc.expression)
		term, err := visibility.Tree().Term([]byte(tc.expression))
		if err != nil {
			t.Fatalf("%q: %v", tc.expression, err)
		}
		if string(term.Term()) != tc.want {
			t.Fatalf("%q term = %q, want %q", tc.expression, term.Term(), tc.want)
		}
	}
}

func TestNewNodeExpressionValidatesItsSpan(t *testing.T) {
	expression := []byte("abc")
	term, err := NewNodeExpression(expression, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(term.Term()) != "bc" {
		t.Fatalf("term = %q", term.Term())
	}
	expression[1] = 'X'
	if string(term.Term()) != "bc" {
		t.Fatalf("term followed the caller's slice: %q", term.Term())
	}
	for _, bad := range [][2]int{{-1, 1}, {0, 4}, {2, 2}, {4, 0}, {maxInt, maxInt}, {1, maxInt}} {
		if _, err := NewNodeExpression([]byte("abc"), bad[0], bad[1]); err == nil {
			t.Fatalf("NewNodeExpression%v accepted an out-of-range span", bad)
		}
	}
	if empty, err := NewNodeExpression([]byte("abc"), 3, 0); err != nil || empty.Size() != 0 {
		t.Fatalf("NewNodeExpression(3,0) = %v, %v", empty, err)
	}
}

const maxInt = int(^uint(0) >> 1)

func TestColumnVisibilityFlattenNormalizes(t *testing.T) {
	cases := []struct{ expression, want string }{
		{"a", "a"},
		{"a&b", "a&b"},
		{"b&a", "a&b"},
		{"a&a", "a"},
		{"a&(b&c)", "a&b&c"},
		{"(a|b)|c", "a|b|c"},
		{"(a|b)&c", "c&(a|b)"},
		{"(b|a)&(a|b)", "a|b"},
	}
	for _, tc := range cases {
		visibility := mustVisibility(t, tc.expression)
		if got := string(visibility.Flatten()); got != tc.want {
			t.Fatalf("Flatten(%q) = %q, want %q", tc.expression, got, tc.want)
		}
	}
}

func TestColumnVisibilityFlattenDoesNotMutateTheTree(t *testing.T) {
	visibility := mustVisibility(t, "a&(b&c)")
	before := visibility.Tree()
	first := string(visibility.Flatten())
	second := string(visibility.Flatten())
	if first != second {
		t.Fatalf("Flatten is not idempotent: %q then %q", first, second)
	}
	after := visibility.Tree()
	if after.Size() != before.Size() || after.Type() != before.Type() {
		t.Fatalf("Tree changed: %s/%d -> %s/%d", before.Type(), before.Size(), after.Type(), after.Size())
	}
	if before.Size() != 2 {
		t.Fatalf("parse tree was already rolled up: size=%d", before.Size())
	}
	normalized := visibility.Normalized()
	if normalized.Size() != 3 {
		t.Fatalf("Normalized() size = %d, want 3", normalized.Size())
	}
	if visibility.Tree().Size() != 2 {
		t.Fatal("Normalized() mutated the parse tree")
	}
}

func TestCompareVisibilityNodesOrdersByTypeThenTerm(t *testing.T) {
	term := mustVisibility(t, "a").Tree()
	other := mustVisibility(t, "b").Tree()
	or := mustVisibility(t, "a|b").Tree()
	and := mustVisibility(t, "a&b").Tree()
	longer := mustVisibility(t, "a&b&c").Tree()

	if CompareVisibilityNodes(term, term) != 0 {
		t.Fatal("a != a")
	}
	if CompareVisibilityNodes(term, other) >= 0 {
		t.Fatal("a is not before b")
	}
	if CompareVisibilityNodes(term, or) >= 0 || CompareVisibilityNodes(or, and) >= 0 {
		t.Fatal("type ordering is wrong")
	}
	if CompareVisibilityNodes(and, longer) >= 0 {
		t.Fatal("child count ordering is wrong")
	}
	if CompareVisibilityNodes(and, mustVisibility(t, "a&c").Tree()) >= 0 {
		t.Fatal("child ordering is wrong")
	}
	var empty VisibilityNode
	if CompareVisibilityNodes(empty, empty) != 0 {
		t.Fatal("empty nodes are not equal")
	}
}

func TestVisibilityEvaluatorEvaluatesExpressions(t *testing.T) {
	evaluator := NewVisibilityEvaluator(NewAuthorizationStrings("a", "b"))
	cases := []struct {
		expression string
		want       bool
	}{
		{"", true},
		{"a", true},
		{"c", false},
		{"a&b", true},
		{"a&c", false},
		{"a|c", true},
		{"c|d", false},
		{"(a|c)&b", true},
		{"(c|d)&b", false},
		{"a&b&c", false},
	}
	for _, tc := range cases {
		got, err := evaluator.Evaluate([]byte(tc.expression))
		if err != nil {
			t.Fatalf("Evaluate(%q) = %v", tc.expression, err)
		}
		if got != tc.want {
			t.Fatalf("Evaluate(%q) = %v, want %v", tc.expression, got, tc.want)
		}
	}
}

func TestVisibilityEvaluatorTreatsAnEmptyExpressionAsUnrestricted(t *testing.T) {
	// The pinned evaluator returns false for an empty expression when the
	// authorizations are non-empty, and throws when they are empty.
	for _, auths := range []*Authorizations{
		NewAuthorizations(),
		NewAuthorizationStrings("a"),
		nil,
	} {
		evaluator := NewVisibilityEvaluator(auths)
		visible, err := evaluator.Evaluate(nil)
		if err != nil || !visible {
			t.Fatalf("Evaluate(nil) = %v, %v", visible, err)
		}
		visible, err = evaluator.Evaluate([]byte{})
		if err != nil || !visible {
			t.Fatalf("Evaluate(empty) = %v, %v", visible, err)
		}
	}
}

func TestVisibilityEvaluatorMatchesQuotedTerms(t *testing.T) {
	evaluator := NewVisibilityEvaluator(NewAuthorizationStrings("a b", `c"d`))
	for _, tc := range []struct {
		expression string
		want       bool
	}{
		{`"a b"`, true},
		{`"c\"d"`, true},
		{`"e f"`, false},
	} {
		got, err := evaluator.Evaluate([]byte(tc.expression))
		if err != nil {
			t.Fatalf("Evaluate(%q) = %v", tc.expression, err)
		}
		if got != tc.want {
			t.Fatalf("Evaluate(%q) = %v, want %v", tc.expression, got, tc.want)
		}
	}
}

func TestVisibilityEvaluatorDropsCachedDecisionsWhenAuthorizationsChange(t *testing.T) {
	evaluator := NewVisibilityEvaluator(NewAuthorizationStrings("a"))
	visible, err := evaluator.Evaluate([]byte("a"))
	if err != nil || !visible {
		t.Fatalf("first Evaluate = %v, %v", visible, err)
	}
	evaluator.SetAuthorizations(NewAuthorizationStrings("b"))
	visible, err = evaluator.Evaluate([]byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	if visible {
		t.Fatal("evaluator answered with the previous principal's authorizations")
	}
	if got := evaluator.Authorizations().Strings(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("Authorizations() = %v", got)
	}
	evaluator.SetAuthorizations(nil)
	if !evaluator.Authorizations().Empty() {
		t.Fatal("nil authorizations did not clear the set")
	}
	visible, err = evaluator.Evaluate([]byte("a"))
	if err != nil || visible {
		t.Fatalf("Evaluate after clearing = %v, %v", visible, err)
	}
}

func TestColumnVisibilityAcceptsAccumulosTermCharacters(t *testing.T) {
	// The pinned parser only allows the Sharkbite authorization character set,
	// so expressions Accumulo itself writes are rejected as malformed.
	for _, expression := range []string{"team.alpha", "org/admin", "team.alpha&org/admin"} {
		if _, err := NewColumnVisibility([]byte(expression)); err != nil {
			t.Fatalf("NewColumnVisibility(%q) = %v", expression, err)
		}
	}
	evaluator := NewVisibilityEvaluator(NewAuthorizationStrings("team.alpha", "org/admin"))
	visible, err := evaluator.Evaluate([]byte("team.alpha&org/admin"))
	if err != nil || !visible {
		t.Fatalf("Evaluate = %v, %v", visible, err)
	}
	if ValidAuthorizationCharacter('.') || ValidAuthorizationCharacter('/') {
		t.Fatal("ValidAuthorizationCharacter drifted from the pinned Sharkbite set")
	}
}

func TestVisibilityEvaluatorDoesNotCacheAcrossAnAuthorizationChange(t *testing.T) {
	evaluator := NewVisibilityEvaluator(NewAuthorizationStrings("a"))
	// Snapshot the old authorizations, replace them, then let the in-flight
	// evaluation finish: its result must not land in the new cache.
	evaluator.mu.Lock()
	auths, generation := evaluator.auths, evaluator.generation
	evaluator.mu.Unlock()

	evaluator.SetAuthorizations(NewAuthorizationStrings("b"))

	stale, err := evaluateVisibilityNode(
		[]byte("a"),
		mustVisibility(t, "a").Tree(),
		auths,
	)
	if err != nil || !stale {
		t.Fatalf("stale evaluation = %v, %v", stale, err)
	}
	evaluator.mu.Lock()
	if evaluator.generation == generation {
		evaluator.mu.Unlock()
		t.Fatal("SetAuthorizations did not advance the generation")
	}
	evaluator.mu.Unlock()

	visible, err := evaluator.Evaluate([]byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	if visible {
		t.Fatal("evaluator served a decision made against the previous authorizations")
	}
}

func TestVisibilityEvaluatorCacheIsBounded(t *testing.T) {
	evaluator := NewVisibilityEvaluator(NewAuthorizationStrings("a"))
	for index := 0; index < visibilityCacheCapacity*2; index++ {
		expression := fmt.Sprintf("label%d", index)
		if _, err := evaluator.Evaluate([]byte(expression)); err != nil {
			t.Fatalf("Evaluate(%q) = %v", expression, err)
		}
	}
	evaluator.mu.Lock()
	size := evaluator.cache.len()
	evaluator.mu.Unlock()
	if size != visibilityCacheCapacity {
		t.Fatalf("cache holds %d entries, want %d", size, visibilityCacheCapacity)
	}
	// The most recent expression is still cached and the oldest is not.
	evaluator.mu.Lock()
	_, newest := evaluator.cache.get(fmt.Sprintf("label%d", visibilityCacheCapacity*2-1))
	_, oldest := evaluator.cache.get("label0")
	evaluator.mu.Unlock()
	if !newest {
		t.Fatal("the most recent decision was evicted")
	}
	if oldest {
		t.Fatal("the least recently used decision survived")
	}
}

func TestVisibilityCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := newVisibilityCache(2)
	cache.put("a", true)
	cache.put("b", false)
	if value, ok := cache.get("a"); !ok || !value {
		t.Fatalf("get(a) = %v, %v", value, ok)
	}
	cache.put("c", true)
	if _, ok := cache.get("b"); ok {
		t.Fatal("b was not evicted")
	}
	if value, ok := cache.get("a"); !ok || !value {
		t.Fatal("a was evicted despite being used most recently")
	}
	cache.put("a", false)
	if value, ok := cache.get("a"); !ok || value {
		t.Fatal("put did not overwrite an existing entry")
	}
	if cache.len() != 2 {
		t.Fatalf("cache holds %d entries", cache.len())
	}
}

func TestVisibilityEvaluatorAuthorizationsAreCopied(t *testing.T) {
	auths := NewAuthorizationStrings("a")
	evaluator := NewVisibilityEvaluator(auths)
	auths.Add([]byte("b"))
	visible, err := evaluator.Evaluate([]byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	if visible {
		t.Fatal("evaluator followed the caller's authorizations")
	}
	returned := evaluator.Authorizations()
	returned.Add([]byte("c"))
	if visible, err := evaluator.Evaluate([]byte("c")); err != nil || visible {
		t.Fatalf("Authorizations() handed out the live set: %v, %v", visible, err)
	}
}

func TestVisibilityEvaluatorReportsMalformedTrees(t *testing.T) {
	evaluator := NewVisibilityEvaluator(NewAuthorizationStrings("a"))
	incomplete := VisibilityNode{
		expression: []byte("a&b"),
		nodeType:   VisibilityAnd,
		start:      0,
		end:        3,
	}
	if _, err := evaluator.EvaluateTree([]byte("a&b"), incomplete); err == nil {
		t.Fatal("an AND node with no children evaluated")
	}
	var unknown VisibilityNode
	unknown.expression = []byte("a")
	unknown.nodeType = VisibilityEmpty
	if _, err := evaluator.EvaluateTree([]byte("a"), unknown); err == nil {
		t.Fatal("an empty node evaluated")
	}
	if _, err := evaluator.EvaluateTree([]byte("a"), VisibilityNode{
		expression: []byte("a"),
		nodeType:   VisibilityTerm,
		start:      0,
		end:        9,
	}); err == nil {
		t.Fatal("an out-of-range term evaluated")
	}
}

func TestVisibilityEvaluatorUsesTheParsedTree(t *testing.T) {
	visibility := mustVisibility(t, "a&b")
	evaluator := NewVisibilityEvaluator(NewAuthorizationStrings("a", "b"))
	visible, err := evaluator.EvaluateTree(visibility.Expression(), visibility.Tree())
	if err != nil || !visible {
		t.Fatalf("EvaluateTree = %v, %v", visible, err)
	}
	var emptyRoot VisibilityNode
	if visible, err := evaluator.EvaluateTree(nil, emptyRoot); err != nil || !visible {
		t.Fatalf("EvaluateTree with an empty expression and root = %v, %v", visible, err)
	}
}

func TestVisibilityEvaluatorRejectsATreeFromAnotherExpression(t *testing.T) {
	evaluator := NewVisibilityEvaluator(NewAuthorizations())
	secret := mustVisibility(t, "secret")
	for _, expression := range [][]byte{nil, []byte{}, []byte("secre"), []byte("public")} {
		visible, err := evaluator.EvaluateTree(expression, secret.Tree())
		if err == nil {
			t.Fatalf("EvaluateTree(%q, secret) = %v with no error", expression, visible)
		}
		if visible {
			t.Fatalf("EvaluateTree(%q, secret) = true", expression)
		}
		if !errors.Is(err, ErrVisibilityParse) {
			t.Fatalf("EvaluateTree(%q, secret) error = %v", expression, err)
		}
	}
	// A truncated expression must not let AND short-circuiting decide.
	both := mustVisibility(t, "a&b")
	if _, err := evaluator.EvaluateTree([]byte("a&"), both.Tree()); err == nil {
		t.Fatal("a truncated expression was accepted")
	}
}

func TestColumnVisibilityCopiesItsInput(t *testing.T) {
	expression := []byte("a&b")
	visibility := mustVisibility(t, string(expression))
	expression[0] = 'X'
	if string(visibility.Expression()) != "a&b" {
		t.Fatalf("Expression() = %q", visibility.Expression())
	}
	returned := visibility.Expression()
	returned[0] = 'Y'
	if string(visibility.Expression()) != "a&b" {
		t.Fatalf("Expression() handed out its own storage: %q", visibility.Expression())
	}
	children := visibility.Tree().Children()
	children[0] = VisibilityNode{}
	if visibility.Tree().Children()[0].Empty() {
		t.Fatal("Children() handed out its own storage")
	}
}

func TestVisibilityEvaluatorIsSafeForConcurrentUse(t *testing.T) {
	evaluator := NewVisibilityEvaluator(NewAuthorizationStrings("a", "b"))
	expressions := []string{"a", "b", "a&b", "a|c", "(a|c)&b"}
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for round := 0; round < 50; round++ {
				expression := expressions[(worker+round)%len(expressions)]
				visible, err := evaluator.Evaluate([]byte(expression))
				if err != nil {
					t.Errorf("Evaluate(%q) = %v", expression, err)
					return
				}
				if !visible {
					t.Errorf("Evaluate(%q) = false", expression)
					return
				}
			}
		}(worker)
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for round := 0; round < 20; round++ {
			evaluator.SetAuthorizations(NewAuthorizationStrings("a", "b"))
		}
	}()
	workers.Wait()
}

func TestVisibilityNodeTypeNames(t *testing.T) {
	cases := map[VisibilityNodeType]string{
		VisibilityEmpty:       "EMPTY",
		VisibilityTerm:        "TERM",
		VisibilityOr:          "OR",
		VisibilityAnd:         "AND",
		VisibilityNodeType(9): "VisibilityNodeType(9)",
	}
	for nodeType, want := range cases {
		if got := nodeType.String(); got != want {
			t.Fatalf("%d.String() = %q, want %q", int(nodeType), got, want)
		}
	}
}
