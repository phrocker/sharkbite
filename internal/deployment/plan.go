package deployment

import "fmt"

type Plan struct {
	Mode         Mode
	Authority    Authority
	Components   []string
	Requirements []string
	Warnings     []string
}

func PlanFor(mode Mode) (Plan, error) {
	if !mode.Valid() {
		return Plan{}, fmt.Errorf("cannot plan invalid mode %q", mode)
	}

	switch mode {
	case ModeSingle:
		return Plan{
			Mode:       mode,
			Authority:  AuthorityLocal,
			Components: []string{"single Shoal runtime"},
			Requirements: []string{
				"exclusive local write authority",
				"durable local state storage",
			},
		}, nil
	case ModeDistributed:
		return Plan{
			Mode:       mode,
			Authority:  AuthorityShoalCoordinated,
			Components: []string{"multiple Shoal runtimes", "Kubernetes Lease coordinator"},
			Requirements: []string{
				"shared durable storage",
				"exclusive shoal-coordinated write authority",
			},
			Warnings: []string{"Kubernetes Lease coordination is not implemented"},
		}, nil
	case ModeAccumulo:
		return Plan{
			Mode:       mode,
			Authority:  AuthorityAccumulo,
			Components: []string{"Apache Accumulo", "ZooKeeper", "durable distributed storage"},
			Requirements: []string{
				"exclusive Accumulo write authority",
				"validated checkpoint and reconciliation before any authority switch",
			},
			Warnings: []string{"shoalctl does not perform data migration or switch live authority"},
		}, nil
	default:
		panic("validated mode not handled")
	}
}
