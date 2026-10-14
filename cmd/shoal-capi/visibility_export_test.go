package main

import (
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
)

func TestVisibilityRegistriesConcurrentGetters(t *testing.T) {
	visibility, err := accumulo.NewColumnVisibility([]byte("A&(B|C)"))
	if err != nil {
		t.Fatal(err)
	}
	visibilityID, ok := columnVisibilityValues.add(visibility)
	if !ok {
		t.Fatal("register visibility")
	}
	defer columnVisibilityValues.remove(visibilityID)
	node := visibility.Tree()
	nodeID, ok := visibilityNodeValues.add(&node)
	if !ok {
		t.Fatal("register node")
	}
	defer visibilityNodeValues.remove(nodeID)

	const goroutines = 32
	const iterations = 1000
	var wait sync.WaitGroup
	wait.Add(goroutines)
	for range goroutines {
		go func() {
			defer wait.Done()
			for range iterations {
				gotVisibility, found := columnVisibilityValues.get(visibilityID)
				if !found || string(gotVisibility.Expression()) != "A&(B|C)" {
					t.Errorf("unstable visibility lookup")
					return
				}
				gotNode, found := visibilityNodeValues.get(nodeID)
				if !found || gotNode.Type() != accumulo.VisibilityAnd || gotNode.Size() != 2 {
					t.Errorf("unstable node lookup")
					return
				}
			}
		}()
	}
	wait.Wait()
}

func TestVisibilityEvaluatorConcurrentReplacement(t *testing.T) {
	evaluator := accumulo.NewVisibilityEvaluator(
		accumulo.NewAuthorizations([]byte("A"), []byte("B")),
	)
	const readers = 24
	const iterations = 500
	var wait sync.WaitGroup
	wait.Add(readers + 1)
	for range readers {
		go func() {
			defer wait.Done()
			for range iterations {
				if _, err := evaluator.Evaluate([]byte("A|B")); err != nil {
					t.Errorf("evaluate: %v", err)
					return
				}
				_ = evaluator.Authorizations()
			}
		}()
	}
	go func() {
		defer wait.Done()
		for index := range iterations {
			if index%2 == 0 {
				evaluator.SetAuthorizations(accumulo.NewAuthorizations([]byte("A")))
			} else {
				evaluator.SetAuthorizations(nil)
			}
		}
	}()
	wait.Wait()
}

func TestVisibilityInputAndResultsAreCopied(t *testing.T) {
	expression := []byte("A")
	visibility, err := accumulo.NewColumnVisibility(expression)
	if err != nil {
		t.Fatal(err)
	}
	expression[0] = 'Z'
	if got := string(visibility.Expression()); got != "A" {
		t.Fatalf("expression = %q, want copied A", got)
	}
	tree := visibility.Tree()
	first := tree.Expression()
	first[0] = 'Z'
	if got := string(tree.Expression()); got != "A" {
		t.Fatalf("tree expression = %q, want independent copy", got)
	}
}
