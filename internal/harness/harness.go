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

// Package harness describes how a coding agent harness is started as an ACP
// agent (Profile) and where its process runs (Runner).
package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// Workspace is what a harness works on.
type Workspace struct {
	// Dir is the workspace directory. It has the same path on the host and
	// in a sandbox, because ACP sessions are created with it as cwd.
	Dir string
	// Home is the host directory holding the harness home: sessions,
	// caches and config. Runners which isolate the harness mount it as its home.
	Home string
}

// Command describes a harness process to start.
type Command struct {
	Path string
	Args []string
	// Dir is the working directory.
	Dir string
	// Env holds NAME=value pairs added to the environment of the harness.
	Env []string
}

// Profile describes how to start one harness as an ACP agent.
type Profile interface {
	// Name is the harness name, such as opencode.
	Name() string
	// Command returns the command which starts the harness in ACP mode.
	Command(ws Workspace) Command
	// Image is the container image with the harness installed.
	Image() string
	// Bootstrap writes harness config into ws.Home before the first start,
	// so that every tool permission is asked and the server's rules decide.
	Bootstrap(ws Workspace) error
	// ModelEnv returns the NAME=value pairs which select model, or nil for
	// a harness without a known way to select one.
	ModelEnv(model string) []string
	// SessionSettings returns the ACP session settings which select model,
	// or nil for none. Setting the model per session makes an unknown model
	// fail the turn; opencode falls back to its default model when the model
	// of ModelEnv is unknown.
	SessionSettings(model string) map[string]any
}

// ProfileNames lists the known harness profiles.
func ProfileNames() []string { return slices.Sorted(maps.Keys(profiles)) }

var profiles = map[string]func() Profile{
	"opencode": func() Profile { return opencode{} },
	"claude":   func() Profile { return claude{} },
}

// Lookup returns the profile of a harness. A non-empty command replaces the
// profile's command line, which also allows harnesses without a profile.
func Lookup(name string, command []string) (Profile, error) {
	if len(command) > 0 {
		var base Profile
		if mk, ok := profiles[name]; ok {
			base = mk()
		}
		return custom{name: name, argv: command, base: base}, nil
	}
	mk, ok := profiles[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q: known agents are %v, or pass --command for any ACP agent", name, ProfileNames())
	}
	return mk(), nil
}

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

// providerKeys are the environment variables that opencode reads the key of
// a provider from, the one to suggest first.
var providerKeys = map[string][]string{
	"anthropic":  {"ANTHROPIC_API_KEY"},
	"deepseek":   {"DEEPSEEK_API_KEY"},
	"google":     {"GOOGLE_GENERATIVE_AI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"},
	"groq":       {"GROQ_API_KEY"},
	"mistral":    {"MISTRAL_API_KEY"},
	"openai":     {"OPENAI_API_KEY"},
	"openrouter": {"OPENROUTER_API_KEY"},
	"xai":        {"XAI_API_KEY"},
}

// claudeKeys are the variables claude reads its Anthropic credentials
// from.
var claudeKeys = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}

// MissingKey returns how to set the key of the provider of cfg's opencode
// model when cfg passes none of its keys to the agent, or "".
func MissingKey(cfg *agentdir.Config) string {
	provider, key := MissingProviderKey(cfg, cfg.Secrets)
	if provider == "" {
		return ""
	}
	return fmt.Sprintf("no key for provider %s: set one with `a2a-serve config set --secret %s`", provider, key)
}

// MissingProviderKey returns the provider of cfg's opencode model and the
// variable to suggest for its key, when neither the --env values of cfg nor
// the variables secrets hold one of its keys. It returns "" otherwise.
func MissingProviderKey(cfg *agentdir.Config, secrets []string) (provider, key string) {
	provider, _, ok := strings.Cut(cfg.Model, "/")
	keys := providerKeys[provider]
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
	keys := claudeKeys
	provider = "anthropic"
	if cfg.Harness == "opencode" {
		var ok bool
		provider, _, ok = strings.Cut(cfg.Model, "/")
		if keys = providerKeys[provider]; !ok || len(keys) == 0 {
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

// ImageRepo is where harness images are published.
const ImageRepo = "ghcr.io/a2aproject/harness-"

// ImageTag is the tag of the harness images this build uses.
const ImageTag = "latest"

// opencodePermission makes opencode ask before every tool, MCP tools
// included, so that the server's rules decide them, except the tools that
// change nothing. Reads still ask for .env files, as opencode's default
// does. opencode applies the last rule that matches, so "*" comes first.
const opencodePermission = `{"*":"ask","glob":"allow","grep":"allow","list":"allow","lsp":"allow","question":"allow",` +
	`"read":{"*":"allow","*.env":"ask","*.env.*":"ask","*.env.example":"allow"},` +
	`"skill":"allow","task":"allow","todowrite":"allow","websearch":"allow"}`

// opencodeConfigEnv is opencode's inline config, which it reads on every
// start, also in agent homes bootstrapped before a setting existed. It keeps
// opencode working after a tool call is denied, by a rule or by the user, so
// that it can answer another way; by default it ends the turn without an
// answer. A non-empty model overrides the model of opencode.json.
func opencodeConfigEnv(model string) []string {
	cfg := map[string]any{"experimental": map[string]any{"continue_loop_on_deny": true}}
	if model != "" {
		cfg["model"] = model
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		panic(err)
	}
	return []string{"OPENCODE_CONFIG_CONTENT=" + string(data), "CLOUDCODE_CONFIG_CONTENT=" + string(data)}
}

// opencodeEnv configures opencode on every start.
var opencodeEnv = append([]string{
	"OPENCODE_PERMISSION=" + opencodePermission,
	"CLOUDCODE_PERMISSION=" + opencodePermission,
}, opencodeConfigEnv("")...)

type opencode struct{}

func (opencode) Name() string  { return "opencode" }
func (opencode) Image() string { return ImageRepo + "opencode:" + ImageTag }

func (opencode) Command(ws Workspace) Command {
	return Command{
		Path: "opencode",
		Args: []string{"acp"},
		Dir:  ws.Dir,
		Env:  slices.Clone(opencodeEnv),
	}
}

// ModelEnv sets the model in opencode's inline config, which overrides the
// model of opencode.json in the harness home.
func (opencode) ModelEnv(model string) []string { return opencodeConfigEnv(model) }

func (opencode) SessionSettings(model string) map[string]any {
	if model == "" {
		return nil
	}
	return map[string]any{"model": model}
}

func (opencode) Bootstrap(ws Workspace) error {
	var permission map[string]any
	if err := json.Unmarshal([]byte(opencodePermission), &permission); err != nil {
		return err
	}
	return writeJSONIfMissing(ws.Home, filepath.Join(".config", "opencode", "opencode.json"), map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"permission": permission,
		"autoupdate": false,
	})
}

type claude struct{}

func (claude) Name() string  { return "claude" }
func (claude) Image() string { return ImageRepo + "claude:" + ImageTag }

func (claude) Command(ws Workspace) Command {
	return Command{Path: "claude-agent-acp", Dir: ws.Dir}
}

func (claude) ModelEnv(model string) []string { return []string{"ANTHROPIC_MODEL=" + model} }

func (claude) SessionSettings(string) map[string]any { return nil }

func (claude) Bootstrap(ws Workspace) error {
	settings := filepath.Join(".claude", "settings.json")
	if err := writeJSONIfMissing(ws.Home, settings, map[string]any{
		"permissions": map[string]any{"defaultMode": "default"},
	}); err != nil {
		return err
	}
	// The server passes the project's MCP servers over ACP, leaving out the
	// ones the config turns off. A sandbox hides .mcp.json; this covers a
	// .mcp.json the mask doesn't, such as one replaced while the agent runs.
	return setJSONKey(ws.Home, settings, "enableAllProjectMcpServers", false)
}

// custom is a harness started with an explicit command line. It keeps the
// image and bootstrap of the profile it replaces, if any.
type custom struct {
	name string
	argv []string
	base Profile
}

func (c custom) Name() string { return c.name }

func (c custom) Image() string {
	if c.base != nil {
		return c.base.Image()
	}
	return ""
}

func (c custom) Command(ws Workspace) Command {
	cmd := Command{Path: c.argv[0], Args: slices.Clone(c.argv[1:]), Dir: ws.Dir}
	if c.base != nil {
		cmd.Env = c.base.Command(ws).Env
	}
	return cmd
}

func (c custom) ModelEnv(model string) []string {
	if c.base != nil {
		return c.base.ModelEnv(model)
	}
	return nil
}

func (c custom) SessionSettings(model string) map[string]any {
	if c.base != nil {
		return c.base.SessionSettings(model)
	}
	return nil
}

func (c custom) Bootstrap(ws Workspace) error {
	if c.base != nil {
		return c.base.Bootstrap(ws)
	}
	return nil
}

// writeJSONIfMissing writes the file name in home, keeping config the user
// changed after the first start. The harness can change its home, so
// symlinks that lead out of it are refused.
func writeJSONIfMissing(home, name string, v any) (err error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	_, err = root.Stat(name)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteRegular(root, name, append(data, '\n'), 0o600)
}

// setJSONKey sets key in the JSON object in the file name in home, keeping
// the other keys. Like [writeJSONIfMissing], it refuses symlinks that lead
// out of home. It skips a file that isn't regular or is too large to read.
func setJSONKey(home, name, key string, v any) (err error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	data, err := fsutil.ReadRegular(root, name)
	if errors.Is(err, fsutil.ErrNotRegular) || errors.Is(err, fsutil.ErrTooLarge) {
		slog.Warn("skipped a settings file the server can't read", "file", filepath.Join(home, name), "error", err)
		return nil
	}
	if err != nil {
		return err
	}
	obj := map[string]any{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("parsing %s: %w", filepath.Join(home, name), err)
	}
	if cur, ok := obj[key]; ok && cur == v {
		return nil
	}
	obj[key] = v
	if data, err = json.MarshalIndent(obj, "", "  "); err != nil {
		return err
	}
	return fsutil.WriteRegular(root, name, append(data, '\n'), 0o600)
}
