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

package harness_test

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/inference/harness"
)

// A future Copilot-style or other hosted/local agent adapter only implements
// these provider-neutral interfaces; the harness package does not import its
// SDK or require a hosted service.
type providerRunner struct{}
type providerSession struct{}

func (providerRunner) Start(context.Context, harness.SessionRequest) (harness.Session, error) {
	return providerSession{}, nil
}
func (providerSession) Next(context.Context, harness.Transcript) (harness.Action, error) {
	request, _ := harness.NewRetrieveRequest("bounded logical query", 5)
	return harness.NewRetrieveAction("provider-correlation-1", request, harness.Usage{})
}

var _ harness.Runner = providerRunner{}
var _ harness.Session = providerSession{}
