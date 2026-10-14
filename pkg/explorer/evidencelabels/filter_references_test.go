// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package evidencelabels

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type ref struct {
	name   string
	labels []string
	nodes  []shoal.ID
	edges  []shoal.ID
}

// labelsHeld answers from a fixed set of held labels.
type labelsHeld map[string]bool

func (h labelsHeld) VisibleToReader(_ context.Context, labels []string) (bool, error) {
	for _, label := range labels {
		if !h[label] {
			return false, nil
		}
	}
	return true, nil
}

// nodesVisible answers from a fixed set of currently visible nodes.
type nodesVisible struct {
	visible map[shoal.ID]bool
	err     error
}

func (n nodesVisible) GraphsVisibleToReader(_ context.Context, graphs []Graph) ([]bool, error) {
	if n.err != nil {
		return nil, n.err
	}
	verdicts := make([]bool, len(graphs))
	for index, graph := range graphs {
		verdicts[index] = true
		for _, id := range append(append([]shoal.ID(nil), graph.NodeIDs...), graph.EdgeIDs...) {
			if !n.visible[id] {
				verdicts[index] = false
			}
		}
	}
	return verdicts, nil
}

func (nodesVisible) GraphEvidenceValid(
	context.Context, shoal.ID, time.Time, []interaction.EvidenceReference,
) (bool, error) {
	return true, nil
}

func names(values []ref) []string {
	result := []string{}
	for _, value := range values {
		result = append(result, value.name)
	}
	return result
}

// TestFilterReferencesLetsCurrentNodesDecide pins the per-reference rule: a
// reference naming nodes is decided by the nodes' current rules alone, both
// after a tightening (stored labels held, node not visible) and after a
// loosening (stored labels not held, node visible); one naming none is
// decided by its stored labels; and a nil gate withholds every reference
// that names a node.
func TestFilterReferencesLetsCurrentNodesDecide(t *testing.T) {
	values := []ref{
		{name: "tightened", labels: []string{"a"}, nodes: []shoal.ID{"n-tight"}},
		{name: "loosened", labels: []string{"a", "b"}, nodes: []shoal.ID{"n-loose"}},
		{name: "nodeless-held", labels: []string{"a"}},
		{name: "nodeless-unheld", labels: []string{"b"}},
		{name: "nodeless-unlabelled"},
		{name: "open-nodes-closed-edge", nodes: []shoal.ID{"n-loose"}, edges: []shoal.ID{"e-closed"}},
		{name: "edge-only-open", labels: []string{"b"}, edges: []shoal.ID{"e-open"}},
	}
	labels := func(value ref) []string { return value.labels }
	nodes := func(value ref) Graph { return Graph{NodeIDs: value.nodes, EdgeIDs: value.edges} }
	held := labelsHeld{"a": true}
	gate := nodesVisible{visible: map[shoal.ID]bool{"n-loose": true, "e-open": true}}

	kept, withheld, err := FilterReferences(context.Background(), held, gate, values, labels, nodes)
	if err != nil || !withheld {
		t.Fatalf("withheld = %v, err = %v", withheld, err)
	}
	if got, want := names(kept), []string{"loosened", "nodeless-held", "nodeless-unlabelled", "edge-only-open"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}

	kept, _, err = FilterReferences(context.Background(), held, nil, values, labels, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(kept), []string{"nodeless-held", "nodeless-unlabelled"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("with no gate kept %v, want %v", got, want)
	}

	if _, _, err := FilterReferences(context.Background(), held,
		nodesVisible{err: errors.New("catalog down")}, values, labels, nodes); err == nil {
		t.Fatal("a failing gate was read as a refusal")
	}
}
