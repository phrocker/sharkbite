package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSharedLibraryCABI(t *testing.T) {
	if goEnv(t, "CGO_ENABLED") != "1" {
		t.Skip("C ABI requires CGO_ENABLED=1")
	}
	cc := compilerCommand(t, "CC")
	cxx := compilerCommand(t, "CXX")
	root := repositoryRoot(t)
	artifacts, err := os.MkdirTemp(root, ".shoal-cabi-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(artifacts)

	libraryName := "libshoal.so"
	executableName := "lifecycle"
	if runtime.GOOS == "windows" {
		libraryName = "shoal.dll"
		executableName += ".exe"
	} else if runtime.GOOS == "darwin" {
		libraryName = "libshoal.dylib"
	}
	library := filepath.Join(artifacts, libraryName)
	runCommand(
		t,
		root,
		nil,
		"go",
		"build",
		"-tags=shoal_capi_test",
		"-buildmode=c-shared",
		"-o",
		library,
		"./cmd/shoal-capi",
	)

	include := filepath.Join(root, "capi", "include")
	cppExecutable := filepath.Join(artifacts, "header_cpp_test")
	if runtime.GOOS == "windows" {
		cppExecutable += ".exe"
	}
	cppArgs := append(
		append([]string{}, cxx.args...),
		"-std=c++11", "-Wall", "-Wextra", "-Werror",
		"-I", include,
		filepath.Join(root, "capi", "tests", "header_cpp_test.cpp"),
		library,
		"-o", cppExecutable,
	)
	runCommand(t, root, nil, cxx.name, cppArgs...)

	executable := filepath.Join(artifacts, executableName)
	cArgs := append(
		append([]string{}, cc.args...),
		"-std=c11", "-Wall", "-Wextra", "-Werror",
		"-I", include,
		"-I", filepath.Join(root, "capi", "tests"),
		filepath.Join(root, "capi", "tests", "lifecycle.c"),
		library,
		"-o", executable,
	)
	runCommand(t, root, nil, cc.name, cArgs...)

	env := append([]string{}, os.Environ()...)
	switch runtime.GOOS {
	case "windows":
		env = prependEnvPath(env, "PATH", artifacts)
	case "darwin":
		env = prependEnvPath(env, "DYLD_LIBRARY_PATH", artifacts)
	default:
		env = prependEnvPath(env, "LD_LIBRARY_PATH", artifacts)
	}
	runCommand(t, artifacts, env, cppExecutable)
	runCommand(t, artifacts, env, executable)

	queryExecutable := filepath.Join(artifacts, "shared_library_query")
	if runtime.GOOS == "windows" {
		queryExecutable += ".exe"
	}
	queryArgs := append(
		append([]string{}, cc.args...),
		"-std=c11", "-Wall", "-Wextra", "-Werror",
		"-I", include,
		filepath.Join(root, "capi", "tests", "shared_library_query.c"),
	)
	if runtime.GOOS == "linux" {
		queryArgs = append(queryArgs, "-ldl")
	}
	queryArgs = append(queryArgs, "-o", queryExecutable)
	runCommand(t, root, nil, cc.name, queryArgs...)
	runCommand(t, artifacts, env, queryExecutable, library)

	linkEveryDeclaredSymbol(t, root, artifacts, env, cc, include, library)

	bridgeExecutable := filepath.Join(artifacts, "result_bridge")
	if runtime.GOOS == "windows" {
		bridgeExecutable += ".exe"
	}
	bridgeArgs := append(
		append([]string{}, cc.args...),
		"-std=c11", "-Wall", "-Wextra", "-Werror", "-DSHOAL_CAPI_TEST",
		"-I", include,
		"-I", filepath.Join(root, "cmd", "shoal-capi"),
		filepath.Join(root, "capi", "tests", "result_bridge.c"),
		filepath.Join(root, "cmd", "shoal-capi", "bridge.c"),
		"-o", bridgeExecutable,
	)
	runCommand(t, root, nil, cc.name, bridgeArgs...)
	runCommand(t, artifacts, nil, bridgeExecutable)

	python, err := exec.LookPath("python")
	if err != nil {
		t.Skipf("python is unavailable: %v", err)
	}
	pythonEnv := append([]string{}, env...)
	pythonEnv = prependEnvPath(
		pythonEnv,
		"PYTHONPATH",
		filepath.Join(root, "python", "src"),
	)
	pythonEnv = append(pythonEnv, "SHOAL_LIBRARY="+library)
	runCommand(
		t,
		root,
		pythonEnv,
		python,
		"-m",
		"unittest",
		"python.tests.test_cross_cutting",
	)
}

func linkEveryDeclaredSymbol(
	t *testing.T,
	root string,
	artifacts string,
	env []string,
	cc command,
	include string,
	library string,
) {
	t.Helper()
	executable := filepath.Join(artifacts, "all_symbols")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	args := append(
		append([]string{}, cc.args...),
		"-std=c11", "-Wall", "-Wextra", "-Werror",
		"-I", include,
		filepath.Join(root, "capi", "tests", "all_symbols.c"),
		library,
		"-o", executable,
	)
	runCommand(t, root, nil, cc.name, args...)
	runCommand(t, artifacts, env, executable)
}

type command struct {
	name string
	args []string
}

func compilerCommand(t *testing.T, variable string) command {
	t.Helper()
	fields := strings.Fields(goEnv(t, variable))
	if len(fields) == 0 {
		t.Fatalf("go env %s returned an empty command", variable)
	}
	path, err := exec.LookPath(fields[0])
	if err != nil {
		t.Skipf("%s compiler %q is unavailable: %v", variable, fields[0], err)
	}
	return command{name: path, args: fields[1:]}
}

func goEnv(t *testing.T, name string) string {
	t.Helper()
	output, err := exec.Command("go", "env", name).CombinedOutput()
	if err != nil {
		t.Fatalf("go env %s: %v\n%s", name, err, output)
	}
	return strings.TrimSpace(string(output))
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func runCommand(t *testing.T, directory string, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = directory
	if env != nil {
		cmd.Env = env
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
}

func prependEnvPath(env []string, name, value string) []string {
	prefix := name + "="
	for index, entry := range env {
		if strings.HasPrefix(strings.ToUpper(entry), strings.ToUpper(prefix)) {
			env[index] = fmt.Sprintf("%s=%s%c%s", name, value, os.PathListSeparator, entry[len(prefix):])
			return env
		}
	}
	return append(env, name+"="+value)
}
