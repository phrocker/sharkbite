//go:build (linux && !android) || (darwin && !ios)

package local

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestReconcilePlatformXattrsMatchesTargetSet(t *testing.T) {
	attrs := map[string]map[string][]byte{
		"target": {"user.keep": []byte("target")},
		"temp": {
			"user.keep":      []byte("inherited"),
			"user.inherited": []byte("remove"),
		},
	}
	ops := mapXattrOperations(attrs)

	if err := reconcilePlatformXattrs("temp", "target", ops); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(attrs["temp"], attrs["target"]) {
		t.Fatalf("temporary xattrs = %v, want exact target set %v", attrs["temp"], attrs["target"])
	}
}

func TestReconcilePlatformXattrsReportsRemovalFailure(t *testing.T) {
	removeErr := errors.New("remove denied")
	ops := xattrOperations{
		list: func(path string) ([]string, error) {
			if path == "temp" {
				return []string{"user.inherited"}, nil
			}
			return nil, nil
		},
		get:    func(string, string) ([]byte, error) { return nil, nil },
		set:    func(string, string, []byte, int) error { return nil },
		remove: func(string, string) error { return removeErr },
	}

	err := reconcilePlatformXattrs("temp", "target", ops)
	if !errors.Is(err, removeErr) || !strings.Contains(err.Error(), "remove inherited extended attribute") {
		t.Fatalf("error = %v, want explicit inherited-attribute removal failure", err)
	}
}

func TestReconcilePlatformXattrsDoesNotRestoreContentSecurityLabels(t *testing.T) {
	attrs := map[string]map[string][]byte{
		"target": {
			"user.keep":           []byte("target"),
			"security.capability": []byte("old-capability"),
			"security.ima":        []byte("old-signature"),
			"security.evm":        []byte("old-integrity"),
		},
		"temp": {
			"user.keep":           []byte("inherited"),
			"security.capability": []byte("inherited-capability"),
			"security.ima":        []byte("inherited-signature"),
			"security.evm":        []byte("inherited-integrity"),
			"user.inherited":      []byte("remove"),
		},
	}

	if err := reconcilePlatformXattrs("temp", "target", mapXattrOperations(attrs)); err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{"user.keep": []byte("target")}
	if !reflect.DeepEqual(attrs["temp"], want) {
		t.Fatalf("temporary xattrs = %v, want content-safe set %v", attrs["temp"], want)
	}
}

func TestReconcilePlatformXattrsSkipsSettingMatchingValue(t *testing.T) {
	setCalls := 0
	ops := xattrOperations{
		list: func(path string) ([]string, error) {
			if path == "target" || path == "temp" {
				return []string{"security.selinux"}, nil
			}
			return nil, nil
		},
		get: func(path, name string) ([]byte, error) {
			if name != "security.selinux" {
				t.Fatalf("unexpected xattr lookup %s for %s", name, path)
			}
			return []byte("system_u:object_r:tmp_t:s0"), nil
		},
		set: func(string, string, []byte, int) error {
			setCalls++
			return errors.New("unexpected relabel")
		},
		remove: func(string, string) error { return nil },
	}

	if err := reconcilePlatformXattrs("temp", "target", ops); err != nil {
		t.Fatal(err)
	}
	if setCalls != 0 {
		t.Fatalf("Setxattr calls = %d, want 0 when value already matches", setCalls)
	}
}

func mapXattrOperations(attrs map[string]map[string][]byte) xattrOperations {
	return xattrOperations{
		list: func(path string) ([]string, error) {
			var names []string
			for name := range attrs[path] {
				names = append(names, name)
			}
			return names, nil
		},
		get: func(path, name string) ([]byte, error) {
			return append([]byte(nil), attrs[path][name]...), nil
		},
		set: func(path, name string, value []byte, _ int) error {
			attrs[path][name] = append([]byte(nil), value...)
			return nil
		},
		remove: func(path, name string) error {
			delete(attrs[path], name)
			return nil
		},
	}
}
