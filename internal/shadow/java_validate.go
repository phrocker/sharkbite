// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package shadow

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// EnvJavaRFileValidate is the env-var name. When set, runJavaValidator
// drops shoal's output bytes to a tempfile, $RFILE-substitutes the value,
// shells out via the platform shell (sh -c on POSIX, PowerShell or cmd on
// Windows), and reports the exit code. Reuses the SHOAL_JAVA_RFILE_VALIDATE
// convention from the C0 parity harness so CI / dev environments only need
// one variable set.
//
// Suggested values:
//
//	# accumulo CLI wrapper:
//	export SHOAL_JAVA_RFILE_VALIDATE='accumulo rfile-info $RFILE'
//
//	# Maven exec (from the Accumulo repo root):
//	export SHOAL_JAVA_RFILE_VALIDATE='cd /mnt/ExtraDrive/repos/accumulo && \
//	    mvn -q -pl core exec:java \
//	    -Dexec.mainClass=org.apache.accumulo.core.file.rfile.PrintInfo \
//	    -Dexec.args=$RFILE'
const EnvJavaRFileValidate = "SHOAL_JAVA_RFILE_VALIDATE"

// javaValidateTimeout bounds how long we'll wait for the external Java
// validator. PrintInfo on a 100MB RFile takes ~5s warm; 30s is generous
// without being so long that a hung JVM blocks the oracle loop.
const javaValidateTimeout = 30 * time.Second

// runJavaValidator is T1: shell out to a Java RFile reader and report
// whether it accepts shoal's bytes. When EnvJavaRFileValidate is unset,
// returns {Attempted: false} — the caller treats that as "T1 skipped",
// not "T1 failed".
func runJavaValidator(shoalBytes []byte) T1Result {
	cmdTemplate := strings.TrimSpace(os.Getenv(EnvJavaRFileValidate))
	if cmdTemplate == "" {
		return T1Result{Attempted: false}
	}
	dir, err := os.MkdirTemp("", "shoal-shadow-t1-")
	if err != nil {
		return T1Result{Attempted: true, Passed: false,
			Error: fmt.Sprintf("mkdir tempdir: %v", err), CommandUsed: cmdTemplate}
	}
	defer os.RemoveAll(dir)
	rfilePath := filepath.Join(dir, "shoal-output.rf")
	if err := os.WriteFile(rfilePath, shoalBytes, 0o600); err != nil {
		return T1Result{Attempted: true, Passed: false,
			Error: fmt.Sprintf("write rfile: %v", err), CommandUsed: cmdTemplate}
	}
	cmdLine := strings.ReplaceAll(cmdTemplate, "$RFILE", rfilePath)

	ctx, cancel := context.WithTimeout(context.Background(), javaValidateTimeout)
	defer cancel()
	cmd := validatorShellCommand(ctx, cmdLine)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err = cmd.Run()
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		combined := stderr.String()
		if combined == "" {
			combined = stdout.String()
		}
		// Truncate noisy stack traces so the report line stays readable.
		if len(combined) > 4000 {
			combined = combined[:4000] + "...[truncated]"
		}
		return T1Result{
			Attempted:   true,
			Passed:      false,
			Error:       fmt.Sprintf("validator exit: %v\n%s", err, combined),
			CommandUsed: cmdLine,
			elapsedMs:   elapsed,
		}
	}
	return T1Result{Attempted: true, Passed: true, CommandUsed: cmdLine, elapsedMs: elapsed}
}

func validatorShellCommand(ctx context.Context, cmdLine string) *exec.Cmd {
	shell, args := validatorShell()
	return exec.CommandContext(ctx, shell, append(args, cmdLine)...)
}

func validatorShell() (string, []string) {
	if runtime.GOOS != "windows" {
		return "sh", []string{"-c"}
	}
	if _, err := exec.LookPath("powershell"); err == nil {
		return "powershell", []string{"-NoProfile", "-NonInteractive", "-Command"}
	}
	return "cmd", []string{"/C"}
}
