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
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
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
		out = append(out, "opencode doesn't use OPENROUTER_API_KEY without an OpenRouter model; pick one with `a2a-serve config set --model openrouter/<model>`, such as openrouter/google/gemini-flash-latest")
	}
	return out
}

// ImageRepo is where harness images are published.
const ImageRepo = "ghcr.io/a2aproject/harness-"

// ImageTag is the tag of the harness images this build uses.
const ImageTag = "latest"

// opencodePermission makes opencode ask before tools that change things,
// instead of its default of approving them.
const opencodePermission = `{"bash":"ask","edit":"ask","webfetch":"ask"}`

type opencode struct{}

func (opencode) Name() string  { return "opencode" }
func (opencode) Image() string { return ImageRepo + "opencode:" + ImageTag }

func (opencode) Command(ws Workspace) Command {
	return Command{
		Path: "opencode",
		Args: []string{"acp"},
		Dir:  ws.Dir,
		Env:  []string{"OPENCODE_PERMISSION=" + opencodePermission, "CLOUDCODE_PERMISSION=" + opencodePermission},
	}
}

// ModelEnv sets the model in inline config, which overrides the model of
// opencode.json in the harness home.
func (opencode) ModelEnv(model string) []string {
	data, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		panic(err)
	}
	return []string{"OPENCODE_CONFIG_CONTENT=" + string(data), "CLOUDCODE_CONFIG_CONTENT=" + string(data)}
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
	return root.WriteFile(name, append(data, '\n'), 0o600)
}

// setJSONKey sets key in the JSON object in the file name in home, keeping
// the other keys. Like [writeJSONIfMissing], it refuses symlinks that lead
// out of home.
func setJSONKey(home, name, key string, v any) (err error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	data, err := root.ReadFile(name)
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
	return root.WriteFile(name, append(data, '\n'), 0o600)
}
