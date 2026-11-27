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

// Package agentprofile describes how a coding agent is started as an ACP agent
// (Profile), and prepares the sandbox its process runs in (Prepare).
package agentprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tailscale/hujson"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
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
type Command = sandboxrt.Command

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
	"opencode": Opencode,
	"claude":   Claude,
	"copilot":  Copilot,
	"agy":      Agy,
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
		return nil, &UnknownAgentError{Name: name, Known: ProfileNames()}
	}
	return mk(), nil
}

// Opencode returns the profile of opencode.
func Opencode() Profile { return opencode{} }

// Claude returns the profile of Claude Code.
func Claude() Profile { return claude{} }

// Copilot returns the profile of GitHub Copilot CLI.
func Copilot() Profile { return copilot{} }

// Agy returns the profile of Antigravity CLI.
func Agy() Profile { return agy{} }

// Custom returns a profile that starts argv, with no image and no home
// setup.
func Custom(name string, argv []string) Profile {
	return custom{name: name, argv: slices.Clone(argv)}
}

// providerKeys are the environment variables that hold the key of an
// opencode provider. opencode reads only the first, which the server also
// sets from the others; see [withProviderKeys].
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

// ProviderKeys returns the environment variables that hold the key of an
// opencode provider, the one opencode reads first, or nil for a provider
// opencode has no key for.
func ProviderKeys(provider string) []string { return slices.Clone(providerKeys[provider]) }

// ClaudeKeys are the variables claude reads its Anthropic credentials from.
func ClaudeKeys() []string { return []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} }

// CopilotKeys are the variables copilot reads its GitHub token from, in the
// order it prefers them.
func CopilotKeys() []string { return []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} }

// AgyKeys are the variables agy reads its Gemini API key from.
func AgyKeys() []string { return []string{"GEMINI_API_KEY"} }

// withProviderKeys returns env with the key of each opencode provider also
// in the variable opencode reads, when env holds it only in another of the
// provider's variables, such as GEMINI_API_KEY.
func withProviderKeys(p Profile, env []string) []string {
	if c, ok := p.(custom); ok && c.base != nil {
		p = c.base
	}
	if _, ok := p.(opencode); !ok {
		return env
	}
	values := map[string]string{}
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok {
			values[name] = value
		}
	}
	out := slices.Clone(env)
	for _, provider := range slices.Sorted(maps.Keys(providerKeys)) {
		keys := providerKeys[provider]
		if _, ok := values[keys[0]]; ok {
			continue
		}
		for _, k := range keys[1:] {
			if v, ok := values[k]; ok {
				out = append(out, keys[0]+"="+v)
				break
			}
		}
	}
	return out
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
	return []string{"OPENCODE_CONFIG_CONTENT=" + string(data)}
}

// opencodeEnv configures opencode on every start.
var opencodeEnv = append([]string{
	"OPENCODE_PERMISSION=" + opencodePermission,
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

// opencodeHomeFile is the config file in the opencode home that the server
// writes.
var opencodeHomeFile = filepath.Join(".config", "opencode", "opencode.json")

func (opencode) Bootstrap(ws Workspace) error {
	var permission map[string]any
	if err := json.Unmarshal([]byte(opencodePermission), &permission); err != nil {
		return err
	}
	return writeJSONIfMissing(ws.Home, opencodeHomeFile, map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"permission": permission,
		"autoupdate": false,
	})
}

// resetPermissions puts the server's permission block back into the
// opencode config of home.
func (opencode) resetPermissions(home string) error {
	var permission map[string]any
	if err := json.Unmarshal([]byte(opencodePermission), &permission); err != nil {
		return err
	}
	return setJSONKey(home, opencodeHomeFile, "permission", permission)
}

type claude struct{}

func (claude) Name() string { return "claude" }

// Image is built on each machine. Anthropic's terms ask whoever preinstalls
// Claude Code to accept its Commercial Terms, so its image isn't published.
func (claude) Image() string { return LocalImage("claude") }

func (claude) Command(ws Workspace) Command {
	return Command{Path: "claude-agent-acp", Dir: ws.Dir}
}

func (claude) ModelEnv(model string) []string { return []string{"ANTHROPIC_MODEL=" + model} }

func (claude) SessionSettings(string) map[string]any { return nil }

// claudeHomeFile is the settings file in the Claude Code home that the
// server writes.
var claudeHomeFile = filepath.Join(".claude", "settings.json")

// claudePermissions are the permission settings of Claude Code that make it
// ask before the tools that change things.
func claudePermissions() map[string]any { return map[string]any{"defaultMode": "default"} }

func (claude) Bootstrap(ws Workspace) error {
	if err := writeJSONIfMissing(ws.Home, claudeHomeFile, map[string]any{
		"permissions": claudePermissions(),
	}); err != nil {
		return err
	}
	// The server passes the project's MCP servers over ACP, leaving out the
	// ones the config turns off. A sandbox hides .mcp.json; this covers a
	// .mcp.json the mask doesn't, such as one replaced while the agent runs.
	return setJSONKey(ws.Home, claudeHomeFile, "enableAllProjectMcpServers", false)
}

// resetPermissions puts the server's permission settings back into the
// Claude Code settings of home.
func (claude) resetPermissions(home string) error {
	return setJSONKey(home, claudeHomeFile, "permissions", claudePermissions())
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
func setJSONKey(home, name, key string, v any) error {
	return setJSONPath(home, name, []string{key}, v)
}

// clearJSON empties the JSON object in the file name in home, which the
// server writes whole. Like [writeJSONIfMissing], it refuses symlinks that
// lead out of home.
func clearJSON(home, name string) (err error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	if _, err := root.Stat(name); err != nil {
		return err
	}
	return fsutil.WriteRegular(root, name, []byte(emptyConfig), 0o600)
}

// setJSONPath sets the key at the end of path in the JSON object in the
// file name in home, as [setJSONKey] does, creating the objects on the way.
func setJSONPath(home, name string, path []string, v any) (err error) {
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
	// opencode reads comments and trailing commas, which a rewrite drops.
	if data, err = hujson.Standardize(data); err != nil {
		return fmt.Errorf("parsing %s: %w", filepath.Join(home, name), err)
	}
	obj := map[string]any{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("parsing %s: %w", filepath.Join(home, name), err)
	}
	want, err := json.Marshal(v)
	if err != nil {
		return err
	}
	parent := obj
	for _, k := range path[:len(path)-1] {
		child, ok := parent[k].(map[string]any)
		if !ok {
			child = map[string]any{}
			parent[k] = child
		}
		parent = child
	}
	key := path[len(path)-1]
	if cur, ok := parent[key]; ok {
		if got, err := json.Marshal(cur); err == nil && bytes.Equal(got, want) {
			return nil
		}
	}
	parent[key] = v
	if data, err = json.MarshalIndent(obj, "", "  "); err != nil {
		return err
	}
	return fsutil.WriteRegular(root, name, append(data, '\n'), 0o600)
}
