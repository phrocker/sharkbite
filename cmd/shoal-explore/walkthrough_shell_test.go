//go:build !windows

package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDocumentedDemoWalkthroughShell executes the bash blocks of
// docs/explorer-demo-walkthrough.md, in order, against a temporary corpus.
// TestDocumentedDemoWorkflow covers the CLI calls in-process; this test
// covers the shell text a Linux or macOS reader actually pastes, including
// the jq pipelines, the web workspace and its curl calls, and the MCP step.
//
// The blocks are grouped by what they talk to. Everything before the block
// that starts shoal-explore-web runs as one script, so the shell variables it
// defines carry across steps. The server block runs in the background; blocks
// that address its listen address run while it is up; the remaining blocks
// run after it has been stopped, as the walkthrough instructs.
func TestDocumentedDemoWalkthroughShell(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three commands and runs the whole walkthrough")
	}
	for _, tool := range []string{"bash", "jq", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed: %v", tool, err)
		}
	}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(root, "docs", "explorer-demo-walkthrough.md"))
	if err != nil {
		t.Fatal(err)
	}
	bash := fencedBlocks(string(source), "bash")
	powershell := fencedBlocks(string(source), "powershell")
	if len(bash) == 0 || len(bash) != len(powershell) {
		t.Fatalf("walkthrough has %d bash blocks and %d powershell blocks; every step shows both",
			len(bash), len(powershell))
	}

	bin := t.TempDir()
	build := exec.Command("go", "build", "-buildvcs=false", "-o", bin+string(filepath.Separator),
		"./cmd/shoal-explore", "./cmd/shoal-explore-web", "./cmd/shoal-mcp")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build demo commands: %v\n%s", err, out)
	}

	data := filepath.Join(t.TempDir(), "explorer-demo")
	listen := freeLoopbackAddress(t)
	// Longest command names first, so "shoal-explore" does not clip
	// "shoal-explore-web".
	rewrite := strings.NewReplacer(
		"go run ./cmd/shoal-explore-web", filepath.Join(bin, "shoal-explore-web"),
		"go run ./cmd/shoal-explore", filepath.Join(bin, "shoal-explore"),
		"go run ./cmd/shoal-mcp", filepath.Join(bin, "shoal-mcp"),
		".shoal/explorer-demo", data,
		"127.0.0.1:8080", listen,
	)

	var before, during, after []string
	server := ""
	for _, block := range bash {
		block = rewrite.Replace(block)
		switch {
		case strings.Contains(block, "shoal-explore-web"):
			if server != "" {
				t.Fatal("walkthrough starts the web workspace twice")
			}
			server = block
		case server == "":
			before = append(before, block)
		case strings.Contains(block, listen):
			during = append(during, block)
		default:
			after = append(after, block)
		}
	}
	if server == "" || len(before) == 0 || len(during) == 0 || len(after) == 0 {
		t.Fatalf("walkthrough shape: %d before, server %t, %d during, %d after",
			len(before), server != "", len(during), len(after))
	}

	cli := runWalkthroughScript(t, root, before)
	for _, want := range []string{
		`"Disposition": "applied"`,
		`"Disposition": "unchanged"`,
		`"Title": "Shoal Explorer Demo Guide"`,
		`"Title": "Explorer Release Checklist"`,
		`"Heading": "Ingested knowledge"`,
		`"Heading": "Promotion gate"`,
		`"Heading": "Relationship target"`,
		"Run the canary validation before promotion",
		`"Answer": "grounded"`,
		`"StopReason": "stop"`,
		`"ID": "demo-edge-guide-supports-release"`,
		`"Type": "supports"`,
	} {
		if !strings.Contains(cli, want) {
			t.Errorf("CLI steps did not print %q", want)
		}
	}
	if ids := regexp.MustCompile(`(?m)^doc_[0-9a-f]+$`).FindAllString(cli, -1); len(ids) != 2 {
		t.Errorf("step 4 printed %d document IDs, want 2: %q", len(ids), ids)
	}

	serverOutput := startWalkthroughServer(t, root, server, "http://"+listen+"/api/v1/meta")
	api := runWalkthroughScript(t, root, during)
	stopWalkthroughServer(t)
	for _, want := range []string{
		"Authenticating every request as development principal",
		"Granted 2 pre-existing document(s)",
		"listening at http://" + listen,
	} {
		if !strings.Contains(serverOutput.String(), want) {
			t.Errorf("web workspace did not print %q in:\n%s", want, serverOutput.String())
		}
	}
	for _, want := range []string{`"max_page_size"`, `"max_upload_total_bytes"`, `"snapshot"`, `"frontier"`} {
		if !strings.Contains(api, want) {
			t.Errorf("web API steps did not print %q", want)
		}
	}

	mcp := runWalkthroughScript(t, root, after)
	responses := strings.Split(strings.TrimSpace(mcp), "\n")
	if len(responses) != 3 {
		t.Fatalf("MCP step printed %d lines, want 3 JSON-RPC responses:\n%s", len(responses), mcp)
	}
	for _, want := range []string{`"shoal.documents"`, `"shoal.retrieve"`, `"shoal.neighborhood"`, `"shoal.ingest"`} {
		if !strings.Contains(responses[1], want) {
			t.Errorf("MCP tools/list did not advertise %s", want)
		}
	}
	if !strings.Contains(responses[2], `"documents"`) || strings.Contains(responses[2], `"isError":true`) {
		t.Errorf("MCP shoal.documents call failed: %s", responses[2])
	}
}

func fencedBlocks(markdown, language string) []string {
	var blocks []string
	open := "```" + language
	inBlock := false
	var current strings.Builder
	for _, line := range strings.Split(markdown, "\n") {
		switch {
		case !inBlock && strings.TrimRight(line, " ") == open:
			inBlock = true
			current.Reset()
		case inBlock && strings.TrimRight(line, " ") == "```":
			inBlock = false
			blocks = append(blocks, current.String())
		case inBlock:
			current.WriteString(line)
			current.WriteString("\n")
		}
	}
	return blocks
}

func runWalkthroughScript(t *testing.T, root string, blocks []string) string {
	t.Helper()
	script := "set -euo pipefail\n" + strings.Join(blocks, "\n")
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("walkthrough script failed: %v\n--- script ---\n%s\n--- stdout ---\n%s\n--- stderr ---\n%s",
			err, script, stdout.String(), stderr.String())
	}
	return stdout.String()
}

var walkthroughServer *exec.Cmd

func startWalkthroughServer(t *testing.T, root, block, readyURL string) *bytes.Buffer {
	t.Helper()
	cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+block)
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output := &bytes.Buffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start web workspace: %v", err)
	}
	walkthroughServer = cmd
	t.Cleanup(stopWalkthroughServerQuietly)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(readyURL)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return output
			}
		}
		if cmd.ProcessState != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("web workspace never answered %s:\n%s", readyURL, output.String())
	return nil
}

func stopWalkthroughServer(t *testing.T) {
	t.Helper()
	stopWalkthroughServerQuietly()
}

func stopWalkthroughServerQuietly() {
	cmd := walkthroughServer
	if cmd == nil || cmd.Process == nil {
		return
	}
	walkthroughServer = nil
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return fmt.Sprintf("127.0.0.1:%d", port)
}
