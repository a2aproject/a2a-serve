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
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
)

// copilotEnv configures GitHub Copilot CLI on every start: the image or the
// user's install decides its version, and its own telemetry stays off.
var copilotEnv = []string{"COPILOT_AUTO_UPDATE=false", "COPILOT_TELEMETRY_DISABLED=true"}

type copilot struct{}

func (copilot) Name() string  { return "copilot" }
func (copilot) Image() string { return ImageRepo + "copilot:" + ImageTag }

func (copilot) Command(ws Workspace) Command {
	return Command{Path: "copilot", Args: []string{"--acp"}, Dir: ws.Dir, Env: slices.Clone(copilotEnv)}
}

func (copilot) ModelEnv(model string) []string { return []string{"COPILOT_MODEL=" + model} }

func (copilot) SessionSettings(model string) map[string]any {
	if model == "" {
		return nil
	}
	return map[string]any{"model": model}
}

// Copilot's files in its home that the server writes.
var (
	copilotSettingsFile  = filepath.Join(".copilot", "settings.json")
	copilotMCPFile       = filepath.Join(".copilot", "mcp-config.json")
	copilotApprovalsFile = filepath.Join(".copilot", "permissions-config.json")
	copilotStateFile     = filepath.Join(".copilot", "config.json")
)

// Bootstrap writes a list of MCP servers with none, which a locked home
// keeps; Copilot reports an error for an empty file. Copilot asks before
// file edits, URLs, MCP tools and the shell commands that aren't read-only
// by default, and reads no permission rules from its home.
func (copilot) Bootstrap(ws Workspace) error {
	return writeJSONIfMissing(ws.Home, copilotMCPFile, map[string]any{"mcpServers": map[string]any{}})
}

// resetPermissions drops what makes Copilot run tools without asking, which
// the agent could have added to its home before the home was locked: the
// URLs it fetches, its hooks, the approvals it saved, and the folders it
// trusts, whose configuration it loads.
func (copilot) resetPermissions(home string) error {
	return errors.Join(
		setJSONKey(home, copilotSettingsFile, "allowedUrls", []string{}),
		setJSONKey(home, copilotSettingsFile, "hooks", map[string]any{}),
		setJSONKey(home, copilotApprovalsFile, "locations", map[string]any{}),
		setJSONKey(home, copilotStateFile, "trustedFolders", []string{}),
	)
}

// copilotPolicyDir is the folder where Copilot reads its managed settings,
// which take precedence over every other source. Copilot reads the file only
// when root owns it.
const copilotPolicyDir = "/etc/github-copilot"

const copilotPolicyFile = "managed-settings.json"

func (copilot) policyPath() (string, string) { return copilotPolicyDir, copilotPolicyFile }
func (copilot) policyInImage() bool          { return true }

// policy makes Copilot run no hooks but its own, since a repository's hooks
// run also in folders it doesn't trust, and keeps a caller's slash command,
// such as /allow-all, from turning its permission prompts off. A
// permissions block with rules would make Copilot ask before every read, so
// the rules of p stay with the server.
func (copilot) policy(Policy) ([]byte, error) {
	data, err := json.MarshalIndent(map[string]any{
		"allowManagedHooksOnly": true,
		"permissions": map[string]any{
			"disableBypassPermissionsMode":   "disable",
			"disableAssistedPermissionsMode": true,
		},
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
