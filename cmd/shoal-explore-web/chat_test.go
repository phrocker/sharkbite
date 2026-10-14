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

package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
)

func TestChatTextGeneratorUsesOnlyProductionProviders(t *testing.T) {
	if _, _, _, err := chatTextGenerator(
		context.Background(), chatModelConfig{provider: "fake"},
	); err == nil {
		t.Fatal("fake chat provider was accepted")
	}
	if _, _, _, err := chatTextGenerator(
		context.Background(),
		chatModelConfig{
			provider: "openai-compatible",
			baseURL:  "https://model.example.test",
			model:    "production-model",
		},
	); err == nil {
		t.Fatal("openai-compatible provider without credential environment was accepted")
	}
	generator, provider, modelName, err := chatTextGenerator(
		context.Background(),
		chatModelConfig{
			provider:  "openai-compatible",
			baseURL:   "https://model.example.test",
			model:     "production-model",
			apiKeyEnv: "SHOAL_TEST_CHAT_KEY",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if generator == nil || provider != "openai-compatible" ||
		modelName != "production-model" {
		t.Fatalf("openai-compatible provider = %T %q %q",
			generator, provider, modelName)
	}
}

func TestChatRetrievalModesUseConfiguredVectorPath(t *testing.T) {
	withoutVector := chatRetrievalModes(nil)
	if want := []retrieval.Mode{
		retrieval.ModeLexical, retrieval.ModeTree,
	}; !reflect.DeepEqual(withoutVector, want) {
		t.Fatalf("without vector = %v, want %v", withoutVector, want)
	}
	withVector := chatRetrievalModes(model.FakeEmbedder{})
	if want := []retrieval.Mode{
		retrieval.ModeLexical, retrieval.ModeVector, retrieval.ModeTree,
	}; !reflect.DeepEqual(withVector, want) {
		t.Fatalf("with vector = %v, want %v", withVector, want)
	}
}
