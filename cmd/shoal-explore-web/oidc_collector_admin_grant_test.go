// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/types"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// TestNoMintedListGrantsCollectorAdmin is the guard on OperationCollectorAdmin
// (#446), modelled on TestTheFleetMappingDoesNotGrantExecute.
//
// Collector admin binds a principal to a collector ID and sets its authority
// ceiling, control and mode: it is the authority behind every collector's
// authority. Every list here is granted verbatim to whatever principal it
// mints, so the operation on any of them would make collector admins of every
// token that maps there, on upgrade, with no mapping saying who they are. And
// unlike execute and approve it has no mapping of its own in this command at
// all: no audience, no operator file, no flag. It reaches a decision only from
// a host that mints its own decisions for the routes it mounts
// (webapi.MountCollectorAdmin), and this command mounts no collector route.
//
// The table is checked against every list the command defines, so a new list
// added without a row fails here rather than going unguarded.
func TestNoMintedListGrantsCollectorAdmin(t *testing.T) {
	lists := map[string][]auth.Operation{
		"oidcFleetOperations":       oidcFleetOperations,
		"oidcContributorOperations": oidcContributorOperations,
		"oidcReaderOperations":      oidcReaderOperations,
		"workspaceOperations":       workspaceOperations,
		"oidcApproverOperations":    oidcApproverOperations,
		"oidcExecutorOperations":    oidcExecutorOperations,
	}
	covered := make([]string, 0, len(lists))
	for name, set := range lists {
		covered = append(covered, name)
		if len(set) == 0 {
			t.Fatalf("the %s list is empty; a guard over it asserts nothing", name)
		}
		for _, operation := range set {
			if operation == auth.OperationCollectorAdmin {
				t.Fatalf("the %s list grants OperationCollectorAdmin; every "+
					"principal it mints could then provision collectors and "+
					"set their authority", name)
			}
		}
	}
	sort.Strings(covered)
	if defined := operationListsDefinedHere(t); strings.Join(covered, ",") != strings.Join(defined, ",") {
		t.Fatalf("this guard covers %v and the command defines %v; every "+
			"package-level []auth.Operation needs a row here", covered, defined)
	}
	if err := auth.OperationCollectorAdmin.Validate(); err != nil {
		t.Fatalf("OperationCollectorAdmin is not a valid operation, so this "+
			"guard asserts nothing: %v", err)
	}
}

// TestCollectorAdminIsDenotedNowhereInThisCommand is the source half: no
// expression in this command's non-test code denotes collector_admin, by
// constant, by alias or dot import, or as any constant of type
// auth.Operation equal to "collector_admin". Execute and approve each have one
// allowed site (their own mint); collector admin has none.
func TestCollectorAdminIsDenotedNowhereInThisCommand(t *testing.T) {
	scanner := newExecuteGrantScanner(t)
	files := scanner.parseDir(t, ".")
	for _, site := range operationSites(t, scanner, files, "OperationCollectorAdmin", auth.OperationCollectorAdmin) {
		t.Errorf("%s denotes OperationCollectorAdmin; this command mints it nowhere", site)
	}
}

// TestTheCollectorAdminScannerSeesThroughSpelling is the scanner's mutation
// check, with a control.
func TestTheCollectorAdminScannerSeesThroughSpelling(t *testing.T) {
	scanner := newExecuteGrantScanner(t)
	for name, source := range map[string]string{
		"constant": `package main
import "` + authPackagePath + `"
var granted = []auth.Operation{auth.OperationCollectorAdmin}`,
		"aliased import": `package main
import a "` + authPackagePath + `"
var granted = []a.Operation{a.OperationCollectorAdmin}`,
		"dot import": `package main
import . "` + authPackagePath + `"
var granted = []Operation{OperationCollectorAdmin}`,
		"conversion": `package main
import "` + authPackagePath + `"
var granted = []auth.Operation{auth.Operation("collector_admin")}`,
		"untyped literal": `package main
import "` + authPackagePath + `"
var granted = append([]auth.Operation(nil), "collector_admin")`,
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := parser.ParseFile(scanner.fset, name+".go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(operationSites(t, scanner, []*ast.File{parsed}, "OperationCollectorAdmin", auth.OperationCollectorAdmin)) == 0 {
				t.Fatal("the scanner did not see collector_admin")
			}
		})
	}
	parsed, err := parser.ParseFile(scanner.fset, "control.go", `package main
import "`+authPackagePath+`"
var granted = []auth.Operation{auth.OperationRead}
var label = "collector_admin"`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sites := operationSites(t, scanner, []*ast.File{parsed}, "OperationCollectorAdmin", auth.OperationCollectorAdmin); len(sites) != 0 {
		t.Fatalf("control: %v", sites)
	}
}

// operationSites type-checks files as one package and returns the position of
// every outermost expression denoting operation: the named constant through
// any import spelling, or any constant of type auth.Operation with its value.
func operationSites(t *testing.T, s *executeGrantScanner, files []*ast.File, constName string, operation auth.Operation) []string {
	t.Helper()
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}}
	if _, err := (&types.Config{Importer: s.importer}).Check("main", s.fset, files, info); err != nil {
		t.Fatalf("type-check: %v", err)
	}
	isConst := func(identifier *ast.Ident) bool {
		object, ok := info.Uses[identifier].(*types.Const)
		return ok && object.Pkg() != nil && object.Pkg().Path() == authPackagePath && object.Name() == constName
	}
	denotes := func(expression ast.Expr) bool {
		switch e := expression.(type) {
		case *ast.Ident:
			if isConst(e) {
				return true
			}
		case *ast.SelectorExpr:
			if isConst(e.Sel) {
				return true
			}
		}
		value, ok := info.Types[expression]
		if !ok || value.Value == nil || value.Value.Kind() != constant.String || constant.StringVal(value.Value) != string(operation) {
			return false
		}
		named, ok := value.Type.(*types.Named)
		return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == authPackagePath && named.Obj().Name() == "Operation"
	}
	var sites []string
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			expression, ok := node.(ast.Expr)
			if !ok || !denotes(expression) {
				return true
			}
			sites = append(sites, s.fset.Position(expression.Pos()).String())
			return false
		})
	}
	return sites
}

// TestNoShippedAuthenticatorMintsCollectorAdmin is the behavioural half:
// every kind of decision this command mints, with every mapping configured at
// once and every claim value a token could carry naming the operation, holds
// no collector_admin. That covers the workspace (reader, contributor, fleet),
// approver and executor audiences, a delegated (on-behalf-of) token, and the
// -dev-auth principal.
func TestNoShippedAuthenticatorMintsCollectorAdmin(t *testing.T) {
	f := newExecutorFixture(t)
	smuggle := func(claims jwt.MapClaims) jwt.MapClaims {
		claims["access"] = []string{"reader", "writer", "fleet", "collector_admin", string(auth.OperationCollectorAdmin)}
		claims["groups"] = []string{labelGroupSecret, "collector_admin"}
		claims["scope"] = "collector_admin"
		return claims
	}
	tokens := map[string]string{
		"workspace": f.human.signRS256(t, testKID, smuggle(f.human.defaultClaims(f.now))),
		// The approver audience refuses a token carrying label groups, so
		// only the mapped-value claim is smuggled there.
		"approver": f.human.signRS256(t, testKID, func(c jwt.MapClaims) jwt.MapClaims {
			c["access"] = []string{"reader", "writer", "collector_admin"}
			return c
		}(approverClaims(f.human, f.now, "bob"))),
		"executor": f.executorToken(t, smuggle(executorClaims(f.executor, f.now, testExecutorSubject))),
	}
	holds := func(d auth.Decision) bool {
		for _, operation := range d.AllowedOperations() {
			if operation == auth.OperationCollectorAdmin {
				return true
			}
		}
		return false
	}
	minted := 0
	for name, token := range tokens {
		decision, err := f.authn.Authenticate(bearerRequest(token))
		if err != nil {
			t.Fatalf("%s token: %v", name, err)
		}
		minted++
		if holds(decision) {
			t.Fatalf("the %s decision holds collector_admin", name)
		}
	}

	// A delegated token: the workspace mapping with an on-behalf-of chain.
	delegating := f.human.testConfig(fixedClock(f.now))
	delegating.delegationClaim = "delegation"
	authn := newTestOIDCAuthenticator(t, delegating)
	claims := smuggle(f.human.defaultClaims(f.now))
	claims["delegation"] = []string{"delegate-a"}
	decision, err := authn.Authenticate(bearerRequest(f.human.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatalf("delegated token: %v", err)
	}
	if len(decision.OnBehalfOf()) == 0 {
		t.Fatal("the delegated token minted no chain; the case is vacuous")
	}
	if holds(decision) {
		t.Fatal("the delegated decision holds collector_admin")
	}

	development, err := newDevelopmentAuthenticator(func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err = development.Authenticate(httptest.NewRequest("GET", "/api/v1/meta", nil))
	if err != nil {
		t.Fatal(err)
	}
	if holds(decision) {
		t.Fatal("the -dev-auth decision holds collector_admin")
	}
	if minted != len(tokens) {
		t.Fatalf("minted %d of %d", minted, len(tokens))
	}
}
