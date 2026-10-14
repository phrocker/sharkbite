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

package gatewaycmd

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
)

// unrecorded is `shoal-gateway unrecorded list|ack`. Both open the log, which
// takes the directory's lock: the log's only writer is the gateway that holds
// it, so a list beside a running gateway could show what it is about to
// change, and an ack beside one would rewrite the file under a writer that
// keeps its own copy in memory and would write the entry back. Both refuse
// while a gateway runs on the directory (docs/effects-gateway-deploy.md:
// "the ack opens the log, so it runs against a stopped gateway's directory").
func unrecorded(args []string, env Env) int {
	if len(args) == 0 {
		return failf(env, ExitUsage, "unrecorded needs list or ack\n\n%s", usage)
	}
	switch args[0] {
	case "list":
		return listUnrecorded(args[1:], env)
	case "ack":
		return ackUnrecorded(args[1:], env)
	}
	return failf(env, ExitUsage, "unknown unrecorded command %q\n\n%s", args[0], usage)
}

// parseInterleaved parses flags wherever they appear among the positional
// arguments, so `ack ID -unrecorded-dir D` and `ack -unrecorded-dir D ID`
// mean the same thing.
func parseInterleaved(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		if flags.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, flags.Arg(0))
		args = flags.Args()[1:]
	}
}

func openForOperator(dir string, env Env) (*effectsgateway.UnrecordedLog, int) {
	if strings.TrimSpace(dir) == "" {
		return nil, failf(env, ExitUsage, "-unrecorded-dir is required")
	}
	// An operator command never creates the directory: a mistyped path must
	// be an error, not an empty log that says nothing awaits reconciliation.
	// Only run creates it.
	log, err := effectsgateway.OpenExistingUnrecordedLog(dir, env.Clock.Now)
	if errors.Is(err, effectsgateway.ErrUnrecordedDirMissing) {
		return nil, failf(env, ExitFailure, "-unrecorded-dir %s is not an existing directory; "+
			"name the directory the gateway runs with", dir)
	}
	if errors.Is(err, effectsgateway.ErrGatewayLocked) {
		return nil, failf(env, ExitFailure, "a gateway is running on %s; stop it first, "+
			"because the running gateway owns the log and would write back what this changes", dir)
	}
	if err != nil {
		return nil, failf(env, ExitFailure, "-unrecorded-dir: %v", err)
	}
	return log, ExitOK
}

// listedEntry is what list prints: the closed fields only. The target and
// the reference are left out — the reference is text the target returned,
// however tightly the route's pattern bounds it — and so is the
// correlation ID; has_reference says whether the log holds one.
type listedEntry struct {
	ID            string    `json:"id"`
	ActionID      string    `json:"action_id"`
	Fence         uint64    `json:"fence"`
	ClaimNonce    string    `json:"claim_nonce,omitempty"`
	RouteAction   string    `json:"route_action"`
	Method        string    `json:"method"`
	PathTemplate  string    `json:"path_template"`
	Outcome       string    `json:"outcome"`
	HasReference  bool      `json:"has_reference"`
	Status        int       `json:"status,omitempty"`
	DispatchError string    `json:"dispatch_error,omitempty"`
	FirstAt       time.Time `json:"first_at"`
	LastAt        time.Time `json:"last_at"`
	Attempts      int       `json:"attempts"`
}

// entryID is how list names an entry and ack takes it: the action ID, a
// colon, and the fence. The colon is outside base64url, so it is unambiguous.
func entryID(entry effectsgateway.UnrecordedEntry) string {
	return base64.RawURLEncoding.EncodeToString(entry.ActionID) + ":" +
		strconv.FormatUint(entry.Fence, 10)
}

func listUnrecorded(args []string, env Env) int {
	flags := flag.NewFlagSet("shoal-gateway unrecorded list", flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	dir := flags.String("unrecorded-dir", "", "The gateway's unrecorded directory. Required")
	positional, err := parseInterleaved(flags, args)
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	if err != nil {
		return ExitUsage
	}
	if len(positional) != 0 {
		return failf(env, ExitUsage, "unexpected argument %q", positional[0])
	}
	log, code := openForOperator(*dir, env)
	if log == nil {
		return code
	}
	defer log.Close()
	encoder := json.NewEncoder(env.Stdout)
	encoder.SetEscapeHTML(false)
	for _, entry := range log.Entries() {
		listed := listedEntry{
			ID:       entryID(entry),
			ActionID: base64.RawURLEncoding.EncodeToString(entry.ActionID),
			Fence:    entry.Fence, RouteAction: entry.RouteAction, Method: entry.Method,
			PathTemplate: entry.PathTemplate, Outcome: string(entry.Outcome),
			HasReference: entry.Reference != "", Status: entry.Status,
			DispatchError: string(entry.DispatchError),
			FirstAt:       entry.FirstAt.UTC(), LastAt: entry.LastAt.UTC(), Attempts: entry.Attempts,
		}
		if entry.ClaimNonce != (effectsgateway.ClaimNonce{}) {
			listed.ClaimNonce = hex.EncodeToString(entry.ClaimNonce[:])
		}
		if err := encoder.Encode(listed); err != nil {
			return failf(env, ExitFailure, "writing the list failed")
		}
	}
	return ExitOK
}

type ackTarget struct {
	actionID []byte
	fence    uint64
	hasFence bool
	raw      string
}

func parseAckTarget(raw string) (ackTarget, error) {
	target := ackTarget{raw: raw}
	encoded, fence, hasFence := strings.Cut(raw, ":")
	id, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(id) == 0 {
		return ackTarget{}, fmt.Errorf("%q: the action ID must be unpadded base64url, "+
			"as list prints it", raw)
	}
	target.actionID = id
	if hasFence {
		value, err := strconv.ParseUint(fence, 10, 64)
		if err != nil || value == 0 {
			return ackTarget{}, fmt.Errorf("%q: the fence must be a positive integer", raw)
		}
		target.fence, target.hasFence = value, true
	}
	return target, nil
}

// ackUnrecorded is the explicit clearing: the operator has reconciled these
// reports by other means. Every named entry must exist, and a bare action ID
// must name exactly one, before anything is removed.
func ackUnrecorded(args []string, env Env) int {
	flags := flag.NewFlagSet("shoal-gateway unrecorded ack", flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	dir := flags.String("unrecorded-dir", "", "The gateway's unrecorded directory. Required")
	all := flags.Bool("all", false, "Acknowledge every held report")
	positional, err := parseInterleaved(flags, args)
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	if err != nil {
		return ExitUsage
	}
	if *all == (len(positional) > 0) {
		return failf(env, ExitUsage, "ack takes ACTION_ID[:FENCE]... or -all, not both and not neither")
	}
	targets := make([]ackTarget, 0, len(positional))
	for _, raw := range positional {
		target, err := parseAckTarget(raw)
		if err != nil {
			return failf(env, ExitUsage, "%v", err)
		}
		targets = append(targets, target)
	}
	log, code := openForOperator(*dir, env)
	if log == nil {
		return code
	}
	defer log.Close()

	entries := log.Entries()
	var chosen []effectsgateway.UnrecordedEntry
	if *all {
		chosen = entries
	}
	for _, target := range targets {
		var matched []effectsgateway.UnrecordedEntry
		for _, entry := range entries {
			if bytes.Equal(entry.ActionID, target.actionID) &&
				(!target.hasFence || entry.Fence == target.fence) {
				matched = append(matched, entry)
			}
		}
		switch {
		case len(matched) == 0:
			return failf(env, ExitFailure, "%s: no held report; nothing was acknowledged", target.raw)
		case len(matched) > 1:
			return failf(env, ExitFailure, "%s: %d reports under different fences; name one "+
				"as ACTION_ID:FENCE; nothing was acknowledged", target.raw, len(matched))
		}
		chosen = append(chosen, matched[0])
	}

	// One durable rewrite for all of them, or none: a failure part way never
	// leaves some named reports cleared and others not.
	keys := make([]effectsgateway.UnrecordedKey, 0, len(chosen))
	named := map[string]bool{}
	unique := chosen[:0:0]
	for _, entry := range chosen {
		if id := entryID(entry); !named[id] {
			named[id] = true
			unique = append(unique, entry)
			keys = append(keys, effectsgateway.UnrecordedKey{ActionID: entry.ActionID, Fence: entry.Fence})
		}
	}
	if err := log.AckAll(keys); err != nil {
		return failf(env, ExitFailure, "%v; nothing was acknowledged", err)
	}
	logger := effectsgateway.NewLogger(env.Stdout, env.Clock.Now)
	remaining := log.Len()
	for _, entry := range unique {
		logger.Log(effectsgateway.LogRecord{
			Event: effectsgateway.EventUnrecordedCleared, ActionID: entry.ActionID,
			Fence: entry.Fence, ClaimNonce: entry.ClaimNonce, Unrecorded: remaining,
		})
	}
	return ExitOK
}
