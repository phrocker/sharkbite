package accumulo

import (
	"math"
	"strconv"
	"sync"
	"testing"
)

func TestConfigurationGetAndSet(t *testing.T) {
	config := NewConfiguration()
	if got := config.Get("missing"); got != "" {
		t.Fatalf("Get(missing) = %q, want empty", got)
	}
	if got := config.GetOr("missing", "fallback"); got != "fallback" {
		t.Fatalf("GetOr(missing) = %q, want fallback", got)
	}
	if _, ok := config.Lookup("missing"); ok {
		t.Fatal("Lookup(missing) reported a value")
	}

	config.Set("FILE_SYSTEM_ROOT", "/accumulo")
	if got := config.Get("FILE_SYSTEM_ROOT"); got != "/accumulo" {
		t.Fatalf("Get = %q", got)
	}
	if got := config.GetOr("FILE_SYSTEM_ROOT", "fallback"); got != "/accumulo" {
		t.Fatalf("GetOr = %q", got)
	}
	value, ok := config.Lookup("FILE_SYSTEM_ROOT")
	if !ok || value != "/accumulo" {
		t.Fatalf("Lookup = %q, %v", value, ok)
	}

	config.Set("FILE_SYSTEM_ROOT", "/accumulo2")
	if got := config.Get("FILE_SYSTEM_ROOT"); got != "/accumulo2" {
		t.Fatalf("Get after overwrite = %q", got)
	}

	// An explicitly empty value is set, unlike a missing key: only Lookup
	// can tell them apart, which is why it exists.
	config.Set("empty", "")
	if got := config.GetOr("empty", "fallback"); got != "" {
		t.Fatalf("GetOr(empty) = %q, want the stored empty value", got)
	}
	if _, ok := config.Lookup("empty"); !ok {
		t.Fatal("Lookup(empty) did not report the stored empty value")
	}
}

func TestConfigurationGetUint32(t *testing.T) {
	config := NewConfiguration()
	tests := []struct {
		name     string
		value    string
		set      bool
		def      uint32
		wantWith uint32
		wantBare uint32
	}{
		{name: "unset", set: false, def: 7, wantWith: 7, wantBare: 0},
		{name: "plain", value: "1000", set: true, def: 7, wantWith: 1000, wantBare: 1000},
		{name: "zero", value: "0", set: true, def: 7, wantWith: 0, wantBare: 0},
		{name: "leading spaces", value: "  42  ", set: true, def: 7, wantWith: 42, wantBare: 42},
		{name: "explicit plus", value: "+42", set: true, def: 7, wantWith: 42, wantBare: 42},
		{name: "numeric prefix", value: "42abc", set: true, def: 7, wantWith: 42, wantBare: 42},
		{name: "unparsable", value: "abc", set: true, def: 7, wantWith: 0, wantBare: 0},
		{name: "empty", value: "", set: true, def: 7, wantWith: 0, wantBare: 0},
		{name: "negative saturates to zero", value: "-1", set: true, def: 7, wantWith: 0, wantBare: 0},
		{
			name:     "uint32 max",
			value:    strconv.FormatUint(math.MaxUint32, 10),
			set:      true,
			def:      7,
			wantWith: math.MaxUint32,
			wantBare: math.MaxUint32,
		},
		{
			name:     "above uint32 saturates",
			value:    strconv.FormatUint(math.MaxUint32+1, 10),
			set:      true,
			def:      7,
			wantWith: math.MaxUint32,
			wantBare: math.MaxUint32,
		},
		{
			name:     "above int64 saturates",
			value:    "99999999999999999999999",
			set:      true,
			def:      7,
			wantWith: math.MaxUint32,
			wantBare: math.MaxUint32,
		},
		{
			name:     "below int64 saturates to zero",
			value:    "-99999999999999999999999",
			set:      true,
			def:      7,
			wantWith: 0,
			wantBare: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key := "key." + test.name
			if test.set {
				config.Set(key, test.value)
			}
			if got := config.GetUint32Or(key, test.def); got != test.wantWith {
				t.Fatalf("GetUint32Or(%q) = %d, want %d", test.value, got, test.wantWith)
			}
			if got := config.GetUint32(key); got != test.wantBare {
				t.Fatalf("GetUint32(%q) = %d, want %d", test.value, got, test.wantBare)
			}
		})
	}
}

func TestConfigurationKeysLenAndClone(t *testing.T) {
	config := NewConfiguration()
	if config.Len() != 0 || config.Keys() != nil {
		t.Fatalf("empty configuration reported %d keys", config.Len())
	}
	config.Set("b", "2")
	config.Set("a", "1")
	keys := config.Keys()
	if len(keys) != 2 || keys[0] != "a" || keys[1] != "b" {
		t.Fatalf("Keys = %v, want sorted [a b]", keys)
	}
	if config.Len() != 2 {
		t.Fatalf("Len = %d, want 2", config.Len())
	}

	clone := config.Clone()
	clone.Set("a", "mutated")
	clone.Set("c", "3")
	if got := config.Get("a"); got != "1" {
		t.Fatalf("clone mutated the original: a = %q", got)
	}
	if _, ok := config.Lookup("c"); ok {
		t.Fatal("clone leaked a new key into the original")
	}
	if got := clone.Get("b"); got != "2" {
		t.Fatalf("clone lost a key: b = %q", got)
	}
}

func TestConfigurationZeroValueIsUsable(t *testing.T) {
	var config Configuration
	if got := config.Get("missing"); got != "" {
		t.Fatalf("Get(missing) = %q, want empty", got)
	}
	config.Set("FILE_SYSTEM_ROOT", "/accumulo")
	if got := config.Get("FILE_SYSTEM_ROOT"); got != "/accumulo" {
		t.Fatalf("Get(FILE_SYSTEM_ROOT) = %q", got)
	}
	clone := config.Clone()
	config.Set("FILE_SYSTEM_ROOT", "/mutated")
	if got := clone.Get("FILE_SYSTEM_ROOT"); got != "/accumulo" {
		t.Fatalf("Clone() did not detach the zero-value configuration: %q", got)
	}
}

func TestConfigurationNilReceiverIsSafe(t *testing.T) {
	var config *Configuration
	config.Set("key", "value")
	if got := config.Get("key"); got != "" {
		t.Fatalf("Get = %q", got)
	}
	if got := config.GetOr("key", "fallback"); got != "fallback" {
		t.Fatalf("GetOr = %q", got)
	}
	if got := config.GetUint32Or("key", 5); got != 5 {
		t.Fatalf("GetUint32Or = %d", got)
	}
	if got := config.GetUint32("key"); got != 0 {
		t.Fatalf("GetUint32 = %d", got)
	}
	if config.Len() != 0 || config.Keys() != nil {
		t.Fatal("nil configuration reported entries")
	}
	if clone := config.Clone(); clone == nil || clone.Len() != 0 {
		t.Fatal("Clone of a nil configuration is not an empty configuration")
	}
}

func TestConfigurationConcurrentUse(t *testing.T) {
	config := NewConfiguration()
	config.Set("seed", "1")

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			key := "worker." + strconv.Itoa(worker)
			for i := 0; i < 200; i++ {
				config.Set(key, strconv.Itoa(i))
				_ = config.Get(key)
				_ = config.GetOr("seed", "")
				_ = config.GetUint32Or(key, 0)
				_ = config.Len()
				_ = config.Keys()
				_ = config.Clone()
			}
		}(worker)
	}
	wg.Wait()

	if config.Len() != 9 {
		t.Fatalf("Len = %d, want 9", config.Len())
	}
}
