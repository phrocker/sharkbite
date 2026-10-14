// Package shoalql is a small read-only SQL frontend over shoal's scan
// engine. It parses a compact SELECT dialect and lowers it to a physical
// plan expressed as key ranges plus iterrt iterator-pushdown specs, so the
// heavy lifting (vector k-NN, keyword term-index, graph expansion, grouped
// aggregation, time-travel) runs inside the existing iterator stack rather
// than in the SQL layer.
//
// The layer is deliberately data-model-agnostic: the parser and planner
// never assume the graph layout. All knowledge of "how a logical table maps
// to Accumulo keys and which iterators serve a predicate" lives behind the
// TableBinding interface (see catalog.go). GraphBinding is the first
// implementation; a DataWave-style DocumentBinding for generic unstructured
// data can implement the same interface later without touching the grammar.
//
// Supported read dialect:
//
//	EXPLAIN [FORMAT <TEXT|JSON>]
//	SELECT <*|cols|count(*)|expand(col,'edge')>
//	FROM   <table> [AS OF <ts>]
//	[WHERE <col op literal | MATCH(col,'terms')> [AND ...]]
//	[GROUP BY <col>]
//	[ORDER BY <col> <-> <[v,..]|:param|'text'>]
//	[LIMIT <n>]
package shoalql

import (
	"fmt"
	"strings"
)

// tokenKind enumerates lexical token classes.
type tokenKind int

const (
	tEOF tokenKind = iota
	tIdent
	tString  // '...'
	tNumber  // 123 or 1.5 or -0.3
	tKeyword // SELECT, FROM, ...
	tComma
	tLParen
	tRParen
	tLBracket
	tRBracket
	tColon
	tStar
	tArrow // <-> (vector distance)
	tEq    // =
	tGE    // >=
	tGT    // >
	tLE    // <=
	tLT    // <
)

type token struct {
	kind tokenKind
	text string // raw/normalized text; for tString, the unquoted content
	pos  int
}

// keywords recognized by the lexer (matched case-insensitively and stored
// upper-cased in token.text).
var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "AND": true,
	"ORDER": true, "BY": true, "GROUP": true, "LIMIT": true,
	"AS": true, "OF": true, "MATCH": true, "LIKE": true,
	"COUNT": true, "EXPAND": true,
	"EXPLAIN": true, "FORMAT": true, "TEXT": true, "JSON": true,
}

type lexer struct {
	src string
	pos int
}

func (l *lexer) errf(format string, a ...any) error {
	return fmt.Errorf("shoalql: parse error at %d: %s", l.pos, fmt.Sprintf(format, a...))
}

// lex tokenizes the whole input.
func lex(src string) ([]token, error) {
	l := &lexer{src: src}
	var out []token
	for {
		tk, err := l.next()
		if err != nil {
			return nil, err
		}
		out = append(out, tk)
		if tk.kind == tEOF {
			return out, nil
		}
	}
}

func (l *lexer) next() (token, error) {
	for l.pos < len(l.src) && isSpace(l.src[l.pos]) {
		l.pos++
	}
	if l.pos >= len(l.src) {
		return token{kind: tEOF, pos: l.pos}, nil
	}
	start := l.pos
	c := l.src[l.pos]

	switch {
	case c == ',':
		l.pos++
		return token{tComma, ",", start}, nil
	case c == '(':
		l.pos++
		return token{tLParen, "(", start}, nil
	case c == ')':
		l.pos++
		return token{tRParen, ")", start}, nil
	case c == '[':
		l.pos++
		return token{tLBracket, "[", start}, nil
	case c == ']':
		l.pos++
		return token{tRBracket, "]", start}, nil
	case c == ':':
		l.pos++
		return token{tColon, ":", start}, nil
	case c == '*':
		l.pos++
		return token{tStar, "*", start}, nil
	case c == '=':
		l.pos++
		return token{tEq, "=", start}, nil
	case c == '\'':
		return l.lexString()
	case c == '<':
		return l.lexLess()
	case c == '>':
		l.pos++
		if l.pos < len(l.src) && l.src[l.pos] == '=' {
			l.pos++
			return token{tGE, ">=", start}, nil
		}
		return token{tGT, ">", start}, nil
	case c == '-' || isDigit(c):
		return l.lexNumber()
	case isIdentStart(c):
		return l.lexIdentOrKeyword()
	default:
		return token{}, l.errf("unexpected character %q", string(c))
	}
}

func (l *lexer) lexLess() (token, error) {
	start := l.pos
	l.pos++ // consume '<'
	if l.pos < len(l.src) && l.src[l.pos] == '-' &&
		l.pos+1 < len(l.src) && l.src[l.pos+1] == '>' {
		l.pos += 2
		return token{tArrow, "<->", start}, nil
	}
	if l.pos < len(l.src) && l.src[l.pos] == '=' {
		l.pos++
		return token{tLE, "<=", start}, nil
	}
	return token{tLT, "<", start}, nil
}

func (l *lexer) lexString() (token, error) {
	start := l.pos
	l.pos++ // opening quote
	var sb strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '\'' {
			// '' escapes a single quote.
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '\'' {
				sb.WriteByte('\'')
				l.pos += 2
				continue
			}
			l.pos++
			return token{tString, sb.String(), start}, nil
		}
		sb.WriteByte(c)
		l.pos++
	}
	return token{}, l.errf("unterminated string literal")
}

func (l *lexer) lexNumber() (token, error) {
	start := l.pos
	if l.src[l.pos] == '-' {
		l.pos++
	}
	seenDot := false
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '.' && !seenDot {
			seenDot = true
			l.pos++
			continue
		}
		if !isDigit(c) {
			break
		}
		l.pos++
	}
	txt := l.src[start:l.pos]
	if txt == "-" || txt == "." {
		return token{}, l.errf("malformed number %q", txt)
	}
	return token{tNumber, txt, start}, nil
}

func (l *lexer) lexIdentOrKeyword() (token, error) {
	start := l.pos
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
		l.pos++
	}
	word := l.src[start:l.pos]
	up := strings.ToUpper(word)
	if keywords[up] {
		return token{tKeyword, up, start}, nil
	}
	return token{tIdent, word, start}, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentPart(c byte) bool {
	return isIdentStart(c) || isDigit(c) || c == '.' || c == ':'
}
