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
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
)

type chatModelConfig struct {
	provider       string
	model          string
	baseURL        string
	apiKeyEnv      string
	organization   string
	project        string
	retrievalModes []retrieval.Mode
}

func newChatProviders(
	ctx context.Context,
	client *authorized.Client,
	resolver auth.Resolver,
	config chatModelConfig,
) (webapi.AskProvider, webapi.InteractionProvider, error) {
	if client == nil {
		return nil, nil, errors.New("chat requires an authorized Explorer client")
	}
	generator, provider, modelName, err := chatTextGenerator(ctx, config)
	if err != nil {
		return nil, nil, err
	}
	provenance, err := inference.NewModelProvenance(
		provider, modelName, "", nil, nil)
	if err != nil {
		return nil, nil, err
	}
	chat, err := webapi.NewChatService(ctx, webapi.ChatConfig{
		Client: client, Resolver: resolver, Generator: generator, Model: provenance,
		RetrievalModes: config.retrievalModes,
	})
	if err != nil {
		return nil, nil, err
	}
	interactions, err := webapi.NewInteractionService(client)
	if err != nil {
		return nil, nil, err
	}
	return chat, interactions, nil
}

func chatRetrievalModes(embedder model.Embedder) []retrieval.Mode {
	modes := []retrieval.Mode{retrieval.ModeLexical}
	if embedder != nil {
		modes = append(modes, retrieval.ModeVector)
	}
	return append(modes, retrieval.ModeTree)
}

func chatTextGenerator(
	ctx context.Context, config chatModelConfig,
) (model.TextGenerator, string, string, error) {
	provider := strings.ToLower(strings.TrimSpace(config.provider))
	name := strings.TrimSpace(config.model)
	switch provider {
	case "ollama":
		if name == "" {
			name = firstNonEmpty(
				os.Getenv("SHOAL_OLLAMA_MODEL"),
				model.DefaultOllamaGenerateModel,
			)
		}
		generator, err := model.NewOllamaGenerator(model.OllamaConfig{
			BaseURL: config.baseURL, Model: name,
		})
		if err != nil {
			return nil, "", "", fmt.Errorf("configure chat provider ollama: %w", err)
		}
		return generator, "ollama", name, ctx.Err()
	case "openai", "openai-compatible":
		if name == "" {
			name = firstNonEmpty(
				os.Getenv("SHOAL_OPENAI_MODEL"), os.Getenv("OPENAI_MODEL"))
		}
		if strings.TrimSpace(config.baseURL) == "" || name == "" {
			return nil, "", "", errors.New(
				"chat provider openai-compatible requires a base URL and model")
		}
		keyEnv := strings.TrimSpace(config.apiKeyEnv)
		if keyEnv == "" {
			return nil, "", "", errors.New(
				"chat provider openai-compatible requires an API key environment name")
		}
		generator, err := model.NewOpenAIGenerator(model.OpenAIConfig{
			BaseURL: config.baseURL, GenerationModel: name,
			Organization: config.organization, Project: config.project,
			Credentials: model.CredentialResolverFunc(func(context.Context) ([]byte, error) {
				value := os.Getenv(keyEnv)
				if value == "" && keyEnv == "SHOAL_OPENAI_API_KEY" {
					value = os.Getenv("OPENAI_API_KEY")
				}
				if value == "" {
					return nil, model.ErrCredential
				}
				return []byte(value), nil
			}),
		})
		if err != nil {
			return nil, "", "", fmt.Errorf(
				"configure chat provider openai-compatible: %w", err)
		}
		return generator, "openai-compatible", name, ctx.Err()
	default:
		return nil, "", "", fmt.Errorf(
			"unknown chat provider %q; use ollama or openai-compatible",
			config.provider,
		)
	}
}
