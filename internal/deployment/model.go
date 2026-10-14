package deployment

import (
	"fmt"
	"strings"
)

type Mode string

const (
	ModeSingle      Mode = "single"
	ModeDistributed Mode = "distributed"
	ModeAccumulo    Mode = "accumulo"
)

func ParseMode(value string) (Mode, error) {
	mode := Mode(strings.TrimSpace(value))
	if !mode.Valid() {
		return "", fmt.Errorf("invalid deployment mode %q", value)
	}
	return mode, nil
}

func (m Mode) Valid() bool {
	switch m {
	case ModeSingle, ModeDistributed, ModeAccumulo:
		return true
	default:
		return false
	}
}

type Phase string

const (
	PhaseReady              Phase = "ready"
	PhaseQuiescing          Phase = "quiescing"
	PhaseCheckpointing      Phase = "checkpointing"
	PhaseReconciling        Phase = "reconciling"
	PhaseSwitchingAuthority Phase = "switching-authority"
	PhaseValidating         Phase = "validating"
	PhaseFailed             Phase = "failed"
	PhaseRollingBack        Phase = "rolling-back"
)

func ParsePhase(value string) (Phase, error) {
	phase := Phase(strings.TrimSpace(value))
	if !phase.Valid() {
		return "", fmt.Errorf("invalid deployment phase %q", value)
	}
	return phase, nil
}

func (p Phase) Valid() bool {
	switch p {
	case PhaseReady, PhaseQuiescing, PhaseCheckpointing, PhaseReconciling,
		PhaseSwitchingAuthority, PhaseValidating, PhaseFailed, PhaseRollingBack:
		return true
	default:
		return false
	}
}

type Authority string

const (
	AuthorityLocal            Authority = "local"
	AuthorityShoalCoordinated Authority = "shoal-coordinated"
	AuthorityAccumulo         Authority = "accumulo"
)

func ParseAuthority(value string) (Authority, error) {
	authority := Authority(strings.TrimSpace(value))
	if !authority.Valid() {
		return "", fmt.Errorf("invalid authority domain %q", value)
	}
	return authority, nil
}

func (a Authority) Valid() bool {
	switch a {
	case AuthorityLocal, AuthorityShoalCoordinated, AuthorityAccumulo:
		return true
	default:
		return false
	}
}

func AuthorityForMode(mode Mode) Authority {
	switch mode {
	case ModeSingle:
		return AuthorityLocal
	case ModeDistributed:
		return AuthorityShoalCoordinated
	case ModeAccumulo:
		return AuthorityAccumulo
	default:
		return ""
	}
}

type State struct {
	CurrentMode    Mode      `json:"currentMode"`
	DesiredMode    Mode      `json:"desiredMode"`
	Phase          Phase     `json:"phase"`
	Authority      Authority `json:"authority"`
	Generation     uint64    `json:"generation"`
	LastValidation string    `json:"lastValidation"`
}

func NewState(mode Mode) (State, error) {
	if !mode.Valid() {
		return State{}, fmt.Errorf("cannot create state with invalid mode %q", mode)
	}
	return State{
		CurrentMode:    mode,
		DesiredMode:    mode,
		Phase:          PhaseReady,
		Authority:      AuthorityForMode(mode),
		LastValidation: "not-run",
	}, nil
}

func (s State) Validate() error {
	if !s.CurrentMode.Valid() {
		return fmt.Errorf("state has invalid current mode %q", s.CurrentMode)
	}
	if !s.DesiredMode.Valid() {
		return fmt.Errorf("state has invalid desired mode %q", s.DesiredMode)
	}
	if !s.Phase.Valid() {
		return fmt.Errorf("state has invalid phase %q", s.Phase)
	}
	if !s.Authority.Valid() {
		return fmt.Errorf("state has invalid authority %q", s.Authority)
	}
	if strings.TrimSpace(s.LastValidation) == "" {
		return fmt.Errorf("state last validation must not be empty")
	}

	currentAuthority := AuthorityForMode(s.CurrentMode)
	desiredAuthority := AuthorityForMode(s.DesiredMode)
	if s.Phase == PhaseReady {
		if s.CurrentMode != s.DesiredMode {
			return fmt.Errorf("ready state current mode %q does not match desired mode %q", s.CurrentMode, s.DesiredMode)
		}
		if s.Authority != currentAuthority {
			return fmt.Errorf("ready state authority %q does not match current mode authority %q", s.Authority, currentAuthority)
		}
		return nil
	}

	if s.CurrentMode == s.DesiredMode {
		return fmt.Errorf("phase %q requires different current and desired modes", s.Phase)
	}
	switch s.Phase {
	case PhaseQuiescing, PhaseCheckpointing, PhaseReconciling, PhaseSwitchingAuthority:
		if s.Authority != currentAuthority {
			return fmt.Errorf("phase %q cannot change authority before validation; got %q, want %q", s.Phase, s.Authority, currentAuthority)
		}
	case PhaseValidating, PhaseFailed, PhaseRollingBack:
		if s.Authority != currentAuthority && s.Authority != desiredAuthority {
			return fmt.Errorf("phase %q has authority %q unrelated to current or desired mode", s.Phase, s.Authority)
		}
	}
	return nil
}

type TransitionResult struct {
	State        State
	Idempotent   bool
	Requirements []string
	Warnings     []string
}

func Transition(state State, target Mode) (TransitionResult, error) {
	if err := state.Validate(); err != nil {
		return TransitionResult{}, fmt.Errorf("invalid current state: %w", err)
	}
	if !target.Valid() {
		return TransitionResult{}, fmt.Errorf("invalid target mode %q", target)
	}
	if target == state.DesiredMode {
		result := TransitionResult{State: state, Idempotent: true}
		if state.CurrentMode != state.DesiredMode {
			result.Requirements, result.Warnings = transitionDetails(state.CurrentMode, state.DesiredMode, state.Authority)
		}
		return result, nil
	}
	if state.Phase != PhaseReady {
		return TransitionResult{}, fmt.Errorf(
			"cannot replace transition intent to %q while generation %d is in phase %q toward %q",
			target, state.Generation, state.Phase, state.DesiredMode,
		)
	}
	if target == state.CurrentMode {
		return TransitionResult{State: state, Idempotent: true}, nil
	}

	next := state
	next.DesiredMode = target
	next.Generation++
	next.LastValidation = "pending"

	requirements, warnings := transitionDetails(state.CurrentMode, target, state.Authority)
	result := TransitionResult{
		State:        next,
		Requirements: requirements,
		Warnings:     warnings,
	}
	if crossesAccumuloBoundary(state.CurrentMode, target) {
		next.Phase = PhaseQuiescing
		result.State = next
		return result, nil
	}

	next.Phase = PhaseReconciling
	result.State = next
	return result, nil
}

func crossesAccumuloBoundary(from, to Mode) bool {
	return (from == ModeAccumulo) != (to == ModeAccumulo)
}

func transitionDetails(from, to Mode, authority Authority) ([]string, []string) {
	if crossesAccumuloBoundary(from, to) {
		return []string{
				"quiesce all writes",
				"create a durable checkpoint",
				"validate reconciliation before switching authority",
			}, []string{
				"authority remains " + string(authority) + "; this command does not migrate data or switch live authority",
			}
	}
	return []string{
			"reconcile the requested Shoal topology",
			"validate coordination before switching authority",
		}, []string{
			"Kubernetes Lease coordination is not implemented",
			"authority remains " + string(authority) + "; this command does not switch live authority",
		}
}
