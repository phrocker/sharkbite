package shoalql

import (
	"fmt"
	"strconv"
	"strings"
)

type parser struct {
	toks []token
	i    int
}

// Parse parses a single read-only SELECT or EXPLAIN SELECT statement.
func Parse(sql string) (*SelectStmt, error) {
	toks, err := lex(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		return nil, p.errf("unexpected trailing input %q", p.peek().text)
	}
	return stmt, nil
}

func (p *parser) parseStatement() (*SelectStmt, error) {
	explain := p.acceptKeyword("EXPLAIN")
	format := ExplainText
	if explain && p.acceptKeyword("FORMAT") {
		switch {
		case p.acceptKeyword("TEXT"):
			format = ExplainText
		case p.acceptKeyword("JSON"):
			format = ExplainJSON
		default:
			return nil, p.errf("expected TEXT or JSON after EXPLAIN FORMAT")
		}
	}
	stmt, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	stmt.Explain = explain
	if explain {
		stmt.ExplainFormat = format
	}
	return stmt, nil
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) advance() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) errf(format string, a ...any) error {
	return fmt.Errorf("shoalql: %s (near token %d)", fmt.Sprintf(format, a...), p.i)
}

func (p *parser) expectKeyword(kw string) error {
	t := p.peek()
	if t.kind != tKeyword || t.text != kw {
		return p.errf("expected %s, got %q", kw, t.text)
	}
	p.advance()
	return nil
}

func (p *parser) acceptKeyword(kw string) bool {
	t := p.peek()
	if t.kind == tKeyword && t.text == kw {
		p.advance()
		return true
	}
	return false
}

func (p *parser) parseSelect() (*SelectStmt, error) {
	if err := p.expectKeyword("SELECT"); err != nil {
		return nil, err
	}
	stmt := &SelectStmt{}

	// Projection list.
	if p.peek().kind == tStar {
		p.advance()
		stmt.Star = true
	} else {
		items, err := p.parseSelectList()
		if err != nil {
			return nil, err
		}
		stmt.Columns = items
	}

	// FROM <table>
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	tbl := p.peek()
	if tbl.kind != tIdent {
		return nil, p.errf("expected table name, got %q", tbl.text)
	}
	p.advance()
	stmt.Table = tbl.text

	// [AS OF <ts>]
	if p.acceptKeyword("AS") {
		if err := p.expectKeyword("OF"); err != nil {
			return nil, err
		}
		ts, err := p.parseTimestamp()
		if err != nil {
			return nil, err
		}
		stmt.AsOf = &ts
	}

	// [WHERE ...]
	if p.acceptKeyword("WHERE") {
		preds, err := p.parseWhere()
		if err != nil {
			return nil, err
		}
		stmt.Where = preds
	}

	// [GROUP BY <col>]
	if p.acceptKeyword("GROUP") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		col := p.peek()
		if col.kind != tIdent {
			return nil, p.errf("expected group-by column, got %q", col.text)
		}
		p.advance()
		stmt.GroupBy = col.text
	}

	// [ORDER BY <col> <-> <vec>]
	if p.acceptKeyword("ORDER") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		ob, err := p.parseOrderBy()
		if err != nil {
			return nil, err
		}
		stmt.Order = ob
	}

	// [LIMIT <n>]
	if p.acceptKeyword("LIMIT") {
		n := p.peek()
		if n.kind != tNumber {
			return nil, p.errf("expected LIMIT count, got %q", n.text)
		}
		p.advance()
		v, err := strconv.Atoi(n.text)
		if err != nil || v < 0 {
			return nil, p.errf("invalid LIMIT %q", n.text)
		}
		stmt.Limit = &v
	}

	return stmt, nil
}

func (p *parser) parseSelectList() ([]SelectItem, error) {
	var items []SelectItem
	for {
		item, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if p.peek().kind == tComma {
			p.advance()
			continue
		}
		break
	}
	return items, nil
}

func (p *parser) parseSelectItem() (SelectItem, error) {
	t := p.peek()

	// count(*)
	if t.kind == tKeyword && t.text == "COUNT" {
		p.advance()
		if p.peek().kind != tLParen {
			return SelectItem{}, p.errf("expected ( after COUNT")
		}
		p.advance()
		if p.peek().kind != tStar {
			return SelectItem{}, p.errf("only COUNT(*) is supported")
		}
		p.advance()
		if p.peek().kind != tRParen {
			return SelectItem{}, p.errf("expected ) after COUNT(*")
		}
		p.advance()
		return p.withAlias(SelectItem{CountStar: true})
	}

	// expand(col, 'edge')
	if t.kind == tKeyword && t.text == "EXPAND" {
		p.advance()
		if p.peek().kind != tLParen {
			return SelectItem{}, p.errf("expected ( after EXPAND")
		}
		p.advance()
		col := p.peek()
		if col.kind != tIdent {
			return SelectItem{}, p.errf("expected column in expand(), got %q", col.text)
		}
		p.advance()
		if p.peek().kind != tComma {
			return SelectItem{}, p.errf("expected , in expand()")
		}
		p.advance()
		edge := p.peek()
		if edge.kind != tString {
			return SelectItem{}, p.errf("expected edge name string in expand(), got %q", edge.text)
		}
		p.advance()
		if p.peek().kind != tRParen {
			return SelectItem{}, p.errf("expected ) to close expand()")
		}
		p.advance()
		return p.withAlias(SelectItem{ExpandCol: col.text, ExpandEdge: edge.text})
	}

	// plain column
	if t.kind == tIdent {
		p.advance()
		return p.withAlias(SelectItem{Column: t.text})
	}
	return SelectItem{}, p.errf("unexpected token %q in select list", t.text)
}

func (p *parser) withAlias(item SelectItem) (SelectItem, error) {
	if p.acceptKeyword("AS") {
		a := p.peek()
		if a.kind != tIdent {
			return SelectItem{}, p.errf("expected alias after AS, got %q", a.text)
		}
		p.advance()
		item.Alias = a.text
	}
	return item, nil
}

func (p *parser) parseWhere() ([]Predicate, error) {
	var preds []Predicate
	for {
		pr, err := p.parsePredicate()
		if err != nil {
			return nil, err
		}
		preds = append(preds, pr)
		if p.acceptKeyword("AND") {
			continue
		}
		break
	}
	return preds, nil
}

func (p *parser) parsePredicate() (Predicate, error) {
	t := p.peek()

	// MATCH(col, 'terms')
	if t.kind == tKeyword && t.text == "MATCH" {
		p.advance()
		if p.peek().kind != tLParen {
			return Predicate{}, p.errf("expected ( after MATCH")
		}
		p.advance()
		col := p.peek()
		if col.kind != tIdent {
			return Predicate{}, p.errf("expected column in MATCH(), got %q", col.text)
		}
		p.advance()
		if p.peek().kind != tComma {
			return Predicate{}, p.errf("expected , in MATCH()")
		}
		p.advance()
		terms := p.peek()
		if terms.kind != tString {
			return Predicate{}, p.errf("expected terms string in MATCH(), got %q", terms.text)
		}
		p.advance()
		if p.peek().kind != tRParen {
			return Predicate{}, p.errf("expected ) to close MATCH()")
		}
		p.advance()
		return Predicate{Kind: PredMatch, Column: col.text, MatchTerms: terms.text}, nil
	}

	// col op literal
	if t.kind != tIdent {
		return Predicate{}, p.errf("expected column in predicate, got %q", t.text)
	}
	p.advance()
	col := t.text

	op, err := p.parseCompareOp()
	if err != nil {
		return Predicate{}, err
	}
	lit, err := p.parseLiteral()
	if err != nil {
		return Predicate{}, err
	}
	if op == OpLike && lit.Kind != LitString {
		return Predicate{}, p.errf("LIKE requires a string pattern")
	}
	return Predicate{Kind: PredCompare, Column: col, Op: op, Value: lit}, nil
}

func (p *parser) parseCompareOp() (CompareOp, error) {
	t := p.peek()
	switch t.kind {
	case tEq:
		p.advance()
		return OpEq, nil
	case tGE:
		p.advance()
		return OpGE, nil
	case tGT:
		p.advance()
		return OpGT, nil
	case tLE:
		p.advance()
		return OpLE, nil
	case tLT:
		p.advance()
		return OpLT, nil
	case tKeyword:
		if t.text == "LIKE" {
			p.advance()
			return OpLike, nil
		}
	}
	return 0, p.errf("expected comparison operator, got %q", t.text)
}

func (p *parser) parseLiteral() (Literal, error) {
	t := p.peek()
	switch t.kind {
	case tString:
		p.advance()
		return Literal{Kind: LitString, Str: t.text}, nil
	case tNumber:
		p.advance()
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return Literal{}, p.errf("invalid number %q", t.text)
		}
		return Literal{Kind: LitNumber, Num: f}, nil
	}
	return Literal{}, p.errf("expected literal, got %q", t.text)
}

func (p *parser) parseTimestamp() (int64, error) {
	t := p.peek()
	switch t.kind {
	case tNumber:
		p.advance()
		v, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return 0, p.errf("AS OF expects an integer epoch-ms, got %q", t.text)
		}
		return v, nil
	case tString:
		p.advance()
		v, err := strconv.ParseInt(strings.TrimSpace(t.text), 10, 64)
		if err != nil {
			return 0, p.errf("AS OF expects an integer epoch-ms string, got %q", t.text)
		}
		return v, nil
	}
	return 0, p.errf("expected timestamp after AS OF, got %q", t.text)
}

func (p *parser) parseOrderBy() (*OrderBy, error) {
	col := p.peek()
	if col.kind != tIdent {
		return nil, p.errf("expected column in ORDER BY, got %q", col.text)
	}
	p.advance()
	if p.peek().kind != tArrow {
		return nil, p.errf("ORDER BY requires the vector-distance operator <->")
	}
	p.advance()
	ve, err := p.parseVectorExpr()
	if err != nil {
		return nil, err
	}
	return &OrderBy{Column: col.text, Target: ve}, nil
}

func (p *parser) parseVectorExpr() (VectorExpr, error) {
	t := p.peek()
	switch t.kind {
	case tLBracket:
		return p.parseVectorLiteral()
	case tColon:
		p.advance()
		name := p.peek()
		if name.kind != tIdent {
			return VectorExpr{}, p.errf("expected parameter name after :, got %q", name.text)
		}
		p.advance()
		return VectorExpr{Kind: VecParam, Param: name.text}, nil
	case tString:
		p.advance()
		return VectorExpr{Kind: VecText, Text: t.text}, nil
	}
	return VectorExpr{}, p.errf("expected [vector], :param, or 'text' after <->, got %q", t.text)
}

func (p *parser) parseVectorLiteral() (VectorExpr, error) {
	p.advance() // [
	var vec []float32
	if p.peek().kind == tRBracket {
		return VectorExpr{}, p.errf("empty vector literal")
	}
	for {
		n := p.peek()
		if n.kind != tNumber {
			return VectorExpr{}, p.errf("expected number in vector literal, got %q", n.text)
		}
		p.advance()
		f, err := strconv.ParseFloat(n.text, 32)
		if err != nil {
			return VectorExpr{}, p.errf("invalid vector component %q", n.text)
		}
		vec = append(vec, float32(f))
		if p.peek().kind == tComma {
			p.advance()
			continue
		}
		break
	}
	if p.peek().kind != tRBracket {
		return VectorExpr{}, p.errf("expected ] to close vector literal")
	}
	p.advance()
	return VectorExpr{Kind: VecLiteral, Literal: vec}, nil
}
