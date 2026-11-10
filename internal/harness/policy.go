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

package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// claudePolicyDir is the folder where Claude Code reads its managed
// settings, which take precedence over every other source.
const claudePolicyDir = "/etc/claude-code"

const claudePolicyFile = "managed-settings.json"

// claudeAskTools are the tools of Claude Code that change files or reach
// the network, which it asks about in its default mode too.
var claudeAskTools = []string{"Edit", "Write", "NotebookEdit", "WebFetch", "WebSearch"}

// Policy is what the settings that the agent can't override cover.
type Policy struct {
	// LockHome keeps the configuration of the agent's home from adding
	// what it runs, for the agents of members and guests.
	LockHome bool
	// AskCommands makes the agent ask before every shell command, also the
	// read-only ones it would run itself, for when the server's rules deny
	// some.
	AskCommands bool
	// MCPServers are the MCP servers whose tools the agent asks about.
	MCPServers []string
}

// policyReader is a profile that reads settings which override its others.
type policyReader interface {
	// policyPath returns the container folder where it reads them, and the
	// name of their file.
	policyPath() (dir, file string)
	policy(p Policy) ([]byte, error)
}

func (claude) policyPath() (string, string) { return claudePolicyDir, claudePolicyFile }

// policy makes Claude Code ask before the tools that change things, and
// run no hooks but its own, whatever its other settings allow: a skill's
// allowed-tools and a slash command add rules that would skip the server,
// and skills and agents can bring hooks that run commands. Claude Code
// checks ask rules for a whole tool before allow rules. With p.LockHome, it
// also ignores the MCP servers of ~/.claude.json, which it writes itself,
// and keeps those the server passes.
func (claude) policy(p Policy) ([]byte, error) {
	var ask []string
	if p.AskCommands {
		ask = append(ask, "Bash")
	}
	ask = append(ask, claudeAskTools...)
	for _, name := range p.MCPServers {
		if rule := "mcp__" + claudeServerName(name); !slices.Contains(ask, rule) {
			ask = append(ask, rule)
		}
	}
	settings := map[string]any{"permissions": map[string]any{"ask": ask}, "allowManagedHooksOnly": true}
	if p.LockHome {
		settings["strictPluginOnlyCustomization"] = []string{"mcp"}
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

var claudeNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// claudeServerName is the server part of the names that Claude Code gives
// the tools of the MCP server name, mcp__<server>__<tool>. Its rules end
// the server part at the first "__" of the tool name, so a rule for that
// part covers every tool of the server.
func claudeServerName(name string) string {
	server, _, _ := strings.Cut(claudeNameChars.ReplaceAllString(name, "_")+"__", "__")
	return server
}

// policyOf returns the policy reader of p: Claude Code's for an agent of
// unknown kind.
func policyOf(p Profile) (policyReader, bool) {
	if c, ok := p.(custom); ok {
		if c.base == nil {
			return claude{}, true
		}
		p = c.base
	}
	r, ok := p.(policyReader)
	return r, ok
}

// WritePolicy writes into dir the settings that the agent of p can't
// override, which [Spec.PolicyDir] mounts in a sandbox. It writes nothing
// for an agent without such settings.
func WritePolicy(p Profile, dir string, policy Policy) error {
	r, ok := policyOf(p)
	if !ok {
		return nil
	}
	data, err := r.policy(policy)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	_, file := r.policyPath()
	return fsutil.WriteAtomic(filepath.Join(dir, file), data, 0o644)
}

// policyMount returns the folder where the agent of spec reads the policy
// in spec.PolicyDir, or "" for an agent without one. It refuses a policy
// that [WritePolicy] didn't write, which would leave the agent's rules
// unbounded.
func policyMount(spec Spec) (string, error) {
	r, ok := policyOf(spec.Profile)
	if !ok {
		return "", nil
	}
	dst, file := r.policyPath()
	path := filepath.Join(spec.PolicyDir, file)
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("the agent's policy %s is missing: %v", path, err)
	}
	return dst, nil
}
