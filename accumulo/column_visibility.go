package accumulo

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrVisibilityParse reports a malformed column-visibility expression.
var ErrVisibilityParse = errors.New("accumulo: malformed visibility expression")

// VisibilityParseError describes where a visibility expression failed to
// parse. Terms carries the expression the parser was reading and Offset the
// byte position it stopped at, which is the field-level detail Sharkbite's
// VisibilityParseException documents but keeps private.
type VisibilityParseError struct {
	Reason string
	Terms  []byte
	Offset int
}

// Error renders the reason, the offset and the expression.
func (e *VisibilityParseError) Error() string {
	return fmt.Sprintf(
		"accumulo: malformed visibility expression at offset %d: %s (%q)",
		e.Offset,
		e.Reason,
		e.Terms,
	)
}

// Unwrap reports ErrVisibilityParse so errors.Is matches the sentinel.
func (e *VisibilityParseError) Unwrap() error { return ErrVisibilityParse }

func visibilityError(reason string, expression []byte, offset int) error {
	return &VisibilityParseError{
		Reason: reason,
		Terms:  cloneRow(expression),
		Offset: offset,
	}
}

// VisibilityNodeType classifies a node of a parsed visibility expression. The
// values match Sharkbite's NodeType so a mapping layer needs no translation
// table.
type VisibilityNodeType int

// Visibility node types, in Sharkbite's declaration order.
const (
	VisibilityEmpty VisibilityNodeType = iota
	VisibilityTerm
	VisibilityOr
	VisibilityAnd
)

// String names the node type.
func (t VisibilityNodeType) String() string {
	switch t {
	case VisibilityEmpty:
		return "EMPTY"
	case VisibilityTerm:
		return "TERM"
	case VisibilityOr:
		return "OR"
	case VisibilityAnd:
		return "AND"
	default:
		return fmt.Sprintf("VisibilityNodeType(%d)", int(t))
	}
}

// NodeExpression is one term of a visibility expression, with the surrounding
// quotes removed and escapes resolved.
type NodeExpression struct {
	term []byte
}

// NewNodeExpression builds a term from the slice of expression that starts at
// offset and runs for size bytes.
func NewNodeExpression(expression []byte, offset, size int) (NodeExpression, error) {
	// Check offset first and then size against what is left, so a span whose
	// sum overflows cannot pass the bounds check.
	if offset < 0 || offset > len(expression) || size < 0 || size > len(expression)-offset {
		return NodeExpression{}, visibilityError("term is outside the expression", expression, offset)
	}
	return NodeExpression{term: cloneRow(expression[offset : offset+size])}, nil
}

// Term returns a copy of the term text.
func (e NodeExpression) Term() []byte { return cloneRow(e.term) }

// Buffer returns a copy of the term bytes. It is the counterpart of
// Sharkbite's getBuffer, which hands out the term's internal storage.
func (e NodeExpression) Buffer() []byte { return cloneRow(e.term) }

// Size returns the term length in bytes.
func (e NodeExpression) Size() int { return len(e.term) }

// String renders the term.
func (e NodeExpression) String() string { return string(e.term) }

// VisibilityNode is one node of a parsed visibility expression. Nodes are
// immutable values produced by NewColumnVisibility: reading one never mutates
// the tree it came from, and a node keeps its own copy of the expression, so
// it stays valid after the ColumnVisibility that produced it is discarded.
type VisibilityNode struct {
	expression []byte
	nodeType   VisibilityNodeType
	start      int
	end        int
	children   []VisibilityNode
}

// Type returns the node type.
func (n VisibilityNode) Type() VisibilityNodeType { return n.nodeType }

// Expression returns a copy of the expression the node was parsed from.
func (n VisibilityNode) Expression() []byte { return cloneRow(n.expression) }

// Children returns a copy of the node's children.
func (n VisibilityNode) Children() []VisibilityNode {
	if len(n.children) == 0 {
		return nil
	}
	children := make([]VisibilityNode, len(n.children))
	copy(children, n.children)
	return children
}

// Empty reports whether the node carries no expression, which is how an
// unpopulated node is recognised.
func (n VisibilityNode) Empty() bool { return n.expression == nil }

// Size returns the number of children.
func (n VisibilityNode) Size() int { return len(n.children) }

// Len returns the length in bytes of the span the node covers.
func (n VisibilityNode) Len() int {
	if n.end < n.start {
		return 0
	}
	return n.end - n.start
}

// TermStart returns the node's first byte offset in the expression.
func (n VisibilityNode) TermStart() int { return n.start }

// TermEnd returns the offset one past the node's last byte.
func (n VisibilityNode) TermEnd() int { return n.end }

// Term returns the node's term read out of expression, with surrounding quotes
// removed and escapes resolved. It reports an error for non-term nodes.
func (n VisibilityNode) Term(expression []byte) (NodeExpression, error) {
	if n.nodeType != VisibilityTerm {
		return NodeExpression{}, fmt.Errorf(
			"%w: %s node has no term",
			ErrVisibilityParse,
			n.nodeType,
		)
	}
	if n.start < 0 || n.end > len(expression) || n.end < n.start {
		return NodeExpression{}, visibilityError(
			"term is outside the expression",
			expression,
			n.start,
		)
	}
	span := expression[n.start:n.end]
	if len(span) >= 2 && span[0] == '"' && span[len(span)-1] == '"' {
		unquoted, err := unescapeVisibilityTerm(span[1:len(span)-1], expression, n.start)
		if err != nil {
			return NodeExpression{}, err
		}
		return NodeExpression{term: unquoted}, nil
	}
	return NodeExpression{term: cloneRow(span)}, nil
}

// String renders the node's span of its own expression.
func (n VisibilityNode) String() string {
	if n.Empty() {
		return ""
	}
	if n.start < 0 || n.end > len(n.expression) || n.end < n.start {
		return ""
	}
	return string(n.expression[n.start:n.end])
}

func unescapeVisibilityTerm(quoted, expression []byte, offset int) ([]byte, error) {
	out := make([]byte, 0, len(quoted))
	for index := 0; index < len(quoted); index++ {
		if quoted[index] != '\\' {
			out = append(out, quoted[index])
			continue
		}
		index++
		if index >= len(quoted) || (quoted[index] != '\\' && quoted[index] != '"') {
			return nil, visibilityError("invalid escaping within quotes", expression, offset)
		}
		out = append(out, quoted[index])
	}
	return out, nil
}

// CompareVisibilityNodes orders two nodes the way normalization does: by node
// type, then by term bytes for terms, then by child count and child order for
// AND and OR nodes. It returns a negative number, zero, or a positive number.
func CompareVisibilityNodes(left, right VisibilityNode) int {
	if diff := int(left.nodeType) - int(right.nodeType); diff != 0 {
		return diff
	}
	switch left.nodeType {
	case VisibilityEmpty:
		return 0
	case VisibilityTerm:
		return bytes.Compare(left.termBytes(), right.termBytes())
	default:
		if diff := len(left.children) - len(right.children); diff != 0 {
			return diff
		}
		for index := range left.children {
			if diff := CompareVisibilityNodes(
				left.children[index],
				right.children[index],
			); diff != 0 {
				return diff
			}
		}
		return 0
	}
}

// termBytes returns the node's raw span, which is what ordering compares.
func (n VisibilityNode) termBytes() []byte {
	if n.start < 0 || n.end > len(n.expression) || n.end < n.start {
		return nil
	}
	return n.expression[n.start:n.end]
}

// ColumnVisibility is a parsed column-visibility expression.
type ColumnVisibility struct {
	expression []byte
	root       VisibilityNode
}

// NewColumnVisibility parses expression. An empty expression parses to an
// empty tree, which every set of authorizations satisfies. Parse failures are
// *VisibilityParseError and match ErrVisibilityParse.
func NewColumnVisibility(expression []byte) (*ColumnVisibility, error) {
	owned := cloneRow(expression)
	if len(owned) == 0 {
		return &ColumnVisibility{expression: owned}, nil
	}
	parser := &visibilityParser{expression: owned}
	root, err := parser.parse(0)
	if err != nil {
		return nil, err
	}
	if parser.index != len(owned) {
		return nil, visibilityError(
			"unbalanced closing parenthesis",
			owned,
			parser.index,
		)
	}
	return &ColumnVisibility{expression: owned, root: root}, nil
}

// Expression returns a copy of the parsed expression.
func (v *ColumnVisibility) Expression() []byte { return cloneRow(v.expression) }

// Tree returns the parse tree. Reading it never mutates the ColumnVisibility.
func (v *ColumnVisibility) Tree() VisibilityNode { return v.root }

// Normalized returns the normalized parse tree: same-type children are rolled
// up into their parent, duplicates are removed, and children are ordered by
// CompareVisibilityNodes. The receiver is left untouched.
func (v *ColumnVisibility) Normalized() VisibilityNode {
	return normalizeVisibilityNode(v.root)
}

// Flatten renders the normalized expression. Parentheses are emitted only
// where a child's type differs from its parent's, which is the pinned
// Sharkbite rule.
func (v *ColumnVisibility) Flatten() []byte {
	if v.root.Empty() {
		return nil
	}
	var out bytes.Buffer
	flattenVisibilityNode(normalizeVisibilityNode(v.root), &out)
	return out.Bytes()
}

func flattenVisibilityNode(node VisibilityNode, out *bytes.Buffer) {
	if node.Type() == VisibilityTerm {
		out.Write(node.termBytes())
		return
	}
	separator := ""
	for _, child := range node.children {
		out.WriteString(separator)
		parenthesize := child.Type() != VisibilityTerm && child.Type() != node.Type()
		if parenthesize {
			out.WriteByte('(')
		}
		flattenVisibilityNode(child, out)
		if parenthesize {
			out.WriteByte(')')
		}
		if node.Type() == VisibilityAnd {
			separator = "&"
		} else {
			separator = "|"
		}
	}
}

// normalizeVisibilityNode rolls up same-type children, removes duplicates and
// orders what is left. A node left with one child collapses to that child.
func normalizeVisibilityNode(node VisibilityNode) VisibilityNode {
	if node.Type() == VisibilityTerm || node.Empty() {
		return node
	}
	var rolled []VisibilityNode
	for _, child := range node.children {
		normalized := normalizeVisibilityNode(child)
		if normalized.Type() == node.Type() {
			rolled = append(rolled, normalized.children...)
			continue
		}
		rolled = append(rolled, normalized)
	}
	sort.SliceStable(rolled, func(left, right int) bool {
		return CompareVisibilityNodes(rolled[left], rolled[right]) < 0
	})
	deduped := rolled[:0]
	for index, child := range rolled {
		if index > 0 && CompareVisibilityNodes(deduped[len(deduped)-1], child) == 0 {
			continue
		}
		deduped = append(deduped, child)
	}
	if len(deduped) == 1 {
		return deduped[0]
	}
	normalized := node
	normalized.children = deduped
	return normalized
}

// visibilityParser implements the pinned grammar: terms joined by & or |,
// which may not be mixed at one level, parenthesised sub-expressions, and
// double-quoted terms with \\ and \" escapes.
type visibilityParser struct {
	expression []byte
	index      int
}

// validVisibilityTermByte reports whether a byte may appear in an unquoted
// term. Accumulo's own visibility grammar admits '.' and '/' on top of the set
// Sharkbite's Authorizations::isValidAuthCharacter allows, and rejecting them
// would make expressions Accumulo itself writes - "team.alpha", "org/admin" -
// unreadable. ValidAuthorizationCharacter stays pinned to the Sharkbite rule.
func validVisibilityTermByte(character byte) bool {
	return ValidAuthorizationCharacter(character) || character == '.' || character == '/'
}

func (p *visibilityParser) parse(depth int) (VisibilityNode, error) {
	var result VisibilityNode
	var current VisibilityNode
	wholeTermStart := p.index
	subtermStart := p.index
	subtermComplete := false

	for p.index < len(p.expression) {
		character := p.expression[p.index]
		p.index++
		switch character {
		case '&', '|':
			nodeType := VisibilityAnd
			if character == '|' {
				nodeType = VisibilityOr
			}
			term, err := p.processTerm(subtermStart, p.index-1, current)
			if err != nil {
				return VisibilityNode{}, err
			}
			current = term
			if !result.Empty() {
				if result.Type() != nodeType {
					return VisibilityNode{}, visibilityError(
						"cannot mix & and |",
						p.expression,
						p.index-1,
					)
				}
			} else {
				result = VisibilityNode{
					expression: p.expression,
					nodeType:   nodeType,
					start:      wholeTermStart,
					end:        wholeTermStart + 1,
				}
			}
			result.children = append(result.children, current)
			current = VisibilityNode{}
			subtermStart = p.index
			subtermComplete = false
		case '(':
			if subtermStart != p.index-1 || !current.Empty() {
				return VisibilityNode{}, visibilityError(
					"expression needs & or |",
					p.expression,
					p.index-1,
				)
			}
			inner, err := p.parse(depth + 1)
			if err != nil {
				return VisibilityNode{}, err
			}
			current = inner
			subtermStart = p.index
			subtermComplete = false
		case ')':
			if depth == 0 {
				return VisibilityNode{}, visibilityError(
					"unbalanced closing parenthesis",
					p.expression,
					p.index-1,
				)
			}
			child, err := p.processTerm(subtermStart, p.index-1, current)
			if err != nil {
				return VisibilityNode{}, err
			}
			if result.Empty() {
				return child, nil
			}
			if result.Type() == child.Type() {
				result.children = append(result.children, child.children...)
			} else {
				result.children = append(result.children, child)
			}
			result.end = p.index - 1
			if len(result.children) < 2 {
				return VisibilityNode{}, visibilityError("missing term", p.expression, p.index-1)
			}
			return result, nil
		case '"':
			if subtermStart != p.index-1 {
				return VisibilityNode{}, visibilityError(
					"expression needs & or |",
					p.expression,
					p.index-1,
				)
			}
			if err := p.consumeQuotedTerm(subtermStart); err != nil {
				return VisibilityNode{}, err
			}
			subtermComplete = true
		default:
			if subtermComplete {
				return VisibilityNode{}, visibilityError(
					"expression needs & or |",
					p.expression,
					p.index-1,
				)
			}
			if !validVisibilityTermByte(character) {
				return VisibilityNode{}, visibilityError(
					fmt.Sprintf("bad character (%s)", string(character)),
					p.expression,
					p.index-1,
				)
			}
		}
	}
	if depth > 0 {
		return VisibilityNode{}, visibilityError(
			"unclosed parenthesis",
			p.expression,
			p.index,
		)
	}

	child, err := p.processTerm(subtermStart, p.index, current)
	if err != nil {
		return VisibilityNode{}, err
	}
	if result.Empty() {
		result = child
	} else {
		result.children = append(result.children, child)
		result.end = p.index
	}
	if result.Type() != VisibilityTerm && len(result.children) < 2 {
		return VisibilityNode{}, visibilityError("missing term", p.expression, p.index)
	}
	return result, nil
}

// processTerm turns the bytes between start and end into a term node, or keeps
// the sub-expression already parsed for that slot.
func (p *visibilityParser) processTerm(
	start, end int,
	current VisibilityNode,
) (VisibilityNode, error) {
	if start != end {
		if !current.Empty() {
			return VisibilityNode{}, visibilityError(
				"expression needs | or &",
				p.expression,
				start,
			)
		}
		return VisibilityNode{
			expression: p.expression,
			nodeType:   VisibilityTerm,
			start:      start,
			end:        end,
		}, nil
	}
	if current.Empty() {
		return VisibilityNode{}, visibilityError("empty term", p.expression, start)
	}
	return current, nil
}

// consumeQuotedTerm advances past a double-quoted term, validating escapes.
func (p *visibilityParser) consumeQuotedTerm(start int) error {
	for p.index < len(p.expression) && p.expression[p.index] != '"' {
		if p.expression[p.index] == '\\' {
			p.index++
			if p.index == len(p.expression) ||
				(p.expression[p.index] != '\\' && p.expression[p.index] != '"') {
				return visibilityError("invalid escaping within quotes", p.expression, p.index)
			}
		}
		p.index++
	}
	if p.index == len(p.expression) {
		return visibilityError("unclosed quote", p.expression, p.index)
	}
	if start+1 == p.index {
		return visibilityError("empty term", p.expression, start)
	}
	p.index++
	return nil
}

// visibilityCacheCapacity bounds the number of decisions one evaluator keeps.
// Cell visibilities repeat heavily within a scan, so a small cache pays for
// itself, but a high-cardinality workload must not grow it without bound.
const visibilityCacheCapacity = 1024

// visibilityCache is a bounded least-recently-used map of decisions.
type visibilityCache struct {
	capacity int
	entries  map[string]*list.Element
	order    *list.List
}

type visibilityCacheEntry struct {
	key   string
	value bool
}

func newVisibilityCache(capacity int) *visibilityCache {
	return &visibilityCache{
		capacity: capacity,
		entries:  make(map[string]*list.Element),
		order:    list.New(),
	}
}

// get returns a cached decision and marks it most recently used.
func (c *visibilityCache) get(key string) (bool, bool) {
	element, ok := c.entries[key]
	if !ok {
		return false, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*visibilityCacheEntry).value, true
}

// put stores a decision, evicting the least recently used one when full.
func (c *visibilityCache) put(key string, value bool) {
	if element, ok := c.entries[key]; ok {
		element.Value.(*visibilityCacheEntry).value = value
		c.order.MoveToFront(element)
		return
	}
	for c.order.Len() >= c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*visibilityCacheEntry).key)
	}
	c.entries[key] = c.order.PushFront(&visibilityCacheEntry{key: key, value: value})
}

func (c *visibilityCache) len() int { return c.order.Len() }

// VisibilityEvaluator decides whether a set of authorizations satisfies a
// visibility expression. It is safe for concurrent use, replacing its
// authorizations discards every cached decision, and the cache is bounded.
type VisibilityEvaluator struct {
	mu    sync.Mutex
	auths *Authorizations
	// generation changes with every authorization replacement so a decision
	// computed against an older set is never written into the newer cache.
	generation uint64
	cache      *visibilityCache
}

// NewVisibilityEvaluator builds an evaluator for auths. A nil or empty set
// satisfies only expressions that impose no requirement.
func NewVisibilityEvaluator(auths *Authorizations) *VisibilityEvaluator {
	evaluator := &VisibilityEvaluator{cache: newVisibilityCache(visibilityCacheCapacity)}
	evaluator.SetAuthorizations(auths)
	return evaluator
}

// Authorizations returns a copy of the authorizations in force.
func (e *VisibilityEvaluator) Authorizations() *Authorizations {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.auths == nil {
		return NewAuthorizations()
	}
	return e.auths.Clone()
}

// SetAuthorizations replaces the authorizations and clears the decision cache,
// so a reused evaluator never answers with a previous principal's set.
func (e *VisibilityEvaluator) SetAuthorizations(auths *Authorizations) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if auths == nil {
		e.auths = NewAuthorizations()
	} else {
		e.auths = auths.Clone()
	}
	e.generation++
	e.cache = newVisibilityCache(visibilityCacheCapacity)
}

// Evaluate reports whether the authorizations satisfy expression. An empty
// expression imposes no requirement and is always satisfied.
func (e *VisibilityEvaluator) Evaluate(expression []byte) (bool, error) {
	if len(expression) == 0 {
		return true, nil
	}
	e.mu.Lock()
	cached, ok := e.cache.get(string(expression))
	auths, generation := e.auths, e.generation
	e.mu.Unlock()
	if ok {
		return cached, nil
	}
	visibility, err := NewColumnVisibility(expression)
	if err != nil {
		return false, err
	}
	result, err := evaluateVisibilityNode(expression, visibility.Tree(), auths)
	if err != nil {
		return false, err
	}
	e.mu.Lock()
	if e.generation == generation {
		e.cache.put(string(expression), result)
	}
	e.mu.Unlock()
	return result, nil
}

// EvaluateTree reports whether the authorizations satisfy an already parsed
// tree read out of expression. The tree must be the one this expression
// produced: a mismatch is rejected rather than decided, because a tree paired
// with the wrong expression reads its terms out of the wrong bytes.
func (e *VisibilityEvaluator) EvaluateTree(
	expression []byte,
	root VisibilityNode,
) (bool, error) {
	if !bytes.Equal(expression, root.expression) {
		return false, visibilityError(
			"tree was parsed from a different expression",
			expression,
			0,
		)
	}
	if len(expression) == 0 {
		return true, nil
	}
	e.mu.Lock()
	auths := e.auths
	e.mu.Unlock()
	return evaluateVisibilityNode(expression, root, auths)
}

func evaluateVisibilityNode(
	expression []byte,
	root VisibilityNode,
	auths *Authorizations,
) (bool, error) {
	switch root.Type() {
	case VisibilityTerm:
		term, err := root.Term(expression)
		if err != nil {
			return false, err
		}
		return auths.Contains(term.term), nil
	case VisibilityAnd, VisibilityOr:
		if root.Size() < 2 {
			return false, visibilityError(
				fmt.Sprintf("%s has less than 2 children", root.Type()),
				expression,
				root.TermStart(),
			)
		}
		for _, child := range root.children {
			satisfied, err := evaluateVisibilityNode(expression, child, auths)
			if err != nil {
				return false, err
			}
			if satisfied == (root.Type() == VisibilityOr) {
				return satisfied, nil
			}
		}
		return root.Type() == VisibilityAnd, nil
	default:
		return false, visibilityError(
			fmt.Sprintf("no such node type %s", root.Type()),
			expression,
			root.TermStart(),
		)
	}
}
