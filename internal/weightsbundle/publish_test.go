// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package weightsbundle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

func TestPublishLocalNeverReplacesAndIsAtomic(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("no-replace rename is linux only")
	}
	root, m := stageBundle(t, svmFiles, svmRoles)
	digest, err := Stage(root, m)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), digest)
	if err := PublishLocal(root, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging directory still present after publication")
	}
	if _, got, err := Load(dest); err != nil || got != digest {
		t.Fatalf("published bundle: %v %s", err, got)
	}
	again, m2 := stageBundle(t, svmFiles, svmRoles)
	if _, err := Stage(again, m2); err != nil {
		t.Fatal(err)
	}
	if err := PublishLocal(again, dest); !errors.Is(err, ErrExists) {
		t.Fatalf("second publication: %v", err)
	}
	if _, err := os.Stat(again); err != nil {
		t.Fatal("losing staging directory was consumed")
	}
	unstaged, _ := stageBundle(t, svmFiles, svmRoles)
	if err := PublishLocal(unstaged, filepath.Join(t.TempDir(), "x")); !errors.Is(err, ErrVerification) {
		t.Fatalf("unstaged bundle published: %v", err)
	}
	// Anything other than the same bundle at the destination is a
	// verification failure naming the path, never "already published".
	third, m3 := stageBundle(t, svmFiles, svmRoles)
	if _, err := Stage(third, m3); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(t.TempDir(), digest)
	if err := os.MkdirAll(junk, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(junk, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PublishLocal(third, junk); !errors.Is(err, ErrVerification) || errors.Is(err, ErrExists) {
		t.Fatalf("junk at destination: %v", err)
	}
	dangling := filepath.Join(t.TempDir(), "dangling")
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), dangling); err == nil {
		if err := PublishLocal(third, dangling); !errors.Is(err, ErrVerification) {
			t.Fatalf("dangling symlink at destination: %v", err)
		}
	}
}

func TestPublishToBackendIsVisibleOnlyWhenComplete(t *testing.T) {
	ctx := context.Background()
	root, m := stageBundle(t, svmFiles, svmRoles)
	digest, err := Stage(root, m)
	if err != nil {
		t.Fatal(err)
	}
	backend := memory.New()
	got, err := Publish(ctx, backend, "bundles", root)
	if err != nil || got != digest {
		t.Fatalf("%v %s", err, got)
	}
	if _, err := Publish(ctx, backend, "bundles", root); !errors.Is(err, ErrExists) {
		t.Fatalf("republish: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "fetched")
	fetched, err := Fetch(ctx, backend, "bundles", digest, dest)
	if err != nil {
		t.Fatal(err)
	}
	if fd, _ := fetched.Digest(); fd != digest {
		t.Fatal("fetched digest differs")
	}
	if _, _, err := Load(dest); err != nil {
		t.Fatal(err)
	}

	// A partial upload (member present, manifest absent) is not a bundle.
	partial := memory.New()
	partial.Put("bundles/"+digest+"/model.json", []byte(svmFiles["model.json"]))
	if _, err := Fetch(ctx, partial, "bundles", digest, filepath.Join(t.TempDir(), "p")); !errors.Is(err, ErrVerification) {
		t.Fatalf("partial bundle fetched: %v", err)
	}
	// A member altered after publication fails verification and leaves no bundle.
	backend.Put("bundles/"+digest+"/runtime.json", []byte("{}"))
	target := filepath.Join(t.TempDir(), "altered")
	if _, err := Fetch(ctx, backend, "bundles", digest, target); !errors.Is(err, ErrVerification) {
		t.Fatalf("altered member fetched: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("altered fetch left a bundle behind")
	}
	// A manifest whose digest is not the requested one is refused.
	if _, err := Fetch(ctx, backend, "bundles", "0"+digest[1:], filepath.Join(t.TempDir(), "q")); !errors.Is(err, ErrVerification) {
		t.Fatalf("mismatched digest fetched: %v", err)
	}
}
