//go:build !embed

package cclient

import (
	"testing"
)

func TestNewIterInfo_Validation(t *testing.T) {
	tests := []struct {
		name      string
		iterName  string
		className string
		wantErr   string
	}{
		{"ok", "vers", "org.apache.accumulo.core.iterators.user.VersioningIterator", ""},
		{"err-empty-name", "", "x.Y", "Name must be non-empty"},
		{"err-empty-class", "vers", "", "ClassName must be non-empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewIterInfo(tc.iterName, tc.className, 10, nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err: %v", err)
				}
				return
			}
			if err == nil || !contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestIterInfo_OptionsDefensiveCopy(t *testing.T) {
	src := map[string]string{"k": "v"}
	ii, err := NewIterInfo("vers", "x.Y", 10, src)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	src["k"] = "MUTATED"
	if ii.Options()["k"] != "v" {
		t.Errorf("options not defensively copied on construction: got %q", ii.Options()["k"])
	}
	got := ii.Options()
	got["k"] = "MUTATED2"
	if ii.Options()["k"] != "v" {
		t.Errorf("options not defensively copied on read: got %q", ii.Options()["k"])
	}
}

func TestIterInfo_ToThrift(t *testing.T) {
	ii, _ := NewIterInfo("vers", "org.apache.accumulo.core.iterators.user.VersioningIterator", 20, nil)
	tw := ii.ToThrift()
	if tw.IterName != "vers" {
		t.Errorf("IterName = %q", tw.IterName)
	}
	if tw.ClassName != "org.apache.accumulo.core.iterators.user.VersioningIterator" {
		t.Errorf("ClassName = %q", tw.ClassName)
	}
	if tw.Priority != 20 {
		t.Errorf("Priority = %d", tw.Priority)
	}
}

func TestIterInfoFromThrift_Roundtrip(t *testing.T) {
	src, _ := NewIterInfo("vers", "x.Y", 10, nil)
	back, err := IterInfoFromThrift(src.ToThrift())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if back.Name() != src.Name() || back.ClassName() != src.ClassName() || back.Priority() != src.Priority() {
		t.Errorf("roundtrip mismatch: %+v vs %+v", back, src)
	}
}

func TestIterInfoFromThrift_Nil(t *testing.T) {
	if _, err := IterInfoFromThrift(nil); err == nil {
		t.Error("nil IterInfo should error")
	}
}

func TestIterInfosToThrift_NilForEmpty(t *testing.T) {
	if got := IterInfosToThrift(nil); got != nil {
		t.Errorf("nil input → %v", got)
	}
}

func TestIterInfoSSIO_GroupsOptions(t *testing.T) {
	a, _ := NewIterInfo("a", "x.Y", 10, map[string]string{"k1": "v1"})
	b, _ := NewIterInfo("b", "x.Z", 20, nil)
	c, _ := NewIterInfo("c", "x.W", 30, map[string]string{"k2": "v2", "k3": "v3"})

	ssio := IterInfoSSIO([]*IterInfo{a, b, c})
	if len(ssio) != 2 {
		t.Fatalf("ssio len = %d, want 2 (b has no options)", len(ssio))
	}
	if ssio["a"]["k1"] != "v1" {
		t.Errorf("a.k1 = %q", ssio["a"]["k1"])
	}
	if _, ok := ssio["b"]; ok {
		t.Error("b should be absent (no options)")
	}
	if ssio["c"]["k2"] != "v2" || ssio["c"]["k3"] != "v3" {
		t.Errorf("c options wrong: %v", ssio["c"])
	}
}

func TestIterInfoSSIO_NilWhenAllEmpty(t *testing.T) {
	a, _ := NewIterInfo("a", "x.Y", 10, nil)
	b, _ := NewIterInfo("b", "x.Z", 20, nil)
	if got := IterInfoSSIO([]*IterInfo{a, b}); got != nil {
		t.Errorf("all-empty options → %v, want nil", got)
	}
}
