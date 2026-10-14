// Package visfilter evaluates Accumulo column-visibility expressions
// against an authorizations set, with a per-Evaluator cache so the
// expression for a given CV byte string is parsed exactly once.
//
// CV expression grammar (Accumulo ColumnVisibility):
//
//	expr   := and ('|' and)*
//	and    := atom ('&' atom)*
//	atom   := label | '(' expr ')'
//	label  := unquoted-bytes | quoted-string
//
// V0 supports unquoted labels — Accumulo's permitted byte set per
// `ColumnVisibility.QUOTABLE_CHARS`-not-set: `[A-Za-z0-9_:./-]+`.
// Quoted-string labels (which allow arbitrary bytes via escapes) are
// not yet supported; they're rare in production CV labels and trip
// the parser's strict mode loudly rather than silently accepting.
//
// Empty CV (`len(cv) == 0`) is always visible, matching Accumulo's
// rule that an unlabeled cell has no visibility restriction.
//
// Concurrency: an Evaluator is single-goroutine. Each scan thread on
// the read fleet creates its own Evaluator over its own authorizations
// — there's no shared cache across scans, which is the right call
// because authorizations differ per scan and the cache values
// (compiled expression trees) don't depend on auths anyway, but
// avoiding cross-scan contention keeps the hot path lock-free.
package visfilter

import (
	"errors"
	"fmt"
)

// Authorizations is the set of labels a scan is allowed to see. Construct
// once per scan; share with the Evaluator. Internally a map for O(1)
// label lookup.
type Authorizations struct {
	set map[string]struct{}
}

// NewAuthorizations builds an Authorizations set from a list of label
// byte strings. Each label is copied into the internal set's key.
func NewAuthorizations(labels ...[]byte) *Authorizations {
	a := &Authorizations{set: make(map[string]struct{}, len(labels))}
	for _, l := range labels {
		a.set[string(l)] = struct{}{}
	}
	return a
}

// AddLabel inserts a label into the set. Returns the receiver for
// fluent chains.
func (a *Authorizations) AddLabel(label []byte) *Authorizations {
	a.set[string(label)] = struct{}{}
	return a
}

// HasLabel reports whether label is in the set. Alloc-free under Go's
// `m[string(b)]` lookup optimization for byte-slice → string conversions.
func (a *Authorizations) HasLabel(label []byte) bool {
	_, ok := a.set[string(label)]
	return ok
}

// Evaluator evaluates CV expressions against a fixed Authorizations set.
// Per-CV-bytes cache keyed by the CV byte string — most cells in a
// tablet share a small handful of CV labels, so the cache hit rate is
// typically >99%.
//
// NOT goroutine-safe. One Evaluator per scan thread.
type Evaluator struct {
	auths *Authorizations
	cache map[string]*node
}

// NewEvaluator constructs an Evaluator over auths. Pre-allocates a
// modest cache; it grows as new CV strings are encountered.
func NewEvaluator(auths *Authorizations) *Evaluator {
	return &Evaluator{
		auths: auths,
		cache: make(map[string]*node, 16),
	}
}

// Visible reports whether a cell with the given CV bytes is visible
// under the configured authorizations. Empty CV is always visible.
//
// Hot path:
//  1. cache hit → walk the precompiled tree (no alloc).
//  2. cache miss → parse + compile, store in cache, evaluate.
//
// `cv` is treated as borrowed bytes — Evaluator copies into its cache
// key on miss, but does not retain the slice.
//
// For malformed CV expressions, returns false (fail-closed). The first
// time we see a malformed CV we cache the parse error so subsequent
// occurrences don't re-pay the parse cost.
func (e *Evaluator) Visible(cv []byte) bool {
	if len(cv) == 0 {
		return true
	}
	// Alloc-free map lookup via the []byte→string conversion optimization
	// (Go compiler recognizes m[string(b)] when m has string keys).
	if n, ok := e.cache[string(cv)]; ok {
		if n == nil {
			return false // cached parse failure
		}
		return evalNode(n, e.auths)
	}
	n, err := parse(cv)
	key := string(cv) // materialize the cache key (one alloc per unique CV)
	if err != nil {
		e.cache[key] = nil
		return false
	}
	e.cache[key] = n
	return evalNode(n, e.auths)
}

// CacheSize returns the number of distinct CV byte strings the
// Evaluator has parsed so far. Useful for diagnostics: a per-tablet
// scan typically has CacheSize ≤ a few dozen.
func (e *Evaluator) CacheSize() int { return len(e.cache) }

// node is the compiled form of one CV expression. nodeKind discriminates
// the union; only one of (label, children) is populated per node.
type node struct {
	kind     nodeKind
	label    []byte  // when kind == kindLabel; owned (copied at parse time)
	children []*node // when kind == kindAnd or kindOr
}

type nodeKind uint8

const (
	kindLabel nodeKind = iota
	kindAnd
	kindOr
)

// evalNode walks the tree, short-circuiting AND on first false and OR
// on first true. Recursion depth is bounded by parenthesis nesting in
// the source CV string — not a stack-overflow risk for any realistic
// CV expression (Accumulo's own enforcement caps these at modest depth).
func evalNode(n *node, auths *Authorizations) bool {
	switch n.kind {
	case kindLabel:
		return auths.HasLabel(n.label)
	case kindAnd:
		for _, c := range n.children {
			if !evalNode(c, auths) {
				return false
			}
		}
		return true
	case kindOr:
		for _, c := range n.children {
			if evalNode(c, auths) {
				return true
			}
		}
		return false
	}
	return false
}

// ErrParse is returned for malformed CV expressions. The Evaluator
// caches the failure so subsequent occurrences don't re-pay the cost.
var ErrParse = errors.New("visfilter: malformed visibility expression")

// parse turns a CV byte string into a compiled tree. Recursive descent
// matching ColumnVisibility's grammar. Mirror of Accumulo's
// `ColumnVisibility.parse`, simplified for V0 (no quoted strings yet).
func parse(cv []byte) (*node, error) {
	p := &parser{src: cv}
	n, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("%w: trailing bytes at position %d", ErrParse, p.pos)
	}
	return n, nil
}

type parser struct {
	src []byte
	pos int
}

// parseExpr parses an OR-list. Single-element OR-lists collapse to the
// single child to keep the eval tree minimal.
func (p *parser) parseExpr() (*node, error) {
	first, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	if p.pos >= len(p.src) || p.src[p.pos] != '|' {
		return first, nil
	}
	children := []*node{first}
	for p.pos < len(p.src) && p.src[p.pos] == '|' {
		p.pos++
		next, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		children = append(children, next)
	}
	return &node{kind: kindOr, children: children}, nil
}

// parseAnd parses an AND-list. Single-element AND-lists collapse like OR.
func (p *parser) parseAnd() (*node, error) {
	first, err := p.parseAtom()
	if err != nil {
		return nil, err
	}
	if p.pos >= len(p.src) || p.src[p.pos] != '&' {
		return first, nil
	}
	children := []*node{first}
	for p.pos < len(p.src) && p.src[p.pos] == '&' {
		p.pos++
		next, err := p.parseAtom()
		if err != nil {
			return nil, err
		}
		children = append(children, next)
	}
	return &node{kind: kindAnd, children: children}, nil
}

// parseAtom parses a single label or a parenthesized sub-expression.
func (p *parser) parseAtom() (*node, error) {
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("%w: unexpected end at position %d", ErrParse, p.pos)
	}
	if p.src[p.pos] == '(' {
		p.pos++
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.pos >= len(p.src) || p.src[p.pos] != ')' {
			return nil, fmt.Errorf("%w: missing close paren at position %d", ErrParse, p.pos)
		}
		p.pos++
		return inner, nil
	}
	// Unquoted label — consume per Accumulo's permitted byte set
	// (matches ColumnVisibility's default acceptable chars).
	start := p.pos
	for p.pos < len(p.src) && isLabelByte(p.src[p.pos]) {
		p.pos++
	}
	if p.pos == start {
		return nil, fmt.Errorf("%w: expected label at position %d (got %q)", ErrParse, start, p.src[start])
	}
	// Copy label bytes — the source slice may be transient (CV bytes
	// from a relkey decode point into the block buffer).
	label := make([]byte, p.pos-start)
	copy(label, p.src[start:p.pos])
	return &node{kind: kindLabel, label: label}, nil
}

// isLabelByte mirrors ColumnVisibility's default unquoted-label byte set.
func isLabelByte(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z':
		return true
	case b >= 'a' && b <= 'z':
		return true
	case b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '_', ':', '.', '/', '-':
		return true
	}
	return false
}
