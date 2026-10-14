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

// Package shoal defines values shared by Shoal's public knowledge APIs.
package shoal

// ID is a stable, caller-visible opaque byte string. Shoal does not generate,
// normalize, or require UTF-8 for IDs at the public value layer.
type ID string

// Metadata contains application-defined attributes. Shoal treats keys and
// values as opaque.
type Metadata map[string]string

// Score is a comparable relevance or relationship score. Higher values are
// more relevant unless a specific operation documents otherwise.
type Score float64
