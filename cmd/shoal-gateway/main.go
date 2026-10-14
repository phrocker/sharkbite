// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

// Command shoal-gateway is the effects gateway (#391): an HTTP Path A worker
// that pulls actions off a Shoal explorer's fleet dispatch queue, claims each
// under a fence, performs one HTTP request against a configured operational
// surface, and reports what happened. See docs/effects-gateway-deploy.md.
//
//	shoal-gateway run [flags]
//	shoal-gateway unrecorded list -unrecorded-dir DIR
//	shoal-gateway unrecorded ack -unrecorded-dir DIR (ACTION_ID[:FENCE]... | -all)
//	shoal-gateway grace-period [-operation-timeout T] [-plane-timeout P]
//
// The composition lives in internal/effectsgateway/gatewaycmd, so the
// end-to-end test can run it against the real explorer in-process.
package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/phrocker/shoal-oss/internal/effectsgateway/gatewaycmd"
)

func main() {
	// Buffered for two: the first signal drains, the second is a hard stop,
	// and neither may be dropped while the other is being handled.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	os.Exit(gatewaycmd.Main(os.Args[1:], gatewaycmd.Env{
		Stdout: os.Stdout, Stderr: os.Stderr, Signals: signals,
	}))
}
