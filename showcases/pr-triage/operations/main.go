// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Command operations emits bounded syntax facts, not a resolved data-flow graph.
package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
)

type request struct {
	Content string `json:"content"`
}
type response struct {
	Facts      []string `json:"facts"`
	Error      string   `json:"error,omitempty"`
	Resolution string   `json:"resolution"`
	Omitted    int      `json:"omitted"`
}

func extract(content string) response {
	out := response{Facts: []string{}, Resolution: "syntax only; expression types, aliasing, dynamic dispatch and interprocedural flow unresolved"}
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "evidence.go", "package evidence\n"+content, parser.AllErrors)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	render := func(n ast.Node) string { var b bytes.Buffer; _ = format.Node(&b, fs, n); return b.String() }
	add := func(f string) {
		if len(out.Facts) >= 512 || len(f) > 1200 {
			out.Omitted++
			return
		}
		out.Facts = append(out.Facts, f)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			add("FUNCTION " + x.Name.Name)
			if x.Recv != nil {
				for _, field := range x.Recv.List {
					add("RECEIVER " + render(field.Type))
				}
			}
			if x.Type.Params != nil {
				for _, field := range x.Type.Params.List {
					for _, name := range field.Names {
						add("PARAM " + name.Name + " TYPE " + render(field.Type))
					}
				}
			}
		case *ast.AssignStmt:
			if len(x.Lhs) == len(x.Rhs) {
				for i, lhs := range x.Lhs {
					add("ASSIGN " + x.Tok.String() + " " + render(lhs) + " FROM " + render(x.Rhs[i]))
				}
			} else {
				add("TUPLE_ASSIGN_UNRESOLVED " + render(x))
			}
		case *ast.ValueSpec:
			if len(x.Names) == len(x.Values) {
				for i, name := range x.Names {
					add("DECLARE " + name.Name + " FROM " + render(x.Values[i]))
				}
			} else if len(x.Values) > 0 {
				add("TUPLE_DECLARE_UNRESOLVED " + render(x))
			}
		case *ast.CompositeLit:
			for _, element := range x.Elts {
				if field, ok := element.(*ast.KeyValueExpr); ok {
					typ := "implicit"
					if x.Type != nil {
						typ = render(x.Type)
					}
					add("CONSTRUCT " + typ + " KEY " + render(field.Key) + " FROM " + render(field.Value))
				}
			}
		case *ast.CallExpr:
			add("CALL " + render(x.Fun))
			for _, arg := range x.Args {
				add("ARG TO " + render(x.Fun) + " FROM " + render(arg))
			}
		case *ast.ReturnStmt:
			for _, value := range x.Results {
				add("RETURN " + render(value))
			}
		case *ast.IfStmt:
			add("BRANCH " + render(x.Cond))
		case *ast.SelectorExpr:
			add("SELECT " + x.Sel.Name + " FROM " + render(x.X))
		}
		return true
	})
	return out
}
func main() {
	var r request
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1000000)).Decode(&r); err != nil {
		panic(err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(extract(r.Content))
}
