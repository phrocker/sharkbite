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

package explorerfleetevents

import (
	"testing"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
)

func TestConfigureRuntimeAddsEventTableOnce(t *testing.T) {
	config := explorercoord.Config{
		PhysicalTables: []string{"existing"},
	}
	ConfigureRuntime(&config)
	ConfigureRuntime(&config)
	count := 0
	for _, table := range config.PhysicalTables {
		if table == Table {
			count++
		}
	}
	if count != 1 || config.PhysicalTables[0] != "existing" {
		t.Fatalf("physical tables = %#v", config.PhysicalTables)
	}
}
