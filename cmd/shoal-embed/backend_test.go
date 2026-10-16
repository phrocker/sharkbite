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
		{name: "environment", env: "gcs", root: "bucket/prefix", want: "gcs", wantRoot: "bucket/prefix"},
		{name: "flag overrides environment", env: "gcs", root: "env-root", args: []string{"-backend", "local", "-backend-root", "flag-root"}, want: "local", wantRoot: "flag-root"},
		{name: "explicit empty root", env: "gcs", root: "env-root", args: []string{"-backend-root", ""}, want: "gcs"},
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
	for _, command := range []string{"init", "write", "scan", "compact", "status", "serve", "up", "export", "sync"} {
		t.Run(command, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "unopened")
			args := []string{command, "-data", dir, "-backend", "gcs"}
			switch command {
			case "init", "write", "scan", "compact":
				args = append(args, "-table", "graph")
			case "export", "sync":
				args = append(args, "-table", "graph", "-dst-root", t.TempDir())
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

func TestEngineCommandsRejectMemoryBackend(t *testing.T) {
	t.Setenv("SHOAL_BACKEND", "")
	t.Setenv("SHOAL_BACKEND_ROOT", "")
	dir := filepath.Join(t.TempDir(), "unopened")
	out, err := backendCommand(t, "init", "-data", dir, "-table", "graph", "-backend", "memory").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "in-process only") {
		t.Fatalf("error = %v, output = %s", err, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("engine directory exists or stat failed: %v", err)
	}
}

func TestEngineCommandsRejectUnknownBackend(t *testing.T) {
	t.Setenv("SHOAL_BACKEND", "")
	t.Setenv("SHOAL_BACKEND_ROOT", "")
	out, err := backendCommand(t, "status", "-data", filepath.Join(t.TempDir(), "unopened"), "-backend", "foo", "-backend-root", "x").CombinedOutput()
	if err == nil || !strings.Contains(string(out), `unknown -backend "foo"`) {
		t.Fatalf("error = %v, output = %s", err, out)
	}
}

// A local backend with a root outside -data is the smallest real
// configuration in which -backend-root changes where files land, so every
// engine-opening subcommand is exercised with it, and the write/scan pair
// proves data survives a process boundary.
func TestEngineCommandsHonorBackendRoot(t *testing.T) {
	t.Setenv("SHOAL_BACKEND", "")
	t.Setenv("SHOAL_BACKEND_ROOT", "")
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	root := filepath.Join(base, "objects")
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := backendCommand(t, args...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
		return string(out)
	}
	common := []string{"-data", dir, "-backend", "local", "-backend-root", root}
	run("", append([]string{"init", "-table", "graph"}, common...)...)
	run(`{"row":"r1","entries":[{"cf":"props","cq":"label","value":"node-1"}]}`+"\n",
		append([]string{"write", "-table", "graph"}, common...)...)
	run("", append([]string{"compact", "-table", "graph"}, common...)...)
	if out := run("", append([]string{"scan", "-table", "graph"}, common...)...); !strings.Contains(out, "node-1") {
		t.Fatalf("scan after write in a new process did not return the row: %s", out)
	}
	run("", append([]string{"status"}, common...)...)
	run("", append([]string{"export", "-table", "graph", "-dst-root", filepath.Join(base, "export")}, common...)...)
	run("", append([]string{"sync", "-table", "graph", "-once", "-dst-root", filepath.Join(base, "sync")}, common...)...)

	if n := countFilesWithExt(t, dir, ".rf"); n != 0 {
		t.Fatalf("%d .rf files under -data; tablet files should be under the root", n)
	}
	if n := countFilesWithExt(t, root, ".rf"); n == 0 {
		t.Fatal("no .rf files under -backend-root after write and compact")
	}
	// Reopening without the root must be refused rather than read as empty.
	out, err := backendCommand(t, "scan", "-data", dir, "-table", "graph").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "backend root") {
		t.Fatalf("scan without the root = %v, output = %s", err, out)
	}
	// Env-only configuration reaches a real subcommand.
	cmd := backendCommand(t, "scan", "-data", dir, "-table", "graph")
	cmd.Env = append(cmd.Env, "SHOAL_BACKEND=local", "SHOAL_BACKEND_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "node-1") {
		t.Fatalf("scan via env = %v, output = %s", err, out)
	}
}

func TestServeHonorsBackendRoot(t *testing.T) {
	base := t.TempDir()
	h, err := startServe(serveConfig{
		DataDir: filepath.Join(base, "data"), GRPCAddress: "127.0.0.1:0",
		Backend: backendConfig{Name: "local", Root: filepath.Join(base, "objects")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
