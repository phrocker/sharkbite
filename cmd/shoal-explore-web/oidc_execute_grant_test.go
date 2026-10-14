// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// TestTheFleetMappingDoesNotGrantExecute is the guard on an upgrade hazard, and
// it exists because adding one line to a list was the whole of the mistake.
//
// oidcFleetOperations is not a ceiling something else narrows: authority()
// grants it verbatim to any token whose claim matches -oidc-fleet-values. Every
// fleet principal is minted with the same authorization domain, source and
// policy, and authorizeResource compares only those three — AuthorizeObject
// validates ObjectID and never consults it. So the only thing separating two
// fleet principals' queued work is the principal check that OperationExecute
// skips by design.
//
// Granting execute here would therefore hand every existing fleet-mapped token,
// on upgrade, the ability to pull another principal's records, claim them, and
// complete them with a fabricated outcome — the exact property the operation
// was introduced to prevent.
//
// That stays true after #391. Execute now has a mapping of its own — the
// executor mint, on an audience and possibly an issuer of its own — and that
// mapping is the only one allowed to grant it. Every other list stays
// execute-free, and this asserts the absence over every list the command
// defines except the executor's, so a new list cannot be added unguarded.
func TestTheFleetMappingDoesNotGrantExecute(t *testing.T) {
	lists := []struct {
		name string
		set  []auth.Operation
	}{
		{"oidcFleetOperations", oidcFleetOperations},
		{"oidcContributorOperations", oidcContributorOperations},
		{"oidcReaderOperations", oidcReaderOperations},
		// The fourth list in this command, and the one an earlier version of
		// this test missed. workspaceOperations is the -dev-auth principal's
		// ceiling, so it is not OIDC-minted — which is exactly why asserting
		// only the three OIDC lists made the claim "unreachable by an
		// OIDC-minted token" true and the conclusion wrong. The dev
		// authenticator serves the same HTTP routes.
		{"workspaceOperations", workspaceOperations},
		{"oidcApproverOperations", oidcApproverOperations},
	}
	covered := map[string]bool{"oidcExecutorOperations": true}
	for _, list := range lists {
		covered[list.name] = true
		for _, operation := range list.set {
			if operation == auth.OperationExecute {
				t.Fatalf("the %s list grants OperationExecute. Every "+
					"principal it mints shares one authorization scope — "+
					"authorizeResource compares only domain, source and "+
					"policy and never matches ObjectID — so this lets any of "+
					"them take the routes that do not compare the caller to "+
					"the record, and claim and complete another's queued work",
					list.name)
			}
		}
	}
	for _, defined := range operationListsDefinedHere(t) {
		if !covered[defined] {
			t.Fatalf("the operation list %s has no row here; every list is "+
				"granted verbatim to some principal, and only the executor "+
				"mint may hold execute", defined)
		}
	}

	// And the operation is still a real one, so this is a statement about the
	// mapping rather than about the vocabulary. A test that passed because the
	// constant had been deleted would assert nothing.
	if err := auth.OperationExecute.Validate(); err != nil {
		t.Fatalf("OperationExecute is not a valid operation: %v", err)
	}
	if !auth.ServiceRoleActionExecution.Allows(auth.OperationExecute) {
		t.Fatal("ServiceRoleActionExecution no longer grants execute")
	}
}

// TestExecuteComesOnlyFromTheExecutorMint is the presence half (#391). The
// executor mint is the only source of OperationExecute in this command, in
// source and in behaviour, and every execute decision it produces carries
// ServiceRoleActionExecution and a non-empty executor binding: a role-less or
// unbound execute decision would claim nothing (#573) at best, and would be
// the cross-principal hazard above at worst.
func TestExecuteComesOnlyFromTheExecutorMint(t *testing.T) {
	// The list is exactly [execute, agent_resolve]: execute, and the
	// resolve of its own descriptor that the fleet confines to the binding
	// (#391). Nothing that registers, heartbeats or reads beyond that.
	if len(oidcExecutorOperations) != 2 ||
		oidcExecutorOperations[0] != auth.OperationExecute ||
		oidcExecutorOperations[1] != auth.OperationAgentResolve {
		t.Fatalf("oidcExecutorOperations = %v, want [execute agent_resolve]",
			oidcExecutorOperations)
	}

	// In source, resolved by go/types: no expression in this command's
	// non-test code denotes execute — auth.OperationExecute through any
	// import name (an alias, a dot import), or any constant of type
	// auth.Operation equal to "execute", such as auth.Operation("execute")
	// — except inside the oidcExecutorOperations declaration, where it
	// appears once.
	scanner := newExecuteGrantScanner(t)
	files := scanner.parseDir(t, ".")
	inside, outside := scanner.sites(t, files)
	for _, site := range outside {
		t.Errorf("%s denotes OperationExecute outside oidcExecutorOperations; "+
			"the executor mint is the only source of execute", site)
	}
	if inside != 1 {
		t.Fatalf("execute appears %d times in oidcExecutorOperations, want once",
			inside)
	}

	// In behaviour: every kind of token this authenticator mints, with
	// every mapping configured at once. Only the executor tokens hold
	// execute, and each carries the role and its binding.
	f := newExecutorFixture(t)
	everything := func(claims jwt.MapClaims) jwt.MapClaims {
		claims["access"] = []string{"reader", "writer", "fleet"}
		claims["groups"] = []string{labelGroupSecret}
		return claims
	}
	type minted struct {
		name     string
		token    string
		executor string
	}
	tokens := []minted{
		{name: "workspace", token: f.human.signRS256(t, testKID,
			everything(f.human.defaultClaims(f.now)))},
		{name: "approver", token: f.human.signRS256(t, testKID,
			approverClaims(f.human, f.now, "bob"))},
		{name: "executor", token: f.executorToken(t, everything(
			executorClaims(f.executor, f.now, testExecutorSubject))),
			executor: testExecutorRef},
		{name: "second executor", token: f.executorToken(t,
			executorClaims(f.executor, f.now, testOtherExecutorSubject)),
			executor: testOtherExecutorRef},
	}
	for _, sample := range tokens {
		decision, err := f.authn.Authenticate(bearerRequest(sample.token))
		if err != nil {
			t.Fatalf("%s token: %v", sample.name, err)
		}
		executes := false
		for _, operation := range decision.AllowedOperations() {
			executes = executes || operation == auth.OperationExecute
		}
		if executes != (sample.executor != "") {
			t.Fatalf("the %s decision holds execute = %v", sample.name, executes)
		}
		if !executes {
			if decision.ServiceRole() != "" || decision.ExecutorBinding() != "" {
				t.Fatalf("the %s decision carries role %q and binding %q",
					sample.name, decision.ServiceRole(), decision.ExecutorBinding())
			}
			continue
		}
		if decision.ServiceRole() != auth.ServiceRoleActionExecution ||
			decision.ExecutorBinding() == "" ||
			decision.ExecutorBinding() != sample.executor {
			t.Fatalf("the %s decision holds execute with role %q and binding "+
				"%q; every execute decision carries action_execution and "+
				"its own binding", sample.name, decision.ServiceRole(),
				decision.ExecutorBinding())
		}
	}

	// Without the mapping, no token mints execute at all: the executor
	// audience is simply not one this authenticator accepts.
	plain := newTestOIDCAuthenticator(t, approverTestConfig(
		t, f.human, fixedClock(f.now), approverMappingDocument(f.human.server.URL)))
	claims := executorClaims(f.human, f.now, testExecutorSubject)
	if _, err := plain.Authenticate(bearerRequest(
		f.human.signRS256(t, testKID, claims))); err == nil {
		t.Fatal("an executor-audience token was minted with no executor mapping")
	}
}

// authPackagePath is the import path of the package that defines
// OperationExecute.
const authPackagePath = "github.com/phrocker/shoal-oss/pkg/explorer/auth"

// executeGrantScanner type-checks this command's sources against the
// compiler's export data, so an expression is judged by what it denotes and
// not by how it is spelled.
type executeGrantScanner struct {
	fset     *token.FileSet
	importer types.Importer
}

func newExecuteGrantScanner(t *testing.T) *executeGrantScanner {
	t.Helper()
	command := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Export", ".")
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list -export: %v", err)
	}
	exports := map[string]string{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var entry struct{ ImportPath, Export string }
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[entry.ImportPath] = entry.Export
	}
	if exports[authPackagePath] == "" {
		t.Fatalf("no export data for %s", authPackagePath)
	}
	fset := token.NewFileSet()
	return &executeGrantScanner{
		fset: fset,
		importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
			file, ok := exports[path]
			if !ok || file == "" {
				return nil, errors.New("no export data for " + path)
			}
			return os.Open(file)
		}),
	}
}

// parseDir parses the non-test Go files of dir.
func (s *executeGrantScanner) parseDir(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(s.fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, parsed)
	}
	return files
}

// sites type-checks files as one package and returns how many outermost
// expressions denoting execute lie inside an oidcExecutorOperations
// declaration, and the positions of those that lie anywhere else.
func (s *executeGrantScanner) sites(t *testing.T, files []*ast.File) (int, []string) {
	t.Helper()
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	config := types.Config{Importer: s.importer}
	if _, err := config.Check("main", s.fset, files, info); err != nil {
		t.Fatalf("type-check: %v", err)
	}
	denotesExecute := func(expression ast.Expr) bool {
		if identifier, ok := expression.(*ast.Ident); ok {
			if object, ok := info.Uses[identifier].(*types.Const); ok &&
				object.Pkg() != nil && object.Pkg().Path() == authPackagePath &&
				object.Name() == "OperationExecute" {
				return true
			}
		}
		if selector, ok := expression.(*ast.SelectorExpr); ok {
			if object, ok := info.Uses[selector.Sel].(*types.Const); ok &&
				object.Pkg() != nil && object.Pkg().Path() == authPackagePath &&
				object.Name() == "OperationExecute" {
				return true
			}
		}
		value, ok := info.Types[expression]
		if !ok || value.Value == nil || value.Value.Kind() != constant.String ||
			constant.StringVal(value.Value) != string(auth.OperationExecute) {
			return false
		}
		named, ok := value.Type.(*types.Named)
		return ok && named.Obj().Pkg() != nil &&
			named.Obj().Pkg().Path() == authPackagePath &&
			named.Obj().Name() == "Operation"
	}
	inside := 0
	var outside []string
	for _, file := range files {
		var allowed []ast.Node
		ast.Inspect(file, func(node ast.Node) bool {
			if spec, ok := node.(*ast.ValueSpec); ok {
				for _, identifier := range spec.Names {
					if identifier.Name == "oidcExecutorOperations" {
						allowed = append(allowed, spec)
					}
				}
			}
			return true
		})
		ast.Inspect(file, func(node ast.Node) bool {
			expression, ok := node.(ast.Expr)
			if !ok || !denotesExecute(expression) {
				return true
			}
			for _, spec := range allowed {
				if expression.Pos() >= spec.Pos() && expression.End() <= spec.End() {
					inside++
					return false
				}
			}
			outside = append(outside, s.fset.Position(expression.Pos()).String())
			return false
		})
	}
	return inside, outside
}

// TestTheExecuteGrantScannerSeesThroughSpelling is the scanner's own
// mutation check: an aliased import, a dot import, a conversion of the
// string "execute", a local typed constant and an untyped literal passed as
// an auth.Operation are each found outside the list.
func TestTheExecuteGrantScannerSeesThroughSpelling(t *testing.T) {
	scanner := newExecuteGrantScanner(t)
	for name, source := range map[string]string{
		"aliased import": `package main
import a "` + authPackagePath + `"
var granted = []a.Operation{a.OperationExecute}`,
		"dot import": `package main
import . "` + authPackagePath + `"
var granted = []Operation{OperationExecute}`,
		"conversion": `package main
import "` + authPackagePath + `"
var granted = []auth.Operation{auth.Operation("execute")}`,
		"local typed constant": `package main
import "` + authPackagePath + `"
const run auth.Operation = "execute"
var granted = []auth.Operation{run}`,
		"untyped literal": `package main
import "` + authPackagePath + `"
var granted = append([]auth.Operation(nil), "execute")`,
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := parser.ParseFile(scanner.fset, name+".go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, outside := scanner.sites(t, []*ast.File{parsed}); len(outside) == 0 {
				t.Fatal("the scanner did not see execute")
			}
		})
	}
	// Control: the same shapes inside oidcExecutorOperations are allowed,
	// and an unrelated "execute" string is not execute.
	parsed, err := parser.ParseFile(scanner.fset, "control.go", `package main
import a "`+authPackagePath+`"
var oidcExecutorOperations = []a.Operation{a.Operation("execute")}
var label = "execute"`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if inside, outside := scanner.sites(t, []*ast.File{parsed}); inside != 1 || len(outside) != 0 {
		t.Fatalf("control: inside %d, outside %v", inside, outside)
	}
}
