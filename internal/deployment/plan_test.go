package deployment

import (
	"reflect"
	"testing"
)

func TestPlans(t *testing.T) {
	tests := []struct {
		mode         Mode
		authority    Authority
		components   []string
		requirements []string
		warnings     []string
	}{
		{
			mode:         ModeSingle,
			authority:    AuthorityLocal,
			components:   []string{"single Shoal runtime"},
			requirements: []string{"exclusive local write authority", "durable local state storage"},
		},
		{
			mode:         ModeDistributed,
			authority:    AuthorityShoalCoordinated,
			components:   []string{"multiple Shoal runtimes", "Kubernetes Lease coordinator"},
			requirements: []string{"shared durable storage", "exclusive shoal-coordinated write authority"},
			warnings:     []string{"Kubernetes Lease coordination is not implemented"},
		},
		{
			mode:         ModeAccumulo,
			authority:    AuthorityAccumulo,
			components:   []string{"Apache Accumulo", "ZooKeeper", "durable distributed storage"},
			requirements: []string{"exclusive Accumulo write authority", "validated checkpoint and reconciliation before any authority switch"},
			warnings:     []string{"shoalctl does not perform data migration or switch live authority"},
		},
	}

	for _, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			first, err := PlanFor(test.mode)
			if err != nil {
				t.Fatal(err)
			}
			second, err := PlanFor(test.mode)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("plan is not deterministic: %#v != %#v", first, second)
			}
			if first.Authority != test.authority ||
				!reflect.DeepEqual(first.Components, test.components) ||
				!reflect.DeepEqual(first.Requirements, test.requirements) ||
				!reflect.DeepEqual(first.Warnings, test.warnings) {
				t.Fatalf("plan = %#v", first)
			}
		})
	}

	if _, err := PlanFor("invalid"); err == nil {
		t.Fatal("PlanFor accepted an invalid mode")
	}
}
