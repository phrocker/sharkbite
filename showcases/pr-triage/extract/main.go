// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file for details.
// The ASF licenses this file under the Apache License, Version 2.0.
// You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0.
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.

// Command extract parses supplied Go bytes. It never loads or executes a package.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"sort"
	"strings"
)

type request struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
type declaration struct {
	Key       string   `json:"key"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Start     int      `json:"start_byte"`
	End       int      `json:"end_byte"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Text      string   `json:"text"`
	Calls     []string `json:"syntactic_calls"`
}
type response struct {
	Declarations []declaration `json:"declarations"`
	Residue      string        `json:"residue"`
	Error        string        `json:"error,omitempty"`
}

func rendered(set *token.FileSet, node ast.Node) string {
	var b bytes.Buffer
	_ = format.Node(&b, set, node)
	return b.String()
}
func extract(r request) response {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, r.Path, r.Content, parser.ParseComments|parser.AllErrors)
	if err != nil {
		return response{Error: err.Error()}
	}
	result := response{Declarations: []declaration{}}
	cursor := 0
	seen := make(map[string]int)
	var residue strings.Builder
	for _, decl := range file.Decls {
		start, end := set.Position(decl.Pos()).Offset, set.Position(decl.End()).Offset
		d := declaration{Calls: []string{}}
		switch node := decl.(type) {
		case *ast.FuncDecl:
			d.Kind = "function"
			d.Name = node.Name.Name
			if node.Recv != nil {
				d.Name = rendered(set, node.Recv.List[0].Type) + "." + d.Name
			}
			if node.Doc != nil {
				start = set.Position(node.Doc.Pos()).Offset
			}
			d.Key = "function:" + d.Name
			calls := map[string]bool{}
			if node.Body != nil {
				ast.Inspect(node.Body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						calls[rendered(set, call.Fun)] = true
					}
					return true
				})
			}
			for call := range calls {
				d.Calls = append(d.Calls, call)
			}
			sort.Strings(d.Calls)
		case *ast.GenDecl:
			d.Kind = node.Tok.String()
			if node.Doc != nil {
				start = set.Position(node.Doc.Pos()).Offset
			}
			var names []string
			for _, spec := range node.Specs {
				switch item := spec.(type) {
				case *ast.TypeSpec:
					names = append(names, item.Name.Name)
				case *ast.ValueSpec:
					for _, name := range item.Names {
						names = append(names, name.Name)
					}
				case *ast.ImportSpec:
					names = append(names, item.Path.Value)
				}
			}
			d.Name = strings.Join(names, ",")
			d.Key = d.Kind + ":" + d.Name
		default:
			d.Kind = "unsupported"
			d.Key = fmt.Sprintf("unsupported:%d", start)
		}
		seen[d.Key]++
		if seen[d.Key] > 1 {
			d.Key = fmt.Sprintf("%s#%d", d.Key, seen[d.Key])
		}
		d.Start = start
		d.End = end
		d.StartLine = 1 + strings.Count(r.Content[:start], "\n")
		d.EndLine = 1 + strings.Count(r.Content[:end], "\n")
		d.Text = r.Content[start:end]
		residue.WriteString(r.Content[cursor:start])
		residue.WriteString("\n<declaration>\n")
		cursor = end
		result.Declarations = append(result.Declarations, d)
	}
	residue.WriteString(r.Content[cursor:])
	result.Residue = residue.String()
	return result
}
func main() {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1000001))
	if err != nil || len(data) > 1000000 {
		fmt.Fprintln(os.Stderr, "bounded input read failed")
		os.Exit(1)
	}
	var r request
	if err = json.Unmarshal(data, &r); err != nil {
		fmt.Fprintln(os.Stderr, "invalid request JSON")
		os.Exit(1)
	}
	if err = json.NewEncoder(os.Stdout).Encode(extract(r)); err != nil {
		os.Exit(1)
	}
}
