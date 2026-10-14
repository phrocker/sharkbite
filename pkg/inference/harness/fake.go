/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package harness

import (
	"context"
	"errors"
	"sync"
)

// ScriptStep produces one deterministic action or fault from the transcript
// visible at that step.
type ScriptStep func(context.Context, Transcript) (Action, error)

func ScriptAction(action Action) ScriptStep {
	return func(context.Context, Transcript) (Action, error) { return action, nil }
}

func ScriptFault(err error) ScriptStep {
	return func(context.Context, Transcript) (Action, error) { return Action{}, err }
}

// FakeRunner is a deterministic, concurrency-safe scripted Runner.
type FakeRunner struct {
	steps []ScriptStep
}

func NewFakeRunner(steps ...ScriptStep) *FakeRunner {
	return &FakeRunner{steps: append([]ScriptStep(nil), steps...)}
}

func (r *FakeRunner) Start(context.Context, SessionRequest) (Session, error) {
	if r == nil {
		return nil, errors.New("nil fake runner")
	}
	return &fakeSession{steps: append([]ScriptStep(nil), r.steps...)}, nil
}

type fakeSession struct {
	mu    sync.Mutex
	steps []ScriptStep
	next  int
}

func (s *fakeSession) Next(ctx context.Context, transcript Transcript) (Action, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.steps) {
		return Action{}, errors.New("fake runner script exhausted")
	}
	step := s.steps[s.next]
	s.next++
	return step(ctx, cloneTranscript(transcript))
}
