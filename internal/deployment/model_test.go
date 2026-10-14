package deployment

import (
	"strings"
	"testing"
)

func TestParseEnums(t *testing.T) {
	t.Run("modes", func(t *testing.T) {
		for _, value := range []string{"single", "distributed", "accumulo"} {
			mode, err := ParseMode(value)
			if err != nil || string(mode) != value {
				t.Fatalf("ParseMode(%q) = %q, %v", value, mode, err)
			}
		}
		if _, err := ParseMode("clustered"); err == nil {
			t.Fatal("ParseMode accepted an unknown mode")
		}
	})

	t.Run("phases", func(t *testing.T) {
		values := []string{
			"ready", "quiescing", "checkpointing", "reconciling",
			"switching-authority", "validating", "failed", "rolling-back",
		}
		for _, value := range values {
			phase, err := ParsePhase(value)
			if err != nil || string(phase) != value {
				t.Fatalf("ParsePhase(%q) = %q, %v", value, phase, err)
			}
		}
		if _, err := ParsePhase("migrating"); err == nil {
			t.Fatal("ParsePhase accepted an unknown phase")
		}
	})

	t.Run("authorities", func(t *testing.T) {
		for _, value := range []string{"local", "shoal-coordinated", "accumulo"} {
			authority, err := ParseAuthority(value)
			if err != nil || string(authority) != value {
				t.Fatalf("ParseAuthority(%q) = %q, %v", value, authority, err)
			}
		}
		if _, err := ParseAuthority("shared"); err == nil {
			t.Fatal("ParseAuthority accepted an unknown authority")
		}
	})
}

func TestNewState(t *testing.T) {
	for _, mode := range []Mode{ModeSingle, ModeDistributed, ModeAccumulo} {
		state, err := NewState(mode)
		if err != nil {
			t.Fatalf("NewState(%q): %v", mode, err)
		}
		if state.CurrentMode != mode || state.DesiredMode != mode || state.Phase != PhaseReady {
			t.Fatalf("NewState(%q) = %+v", mode, state)
		}
		if state.Authority != AuthorityForMode(mode) {
			t.Fatalf("NewState(%q) authority = %q", mode, state.Authority)
		}
		if err := state.Validate(); err != nil {
			t.Fatalf("NewState(%q) invalid: %v", mode, err)
		}
	}
}

func TestTransitions(t *testing.T) {
	tests := []struct {
		name         string
		from         Mode
		to           Mode
		wantPhase    Phase
		wantWarning  string
		wantRequired string
	}{
		{
			name:         "single to distributed",
			from:         ModeSingle,
			to:           ModeDistributed,
			wantPhase:    PhaseReconciling,
			wantWarning:  "Kubernetes Lease coordination is not implemented",
			wantRequired: "reconcile the requested Shoal topology",
		},
		{
			name:         "distributed to single",
			from:         ModeDistributed,
			to:           ModeSingle,
			wantPhase:    PhaseReconciling,
			wantWarning:  "Kubernetes Lease coordination is not implemented",
			wantRequired: "reconcile the requested Shoal topology",
		},
		{
			name:         "single to accumulo",
			from:         ModeSingle,
			to:           ModeAccumulo,
			wantPhase:    PhaseQuiescing,
			wantWarning:  "does not migrate data or switch live authority",
			wantRequired: "quiesce all writes",
		},
		{
			name:         "distributed to accumulo",
			from:         ModeDistributed,
			to:           ModeAccumulo,
			wantPhase:    PhaseQuiescing,
			wantWarning:  "does not migrate data or switch live authority",
			wantRequired: "create a durable checkpoint",
		},
		{
			name:         "accumulo to single",
			from:         ModeAccumulo,
			to:           ModeSingle,
			wantPhase:    PhaseQuiescing,
			wantWarning:  "does not migrate data or switch live authority",
			wantRequired: "validate reconciliation before switching authority",
		},
		{
			name:         "accumulo to distributed",
			from:         ModeAccumulo,
			to:           ModeDistributed,
			wantPhase:    PhaseQuiescing,
			wantWarning:  "does not migrate data or switch live authority",
			wantRequired: "quiesce all writes",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, err := NewState(test.from)
			if err != nil {
				t.Fatal(err)
			}
			state.Generation = 7
			result, err := Transition(state, test.to)
			if err != nil {
				t.Fatalf("Transition: %v", err)
			}
			if result.Idempotent {
				t.Fatal("new transition reported idempotent")
			}
			if result.State.CurrentMode != test.from || result.State.DesiredMode != test.to {
				t.Fatalf("transition modes = %q -> %q", result.State.CurrentMode, result.State.DesiredMode)
			}
			if result.State.Phase != test.wantPhase {
				t.Fatalf("phase = %q, want %q", result.State.Phase, test.wantPhase)
			}
			if result.State.Authority != state.Authority {
				t.Fatalf("authority switched from %q to %q", state.Authority, result.State.Authority)
			}
			if result.State.Generation != 8 {
				t.Fatalf("generation = %d, want 8", result.State.Generation)
			}
			if result.State.LastValidation != "pending" {
				t.Fatalf("last validation = %q", result.State.LastValidation)
			}
			if !containsSubstring(result.Warnings, test.wantWarning) {
				t.Fatalf("warnings %q do not contain %q", result.Warnings, test.wantWarning)
			}
			if !containsSubstring(result.Requirements, test.wantRequired) {
				t.Fatalf("requirements %q do not contain %q", result.Requirements, test.wantRequired)
			}
			if err := result.State.Validate(); err != nil {
				t.Fatalf("transition produced invalid state: %v", err)
			}
		})
	}
}

func TestTransitionIdempotenceAndRejection(t *testing.T) {
	state, err := NewState(ModeSingle)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation = 4

	same, err := Transition(state, ModeSingle)
	if err != nil {
		t.Fatal(err)
	}
	if !same.Idempotent || same.State.Generation != 4 {
		t.Fatalf("same-mode transition = %+v", same)
	}

	accepted, err := Transition(state, ModeAccumulo)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := Transition(accepted.State, ModeAccumulo)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Idempotent || replayed.State.Generation != 5 {
		t.Fatalf("replayed transition = %+v", replayed)
	}

	if _, err := Transition(accepted.State, ModeDistributed); err == nil {
		t.Fatal("accepted replacement intent while transition was in progress")
	}
}

func TestStateValidationRejectsInconsistentStates(t *testing.T) {
	base, err := NewState(ModeSingle)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*State)
	}{
		{"invalid current mode", func(state *State) { state.CurrentMode = "bad" }},
		{"invalid desired mode", func(state *State) { state.DesiredMode = "bad" }},
		{"invalid phase", func(state *State) { state.Phase = "bad" }},
		{"invalid authority", func(state *State) { state.Authority = "bad" }},
		{"empty validation", func(state *State) { state.LastValidation = "" }},
		{"ready mismatch", func(state *State) { state.DesiredMode = ModeAccumulo }},
		{"ready wrong authority", func(state *State) { state.Authority = AuthorityAccumulo }},
		{"transition same mode", func(state *State) { state.Phase = PhaseQuiescing }},
		{"early authority switch", func(state *State) {
			state.DesiredMode = ModeAccumulo
			state.Phase = PhaseCheckpointing
			state.Authority = AuthorityAccumulo
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := base
			test.mutate(&state)
			if err := state.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", state)
			}
		})
	}
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}
