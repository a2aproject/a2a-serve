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

package recipe

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/project"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// Line is a line of the review of what a recipe makes the machine run.
type Line struct {
	Label string
	Items []Item
}

// Item is an entry of a review line. Warn marks a risky entry.
type Item struct {
	Text string
	// Detail is shown under the entry, such as the description of an MCP
	// server.
	Detail string
	Warn   bool
}

// agentConfigFiles are the files of an agent's own configuration, which can
// change what the agent runs and approves.
var agentConfigFiles = []string{".claude/settings.json", ".claude/settings.local.json", "opencode.json", "opencode.jsonc", ".opencode/"}

// instructionFiles are the files of instructions agents read.
var instructionFiles = []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"}

// safeKinds are the ACP tool kinds that don't change anything.
var safeKinds = []string{"read", "search", "think"}

// Review lists what the agent of the recipe will run with cfg, the recipe's
// agent settings with the owner's flags applied, in the directory
// workspace: the agent, sandbox, mounts, environment, MCP servers, skills,
// instructions, agent configuration, approvals and limits, and the owner
// settings of the recipe that aren't taken.
func Review(r *Recipe, cfg *agentdir.Config, workspace string) []Line {
	var out []Line
	add := func(label string, items ...Item) {
		if len(items) > 0 {
			out = append(out, Line{Label: label, Items: items})
		}
	}
	local := cfg.Sandbox == agentdir.SandboxLocal
	agent := cfg.Harness
	if len(cfg.Command) > 0 {
		agent += ": " + agentdir.ShellJoin(cfg.Command)
	}
	if cfg.Image != "" {
		agent += ", image " + cfg.Image
	}
	add("agent", Item{Text: agent, Warn: len(cfg.Command) > 0 && local})
	if local {
		add("sandbox", Item{Text: "local: no isolation, with your permissions", Warn: true})
	} else {
		add("sandbox", Item{Text: agentdir.SandboxDocker})
	}
	add("workspace", Item{Text: workspace + ", read-write"})
	add("mounts", mountItems(r, cfg)...)
	add("environment", envItems(cfg)...)
	add("mcp", mcpItems(r, cfg)...)
	add("skills", skillItems(r.Skills)...)
	add("instructions", fileItems(r.Files, instructionFiles, false)...)
	add("agent config", fileItems(r.Files, agentConfigFiles, true)...)
	add("approvals", approvalItems(cfg.Approvals)...)
	add("limits", limitItems(cfg.Limits)...)
	owner := slices.DeleteFunc(r.Config.OwnerSettings(), func(s string) bool { return s == "name" })
	if len(owner) > 0 {
		add("not taken", Item{Text: strings.Join(owner, ", ") + ": the owner sets these"})
	}
	return out
}

func mountItems(r *Recipe, cfg *agentdir.Config) []Item {
	var out []Item
	for _, spec := range cfg.Mounts {
		m, err := agentdir.ParseMount(spec)
		if err != nil {
			out = append(out, Item{Text: spec, Warn: true})
			continue
		}
		text := homeRel(m.Src) + " → " + m.Dst
		if m.ReadOnly {
			text += ", read-only"
		} else {
			text += ", read-write"
		}
		if srv := r.Manifest.MountServer(m.Dst); srv != "" {
			text += ", for " + srv
		}
		out = append(out, Item{Text: text, Warn: !m.ReadOnly})
	}
	return out
}

// homeRel writes a path in the home directory with ~.
func homeRel(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if rel, err := filepath.Rel(home, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return filepath.Join("~", rel)
	}
	return p
}

func envItems(cfg *agentdir.Config) []Item {
	var out []Item
	if len(cfg.Secrets) > 0 {
		out = append(out, Item{Text: "secrets " + strings.Join(cfg.Secrets, ", ")})
	}
	if len(cfg.Env) > 0 {
		out = append(out, Item{Text: "env " + strings.Join(cfg.Env, ", ")})
	}
	return out
}

func mcpItems(r *Recipe, cfg *agentdir.Config) []Item {
	var out []Item
	for _, s := range r.MCP {
		name := s.Name
		switch {
		case cfg.MCPDisabled(s.Name):
			name += ", off"
		case r.Manifest.Optional(s.Name):
			name += ", optional"
		}
		out = append(out, Item{Text: name + ": " + s.Summary(), Detail: r.Manifest.MCP[s.Name].Description})
	}
	return out
}

func skillItems(skills []project.Skill) []Item {
	if len(skills) == 0 {
		return nil
	}
	var names []string
	for _, s := range skills {
		if !slices.Contains(names, s.Name) {
			names = append(names, s.Name)
		}
	}
	return []Item{{Text: strings.Join(names, ", ")}}
}

// fileItems lists the files of the recipe among names, where a name ending
// in / matches the files in that directory.
func fileItems(files Files, names []string, warn bool) []Item {
	var found []string
	for _, name := range names {
		dir, isDir := strings.CutSuffix(name, "/")
		for _, p := range files.Paths() {
			if isDir && strings.HasPrefix(p, dir+"/") {
				found = append(found, name)
				break
			}
			if p != name {
				continue
			}
			if f := files[p]; f.Link != "" {
				found = append(found, p+" → "+path.Join(path.Dir(p), f.Link))
			} else {
				found = append(found, p)
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	text := strings.Join(found, ", ")
	if warn {
		text += ": they can change what the agent runs and approves"
	}
	return []Item{{Text: text, Warn: warn}}
}

func approvalItems(a agentdir.ApprovalsConfig) []Item {
	var out []Item
	if len(a.Allow) > 0 {
		out = append(out, Item{Text: "approved without asking: " + strings.Join(a.Allow, ", "), Warn: allowsChanges(a.Allow)})
	}
	if len(a.Deny) > 0 {
		out = append(out, Item{Text: "denied: " + strings.Join(a.Deny, ", ")})
	}
	return out
}

// allowsChanges reports whether the patterns allow MCP tools or commands,
// or match a tool kind besides the ones that don't change anything.
func allowsChanges(patterns []string) bool {
	for _, p := range patterns {
		if strings.HasPrefix(p, toolrules.PrefixTool) || strings.HasPrefix(p, toolrules.PrefixCommand) {
			return true
		}
	}
	for _, kind := range agentdir.ToolKinds {
		if slices.Contains(safeKinds, kind) {
			continue
		}
		for _, p := range patterns {
			if ok, err := path.Match(p, kind); err == nil && ok {
				return true
			}
		}
	}
	return false
}

func limitItems(l agentdir.LimitsConfig) []Item {
	var parts []string
	if l.Memory != "" {
		parts = append(parts, "memory "+l.Memory)
	}
	if l.CPUs != "" {
		parts = append(parts, "cpus "+l.CPUs)
	}
	if l.PIDs != 0 {
		parts = append(parts, fmt.Sprintf("pids %d", l.PIDs))
	}
	if len(parts) == 0 {
		return nil
	}
	return []Item{{Text: strings.Join(parts, ", ")}}
}
