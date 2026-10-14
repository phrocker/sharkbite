package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSharedLibraryTestSeamExports(t *testing.T) {
	if goEnv(t, "CGO_ENABLED") != "1" {
		t.Skip("shared-library export checks require CGO_ENABLED=1")
	}
	root := repositoryRoot(t)
	artifacts, err := os.MkdirTemp(root, ".shoal-capi-exports-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(artifacts)

	libraryExtension := ".so"
	if runtime.GOOS == "windows" {
		libraryExtension = ".dll"
	} else if runtime.GOOS == "darwin" {
		libraryExtension = ".dylib"
	}
	productionLibrary := filepath.Join(artifacts, "libshoal-production"+libraryExtension)
	testLibrary := filepath.Join(artifacts, "libshoal-test"+libraryExtension)

	runCommand(
		t,
		root,
		nil,
		"go",
		"build",
		"-buildmode=c-shared",
		"-o",
		productionLibrary,
		"./cmd/shoal-capi",
	)
	runCommand(
		t,
		root,
		nil,
		"go",
		"build",
		"-tags=shoal_capi_test",
		"-buildmode=c-shared",
		"-o",
		testLibrary,
		"./cmd/shoal-capi",
	)
	checkSharedLibraryTestSeamExports(t, productionLibrary, false)
	checkSharedLibraryTestSeamExports(t, testLibrary, true)
}

func checkSharedLibraryTestSeamExports(t *testing.T, library string, wantPresent bool) {
	t.Helper()
	output, err := runCommandOutput("go", "tool", "nm", library)
	if err != nil {
		t.Fatalf("go tool nm %s: %v\n%s", library, err, output)
	}
	seamSymbols := []string{
		"shoal_bridge_test_string_alloc_fail_after",
		"shoal_bridge_test_string_alloc_reset",
		"shoal_bridge_test_result_alloc_fail_after",
		"shoal_bridge_test_result_alloc_reset",
		"shoal_bridge_test_error_alloc_fail_after",
		"shoal_bridge_test_error_alloc_reset",
		"shoal_bridge_test_error_message_alloc_fail_after",
		"shoal_bridge_test_error_message_alloc_reset",
	}
	text := string(output)
	for _, symbol := range seamSymbols {
		found := strings.Contains(text, " "+symbol+"\n")
		if found != wantPresent {
			t.Fatalf("symbol %s presence = %v in %s, want %v", symbol, found, library, wantPresent)
		}
	}
}

func runCommandOutput(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	return cmd.CombinedOutput()
}
