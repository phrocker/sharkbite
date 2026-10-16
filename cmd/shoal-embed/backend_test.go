package main

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
