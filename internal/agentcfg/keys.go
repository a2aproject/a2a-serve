// Copyright 2026 The A2A Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agentcfg

import (
	"fmt"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/core/agent"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// Warnings returns the problems of cfg that leave the agent working, but not
// as the user likely meant.
func Warnings(cfg *agentdir.Config) []string {
	var out []string
	if cfg.Harness == "opencode" && cfg.Model == "" && slices.Contains(cfg.Secrets, "OPENROUTER_API_KEY") {
		out = append(out, "opencode doesn't use OPENROUTER_API_KEY without an OpenRouter model; pick one with `a2a-serve config set --model openrouter/<model>`, such as openrouter/~google/gemini-flash-latest")
	}
	if hint := MissingKey(cfg); hint != "" {
		out = append(out, hint)
	}
	return out
}

// providerNames are how the output names the model providers.
var providerNames = map[string]string{
	"anthropic": "Anthropic", "deepseek": "DeepSeek", "google": "Google", "groq": "Groq",
	"mistral": "Mistral", "openai": "OpenAI", "openrouter": "OpenRouter", "xai": "xAI",
}

// ProviderName returns how the output names a model provider.
func ProviderName(provider string) string {
	if n, ok := providerNames[provider]; ok {
		return n
	}
	return provider
}

// MissingKey returns how to set the key of the provider of cfg's opencode
// model when cfg passes none of its keys to the agent, or "".
func MissingKey(cfg *agentdir.Config) string {
	provider, key := MissingProviderKey(cfg, cfg.Secrets)
	if provider == "" {
		return ""
	}
	return fmt.Sprintf("no key for provider %s: set one with `a2a-serve config set --secret %s --restart`", provider, key)
}

// MissingProviderKey returns the provider of cfg's opencode model and the
// variable to suggest for its key, when neither the --env values of cfg nor
// the variables secrets hold one of its keys. It returns "" otherwise.
func MissingProviderKey(cfg *agentdir.Config, secrets []string) (provider, key string) {
	provider, _, ok := strings.Cut(cfg.Model, "/")
	keys := agent.ProviderKeys(provider)
	if cfg.Harness != "opencode" || !ok || len(keys) == 0 || hasAny(cfg, secrets, keys) {
		return "", ""
	}
	return provider, keys[0]
}

// ModelKeyWithheld returns the model provider and the variable of its key
// when the agent gets one from cfg, but a container that gets only the
// secrets granted has none. It returns "" otherwise. It knows the keys of
// opencode's providers and of claude.
func ModelKeyWithheld(cfg *agentdir.Config, granted []string) (provider, key string) {
	keys := agent.ClaudeKeys()
	provider = "anthropic"
	if cfg.Harness == "opencode" {
		var ok bool
		provider, _, ok = strings.Cut(cfg.Model, "/")
		if keys = agent.ProviderKeys(provider); !ok || len(keys) == 0 {
			return "", ""
		}
	} else if cfg.Harness != "claude" {
		return "", ""
	}
	if !hasAny(cfg, cfg.Secrets, keys) || hasAny(cfg, granted, keys) {
		return "", ""
	}
	for _, k := range keys {
		if slices.Contains(cfg.Secrets, k) {
			return provider, k
		}
	}
	return provider, keys[0]
}

// hasAny reports whether the --env values of cfg or the variables secrets
// hold one of keys.
func hasAny(cfg *agentdir.Config, secrets, keys []string) bool {
	for _, kv := range cfg.Env {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(keys, name) {
			return true
		}
	}
	return slices.ContainsFunc(secrets, func(name string) bool { return slices.Contains(keys, name) })
}
