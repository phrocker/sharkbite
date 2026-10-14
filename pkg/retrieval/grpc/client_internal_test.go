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

package retrievalgrpc

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestClientRetrieveDefensiveGuard(t *testing.T) {
	tests := []struct {
		name   string
		client *client
	}{
		{name: "nil receiver"},
		{name: "missing generated client", client: &client{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.client.Retrieve(
				context.Background(), retrieval.Request{Text: "query"})
			if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
				t.Fatalf("error = %v, want internal", err)
			}
		})
	}
}
