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

package explorer

import "testing"

func TestValidateStrictJSONStringEncoding(t *testing.T) {
	valid := [][]byte{
		[]byte(`{"value":"plain"}`),
		[]byte(`{"value":"🙂"}`),
		[]byte(`{"value":"\ud83d\ude42"}`),
	}
	for _, data := range valid {
		if err := validateStrictJSONStringEncoding(data); err != nil {
			t.Fatalf("valid JSON string %q: %v", data, err)
		}
	}

	invalidUTF8 := append([]byte(`{"value":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	invalid := [][]byte{
		invalidUTF8,
		[]byte(`{"value":"\ud83d"}`),
		[]byte(`{"value":"\ude42"}`),
	}
	for _, data := range invalid {
		if err := validateStrictJSONStringEncoding(data); err == nil {
			t.Fatalf("invalid JSON string %q was accepted", data)
		}
	}
}
