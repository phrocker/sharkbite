// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strconv"
	"testing"
)

// Source analyzers used by the parity, coverage and hygiene tests. Each one
// fails closed: a form it cannot resolve is reported as a problem rather
// than skipped, so a vocabulary cannot grow past the reader unnoticed.
// mutation_test.go runs each against synthetic source.

func parseSource(t *testing.T, src string) []*ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	return []*ast.File{f}
}

// stringConsts maps every package-level constant with a string-literal value
// (typed, untyped or converted) to that value.
func stringConsts(files []*ast.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if v, ok := constString(vs.Values[i], ""); ok {
						out[name.Name] = v
					}
				}
			}
		}
	}
	return out
}

// constString reads a string literal, or a conversion T("literal") when
// typeName is empty or names T.
func constString(e ast.Expr, typeName string) (string, bool) {
	if s, ok := stringLit(e); ok {
		return s, true
	}
	if p, ok := e.(*ast.ParenExpr); ok {
		return constString(p.X, typeName)
	}
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || (typeName != "" && fn.Name != typeName) {
		return "", false
	}
	return stringLit(call.Args[0])
}

// typedConstsIn returns name → value for every constant of typeName: declared
// with the type (`X T = "x"`) or converted to it (`X = T("x")`). A constant of
// the type whose value is not one of those forms is a problem.
func typedConstsIn(files []*ast.File, typeName string) (map[string]string, []string) {
	out := map[string]string{}
	var problems []string
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				typed := false
				if typ, ok := vs.Type.(*ast.Ident); ok && typ.Name == typeName {
					typed = true
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						if typed {
							problems = append(problems, "constant "+name.Name+" has no value")
						}
						continue
					}
					value := vs.Values[i]
					converted := false
					if call, ok := value.(*ast.CallExpr); ok {
						if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == typeName {
							converted = true
						}
					}
					if !typed && !converted {
						continue
					}
					v, ok := constString(value, typeName)
					if !ok {
						problems = append(problems, fmt.Sprintf(
							"constant %s of type %s is not a string literal", name.Name, typeName))
						continue
					}
					out[name.Name] = v
				}
			}
		}
	}
	return out, problems
}

// errorCodesIn reads every value assigned to a field named ErrorCode, by
// assignment or composite literal. A string literal or a string constant is
// collected; a copy of another ErrorCode field (x.ErrorCode) carries a value
// collected where it was set; anything else is a problem, because the reader
// cannot say which codes it may produce.
func errorCodesIn(files []*ast.File) ([]string, []string) {
	consts := stringConsts(files)
	seen := map[string]bool{}
	var problems []string
	take := func(e ast.Expr) {
		if s, ok := constString(e, ""); ok {
			if s != "" {
				seen[s] = true
			}
			return
		}
		switch v := e.(type) {
		case *ast.Ident:
			if s, ok := consts[v.Name]; ok {
				if s != "" {
					seen[s] = true
				}
				return
			}
		case *ast.SelectorExpr:
			if v.Sel.Name == "ErrorCode" {
				return
			}
		}
		problems = append(problems, fmt.Sprintf("ErrorCode set from an unresolvable %T", e))
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "ErrorCode" {
						continue
					}
					if len(n.Rhs) != len(n.Lhs) {
						problems = append(problems, "ErrorCode set from a multi-value expression")
						continue
					}
					take(n.Rhs[i])
				}
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "ErrorCode" {
					take(n.Value)
				}
			}
			return true
		})
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, problems
}

// clockViolations reports every read of the clock, the environment's time
// zone, or a timer. Selectors are resolved through each file's imports, so an
// aliased or dot import of "time" is refused outright and cannot hide one.
func clockViolations(files []*ast.File) []string {
	denied := map[string]bool{
		"Now": true, "Since": true, "Until": true, "NewTimer": true,
		"AfterFunc": true, "NewTicker": true, "Tick": true, "After": true,
		"Sleep": true, "LoadLocation": true, "Local": true,
	}
	var out []string
	for _, f := range files {
		names := map[string]string{}
		for _, spec := range f.Imports {
			p, _ := strconv.Unquote(spec.Path.Value)
			name := path.Base(p)
			if spec.Name != nil {
				name = spec.Name.Name
				if p == "time" {
					out = append(out, fmt.Sprintf("%s imports time as %q", f.Name.Name, name))
				}
			}
			names[name] = p
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && names[x.Name] == "time" && x.Obj == nil {
					if denied[n.Sel.Name] {
						out = append(out, "time."+n.Sel.Name)
					}
				}
			case *ast.CallExpr:
				// t.Local() converts to the process's zone, which the
				// renderer must never depend on.
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Local" && len(n.Args) == 0 {
					out = append(out, "Local()")
				}
			}
			return true
		})
	}
	return out
}

// funcIn finds a top-level function by name.
func funcIn(files []*ast.File, name string) *ast.FuncDecl {
	for _, f := range files {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
				return fn
			}
		}
	}
	return nil
}

func identName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			return x.Name + "." + v.Sel.Name
		}
	}
	return ""
}

// effectiveStatePairs reads the (state, condition) pairs
// ApprovalService.effectiveState can return, resolving:
//   - `current.State` to the stored states of its case clause, or, outside
//     the switch, to the stored states no case clause handles;
//   - a returned `condition` variable to every non-empty condition the
//     `unreachable` method returns.
func effectiveStatePairs(files []*ast.File) ([][2]string, []string) {
	consts := stringConsts(files)
	states, _ := typedConstsIn(files, "ApprovalState")
	var problems []string
	fn := funcIn(files, "effectiveState")
	unreachable := funcIn(files, "unreachable")
	if fn == nil || unreachable == nil {
		return nil, []string{"effectiveState or unreachable not found"}
	}
	var conditions []string
	ast.Inspect(unreachable.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		if len(ret.Results) != 2 {
			problems = append(problems, "unreachable returns a form the reader cannot resolve")
			return true
		}
		if s, ok := stringLit(ret.Results[0]); ok && s == "" {
			return true // error return
		}
		v, ok := consts[identName(ret.Results[0])]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"unreachable returns an unresolvable %T", ret.Results[0]))
			return true
		}
		if v != "" {
			conditions = append(conditions, v)
		}
		return true
	})
	handled := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if clause, ok := n.(*ast.CaseClause); ok {
			for _, e := range clause.List {
				handled[consts[identName(e)]] = true
			}
		}
		return true
	})
	var unhandled []string
	for _, v := range states {
		if !handled[v] && v != "unresolvable" {
			unhandled = append(unhandled, v)
		}
	}
	var pairs [][2]string
	var walk func(stmts []ast.Stmt, caseStates []string)
	resolve := func(ret *ast.ReturnStmt, caseStates []string) {
		if len(ret.Results) != 3 {
			problems = append(problems, "effectiveState returns a form the reader cannot resolve")
			return
		}
		if s, ok := stringLit(ret.Results[0]); ok && s == "" {
			return // error return
		}
		var stateValues []string
		switch name := identName(ret.Results[0]); {
		case name == "current.State":
			stateValues = caseStates
		case consts[name] != "":
			stateValues = []string{consts[name]}
		default:
			problems = append(problems, "unresolvable returned state "+name)
			return
		}
		var conditionValues []string
		switch name := identName(ret.Results[1]); {
		case name == "condition":
			conditionValues = conditions
		default:
			v, ok := consts[name]
			if !ok {
				problems = append(problems, "unresolvable returned condition "+name)
				return
			}
			conditionValues = []string{v}
		}
		for _, s := range stateValues {
			for _, c := range conditionValues {
				pairs = append(pairs, [2]string{s, c})
			}
		}
	}
	walk = func(stmts []ast.Stmt, caseStates []string) {
		for _, stmt := range stmts {
			ast.Inspect(stmt, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CaseClause:
					var listed []string
					for _, e := range n.List {
						listed = append(listed, consts[identName(e)])
					}
					walk(n.Body, listed)
					return false
				case *ast.ReturnStmt:
					resolve(n, caseStates)
				}
				return true
			})
		}
	}
	walk(fn.Body.List, unhandled)
	if len(pairs) == 0 {
		problems = append(problems, "effectiveState returns nothing readable")
	}
	return pairs, problems
}

// dispatchDestinations reads every DispatchState a fleet record is set to,
// and the kind actionEventKind pairs with each state.
//
// It fails closed. A State field set by assignment, or in an ActionRecord
// literal (or an element literal whose type is elided), must be set to a
// DispatchState constant, a constant of another vocabulary (an approval's
// state), or a copy of another State field; any other value — a variable, a
// call, an expression — is a problem, since the reader cannot say which
// states it produces. Literals of other named types are another record's
// State and are skipped.
func dispatchDestinations(files []*ast.File) (map[string]bool, map[string]string, []string) {
	consts, _ := typedConstsIn(files, "DispatchState")
	all := stringConsts(files)
	destinations := map[string]bool{}
	var problems []string
	take := func(e ast.Expr, where string) {
		name := identName(e)
		if v, ok := consts[name]; ok {
			destinations[v] = true
			return
		}
		if _, ok := all[name]; ok {
			return // a constant of another vocabulary
		}
		if sel, ok := e.(*ast.SelectorExpr); ok && sel.Sel.Name == "State" {
			return // copied from another record
		}
		problems = append(problems, fmt.Sprintf("State set %s from an unresolvable %T", where, e))
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "State" {
						continue
					}
					if len(n.Rhs) != len(n.Lhs) {
						problems = append(problems, "State set from a multi-value expression")
						continue
					}
					take(n.Rhs[i], "by assignment")
				}
			case *ast.CompositeLit:
				typeName := ""
				switch typ := n.Type.(type) {
				case *ast.Ident:
					typeName = typ.Name
				case *ast.SelectorExpr:
					typeName = typ.Sel.Name
				case nil:
				default:
					return true
				}
				if typeName != "" && typeName != "ActionRecord" {
					return true
				}
				for _, elt := range n.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "State" {
						take(kv.Value, "in a literal")
					}
				}
			}
			return true
		})
	}
	kinds := map[string]string{}
	if fn := funcIn(files, "actionEventKind"); fn != nil {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok || len(clause.Body) != 1 {
				return true
			}
			ret, ok := clause.Body[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				return true
			}
			kind, ok := stringLit(ret.Results[0])
			if !ok {
				return true
			}
			for _, e := range clause.List {
				kinds[consts[identName(e)]] = kind
			}
			return true
		})
	} else {
		problems = append(problems, "actionEventKind not found")
	}
	return destinations, kinds, problems
}
