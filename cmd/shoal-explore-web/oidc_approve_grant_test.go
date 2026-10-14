// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// TestNoMintedPrincipalCanApprove is the guard on the same upgrade hazard as
// TestTheFleetMappingDoesNotGrantExecute, for OperationActionApprove (#451).
//
// Every list here is granted verbatim to whatever principal it mints, and the
// fleet and dev-auth lists also grant dispatch and invoke. An approver holding
// either is refused by the approval service's separation check, so adding
// approve to one of those lists would grant nothing usable — but adding it to
// the reader or contributor list would mint approvers on upgrade for every
// token that maps there, with no role mapping saying who they are. Approval is
// a human role, and it stays unreachable through any minted list until it has
// a claim mapping of its own.
//
// All four lists, by construction. The #443 version of the execute guard first
// missed workspaceOperations — the -dev-auth principal's ceiling — on the
// argument that it is not OIDC-minted, and that is the one a reviewer found.
// The table below is checked against the set of lists this command defines, so
// a sixth list added without a row fails here rather than going unguarded.
//
// Since #451 slice 2 exactly one list may grant approve: oidcApproverOperations,
// minted only on the approver audience under the operator mapping file. It
// must grant approve and nothing else — in particular none of dispatch,
// invoke or execute, which the approval service refuses an approver for.
func TestNoMintedPrincipalCanApprove(t *testing.T) {
	lists := []struct {
		name     string
		set      []auth.Operation
		approver bool
	}{
		{"oidcFleetOperations", oidcFleetOperations, false},
		{"oidcContributorOperations", oidcContributorOperations, false},
		{"oidcReaderOperations", oidcReaderOperations, false},
		{"workspaceOperations", workspaceOperations, false},
		{"oidcApproverOperations", oidcApproverOperations, true},
		// The executor mint (#391): execute and nothing else.
		{"oidcExecutorOperations", oidcExecutorOperations, false},
	}
	covered := make([]string, len(lists))
	for i, list := range lists {
		covered[i] = list.name
	}
	sort.Strings(covered)
	defined := operationListsDefinedHere(t)
	if strings.Join(covered, ",") != strings.Join(defined, ",") {
		t.Fatalf("this guard covers %v and the command defines %v; every "+
			"package-level []auth.Operation is a list some principal is "+
			"minted with, and each needs a row here", covered, defined)
	}
	for _, list := range lists {
		if len(list.set) == 0 {
			t.Fatalf("the %s list is empty; a guard over an empty list "+
				"asserts nothing", list.name)
		}
		if list.approver {
			if len(list.set) != 1 ||
				list.set[0] != auth.OperationActionApprove {
				t.Fatalf("the %s list is %v; the approver role grants "+
					"action_approve and nothing else", list.name, list.set)
			}
			continue
		}
		for _, operation := range list.set {
			if operation == auth.OperationActionApprove {
				t.Fatalf("the %s list grants OperationActionApprove. It is "+
					"granted verbatim to every principal it mints, so this "+
					"makes approvers of them all with no mapping saying who "+
					"may approve", list.name)
			}
		}
	}
	// The operation and its role are real, so this is a statement about the
	// mappings and not about a vocabulary that lost the constant.
	if err := auth.OperationActionApprove.Validate(); err != nil {
		t.Fatalf("OperationActionApprove is not a valid operation: %v", err)
	}
	if !auth.ServiceRoleActionApproval.Allows(auth.OperationActionApprove) {
		t.Fatal("ServiceRoleActionApproval no longer grants approve")
	}
	for _, operation := range []auth.Operation{
		auth.OperationDispatch, auth.OperationInvoke, auth.OperationExecute,
	} {
		if auth.ServiceRoleActionApproval.Allows(operation) {
			t.Fatalf("ServiceRoleActionApproval grants %q; an approver that "+
				"may create or perform work is not a separate role", operation)
		}
	}
}

// operationListsDefinedHere returns the name of every package-level variable
// of type []auth.Operation declared in this command's non-test sources.
func operationListsDefinedHere(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	files := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(files, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range parsed.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, spec := range general.Specs {
				value := spec.(*ast.ValueSpec)
				if !isOperationSlice(value) {
					continue
				}
				for _, identifier := range value.Names {
					names = append(names, identifier.Name)
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

func isOperationSlice(value *ast.ValueSpec) bool {
	isType := func(expression ast.Expr) bool {
		array, ok := expression.(*ast.ArrayType)
		if !ok || array.Len != nil {
			return false
		}
		selector, ok := array.Elt.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := selector.X.(*ast.Ident)
		return ok && pkg.Name == "auth" && selector.Sel.Name == "Operation"
	}
	if value.Type != nil {
		return isType(value.Type)
	}
	for _, initial := range value.Values {
		if literal, ok := initial.(*ast.CompositeLit); ok && isType(literal.Type) {
			return true
		}
	}
	return false
}
