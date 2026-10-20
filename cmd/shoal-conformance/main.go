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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/phrocker/shoal-oss/internal/conformance"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		mode        = flag.String("mode", "replay", "verdict mode: replay or live")
		root        = flag.String("repo-root", "", "repository root (default: current directory)")
		required    = flag.String("required", strings.Join(conformance.Roles, ","), "comma-separated required gates")
		output      = flag.String("output", "-", "JSON output path, or - for stdout")
		liveLogDir  = flag.String("live-log-dir", "", "directory for raw live role output (optional)")
		liveTimeout = flag.Duration("live-timeout", 0, "maximum duration for a live verdict (0 means no limit)")
	)
	flag.Parse()
	if *liveTimeout < 0 {
		fmt.Fprintln(os.Stderr, "live-timeout must be nonnegative")
		return 1
	}

	if *root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		*root = cwd
	}
	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var requiredRoles []string
	for _, role := range strings.Split(*required, ",") {
		if role = strings.TrimSpace(role); role != "" {
			requiredRoles = append(requiredRoles, role)
		}
	}
	ctx := context.Background()
	if *liveTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *liveTimeout)
		defer cancel()
	}
	verdict, exitCode, err := conformance.Run(ctx, conformance.Options{
		Root:       absoluteRoot,
		Mode:       *mode,
		Required:   requiredRoles,
		LiveLogDir: *liveLogDir,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	data, err := conformance.Marshal(verdict)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *output == "-" {
		_, err = os.Stdout.Write(data)
	} else {
		err = os.WriteFile(*output, data, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return exitCode
}
