package main

import (
	"context"
	"flag"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/storage"
)

func TestBackendFlags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      string
		root     string
		args     []string
		want     string
		wantRoot string
	}{
		{name: "default", want: "local"},
		{name: "environment", env: "memory", root: "bucket/prefix", want: "memory", wantRoot: "bucket/prefix"},
		{name: "flag overrides environment", env: "memory", root: "env-root", args: []string{"-backend", "local", "-backend-root", "flag-root"}, want: "local", wantRoot: "flag-root"},
		{name: "explicit empty root", env: "memory", root: "env-root", args: []string{"-backend-root", ""}, want: "memory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SHOAL_BACKEND", tc.env)
			t.Setenv("SHOAL_BACKEND_ROOT", tc.root)
			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			cfg := registerBackendFlags(flags)
			if err := flags.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if cfg.Name != tc.want || cfg.Root != tc.wantRoot {
				t.Fatalf("config = %+v, want backend %q root %q", cfg, tc.want, tc.wantRoot)
			}
		})
	}
}

func TestBackendCommandProcess(t *testing.T) {
	if os.Getenv("SHOAL_BACKEND_TEST_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"shoal-embed"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	t.Fatal("missing command arguments")
}

func backendCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestBackendCommandProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "SHOAL_BACKEND_TEST_PROCESS=1")
	return cmd
}

func TestEngineCommandsRequireBackendRoot(t *testing.T) {
	t.Setenv("SHOAL_BACKEND", "")
	t.Setenv("SHOAL_BACKEND_ROOT", "")
	for _, command := range []string{"init", "write", "scan", "compact", "status", "serve", "up", "export", "import", "sync"} {
		t.Run(command, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "unopened")
			args := []string{command, "-data", dir, "-backend", "gcs"}
			switch command {
			case "init", "write", "scan", "compact":
				args = append(args, "-table", "graph")
			case "export", "sync":
				args = append(args, "-table", "graph", "-dst-root", t.TempDir())
			case "import":
				args = append(args, "-manifest", "gs://bucket/manifest.json")
			}
			out, err := backendCommand(t, args...).CombinedOutput()
			if err == nil || !strings.Contains(string(out), "-backend-root") || !strings.Contains(string(out), "SHOAL_BACKEND_ROOT") {
				t.Fatalf("error = %v, output = %s", err, out)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("engine directory exists or stat failed: %v", err)
			}
		})
	}
}

func TestEngineCommandsAcceptMemoryBackend(t *testing.T) {
	t.Setenv("SHOAL_BACKEND", "")
	t.Setenv("SHOAL_BACKEND_ROOT", "")
	for _, command := range []string{"init", "write", "scan", "compact", "status", "export", "sync"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			if command != "init" {
				cmdInit([]string{"-data", dir, "-table", "graph"})
			}
			args := []string{command, "-data", dir, "-backend", "memory"}
			if command != "status" {
				args = append(args, "-table", "graph")
			}
			if command == "export" || command == "sync" {
				args = append(args, "-dst-root", t.TempDir())
			}
			if command == "sync" {
				args = append(args, "-once")
			}
			if out, err := backendCommand(t, args...).CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
		})
	}
}

func TestWriteBackendPlacement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       string
		flags     []string
		wantFiles bool
	}{
		{name: "default local", wantFiles: true},
		{name: "memory flag", flags: []string{"-backend", "memory"}},
		{name: "memory environment", env: "memory"},
		{name: "local flag overrides environment", env: "memory", flags: []string{"-backend", "local"}, wantFiles: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SHOAL_BACKEND", tc.env)
			t.Setenv("SHOAL_BACKEND_ROOT", "")
			for _, format := range []string{"rfile", "parquet"} {
				t.Run(format, func(t *testing.T) {
					dir := t.TempDir()
					cmdInit(append([]string{"-data", dir, "-table", "graph", "-format", format}, tc.flags...))
					cmd := backendCommand(t, append([]string{"write", "-data", dir, "-table", "graph"}, tc.flags...)...)
					cmd.Stdin = strings.NewReader("{\"row\":\"row\",\"entries\":[{\"cf\":\"cf\",\"cq\":\"cq\",\"value\":\"value\"}]}\n")
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("%v: %s", err, out)
					}
					var immutable, wal, tableManifest, tabletManifest bool
					err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
						if err != nil {
							return err
						}
						immutable = immutable || storage.ImmutableFormatForPath(path) != ""
						wal = wal || entry.Name() == "wal.log"
						tableManifest = tableManifest || entry.Name() == "table.json"
						tabletManifest = tabletManifest || entry.Name() == "files.json"
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					if immutable != tc.wantFiles || !wal || !tableManifest || !tabletManifest {
						t.Fatalf("immutable=%v (want %v), WAL=%v, table manifest=%v, tablet manifest=%v", immutable, tc.wantFiles, wal, tableManifest, tabletManifest)
					}
				})
			}
		})
	}
}

func TestBackendRootFlushAndReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := backendConfig{Name: "memory", Root: "gs://bucket/prefix"}
	be, cleanup, err := cfg.open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	eng, err := engine.Open(dir, engine.Options{Backend: be})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	mutation, err := cclient.NewMutation([]byte("row"))
	if err != nil {
		t.Fatal(err)
	}
	mutation.Put([]byte("cf"), []byte("cq"), nil, 1, []byte("value"))
	if err := eng.Write("graph", []*cclient.Mutation{mutation}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}
	adapter := be.(*engineBackend)
	keys, err := adapter.backend.(storage.Lister).List(ctx, cfg.Root+"/graph/t-0000")
	if err != nil || len(keys) != 1 || !strings.HasSuffix(keys[0], ".rf") {
		t.Fatalf("rooted objects = %v, error = %v", keys, err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := engine.Open(dir, engine.Options{Backend: be})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	scan, err := reopened.Scan("graph", iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer scan.Close()
	if !scan.Next() || string(scan.Value()) != "value" {
		t.Fatal("flushed value not readable after reopen")
	}
	if err := reopened.Compact("graph", nil); err != nil {
		t.Fatal(err)
	}
}

func TestServeMemoryBackend(t *testing.T) {
	h, err := startServe(serveConfig{
		DataDir: t.TempDir(), GRPCAddress: "127.0.0.1:0",
		Backend: backendConfig{Name: "memory"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBackendExternalPaths(t *testing.T) {
	ctx := context.Background()
	be, cleanup, err := (backendConfig{Name: "memory", Root: "gs://bucket/prefix"}).open(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	adapter := be.(*engineBackend)
	for _, path := range []string{"gs://imports/manifest.json", "gs://imports/table/t-0000/F1.rf", "hdfs:/imports/F2.rf"} {
		if err := storage.WriteAll(ctx, adapter.backend, path, []byte("source")); err != nil {
			t.Fatal(err)
		}
		data, err := storage.ReadAll(ctx, be, path)
		if err != nil || string(data) != "source" {
			t.Fatalf("external path %s: data=%q, error=%v", path, data, err)
		}
	}
}
