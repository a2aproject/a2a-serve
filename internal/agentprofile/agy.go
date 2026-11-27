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

package agentprofile

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// agyEnv configures Antigravity CLI on every start: the image or the user's
// install decides its version.
var agyEnv = []string{"AGY_CLI_DISABLE_AUTO_UPDATE=true"}

type agy struct{}

func (agy) Name() string { return "agy" }

// Image is built on each machine; the project doesn't publish an
// Antigravity image.
func (agy) Image() string { return LocalImage("agy") }

func (agy) Command(ws Workspace) Command {
	return Command{Path: "agy", Args: []string{"acp", "--disable-telemetry"}, Dir: ws.Dir, Env: slices.Clone(agyEnv)}
}

// ModelEnv returns nil: Antigravity CLI takes its model only per session.
func (agy) ModelEnv(string) []string { return nil }

func (agy) SessionSettings(model string) map[string]any {
	if model == "" {
		return nil
	}
	return map[string]any{"model": model}
}

// Antigravity CLI's files in its home that the server writes.
var (
	agySettingsFile = filepath.Join(".gemini", "antigravity-cli", "settings.json")
	agyStateFile    = filepath.Join(".gemini", "config", "config.json")
	agyProjectsDir  = filepath.Join(".gemini", "config", "projects")
	agyHooksFiles   = []string{filepath.Join(".gemini", "config", "hooks.json"), filepath.Join(".gemini", "antigravity-cli", "hooks.json")}
)

// agyPermissions makes Antigravity CLI ask before every file write, which
// it otherwise makes in the workspace without asking. It asks before
// commands, URLs, MCP tools and reads outside the workspace by default.
func agyPermissions() map[string]any {
	return map[string]any{"ask": []string{"write_file(*)"}}
}

// Bootstrap makes Antigravity CLI use the Gemini API key of the agent's
// environment, since Google's terms don't allow a signed-in account in
// other tools, and keeps the modes that run tools without asking off.
func (agy) Bootstrap(ws Workspace) error {
	return writeJSONIfMissing(ws.Home, agySettingsFile, map[string]any{
		"modelProvider":  "gemini",
		"toolPermission": "request-review",
		"agentMode":      "",
		"permissions":    agyPermissions(),
	})
}

// resetPermissions drops what makes Antigravity CLI run tools without
// asking, which the agent could have added to its home before the home was
// locked: the modes and rules of its settings, its hooks, which can approve
// a call, and the permissions it granted for all projects and for each.
func (agy) resetPermissions(home string) error {
	errs := []error{
		setJSONKey(home, agySettingsFile, "permissions", agyPermissions()),
		setJSONKey(home, agySettingsFile, "toolPermission", "request-review"),
		setJSONKey(home, agySettingsFile, "agentMode", ""),
		setJSONKey(home, agySettingsFile, "allowNonWorkspaceAccess", false),
		setJSONPath(home, agyStateFile, []string{"userSettings", "globalPermissionGrants"}, map[string]any{}),
		resetProjectGrants(home),
	}
	for _, f := range agyHooksFiles {
		errs = append(errs, clearJSON(home, f))
	}
	return errors.Join(errs...)
}

// resetProjectGrants drops the permissions that Antigravity CLI saved for
// each project.
func resetProjectGrants(home string) (err error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	entries, err := fsutil.ReadDirIn(root, agyProjectsDir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".json") {
			errs = append(errs, setJSONKey(home, filepath.Join(agyProjectsDir, e.Name()), "permissionGrants", map[string]any{}))
		}
	}
	return errors.Join(errs...)
}
