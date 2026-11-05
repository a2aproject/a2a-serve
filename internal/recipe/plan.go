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
	"os"
	"slices"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/project"
)

// Var is a variable the agent needs a value for: a secret of config.yaml,
// or a variable of an MCP server.
type Var struct {
	Name        string
	Description string
	// Secret values are hidden as they are typed and stored as secrets;
	// others are saved with --env.
	Secret bool
}

// NewConfig returns the config of a new agent named name: the agent settings
// of the recipe. A recipe doesn't set where the agent is reachable or who
// may call it.
func (r *Recipe) NewConfig(name string) *agentdir.Config {
	cfg := r.Config.AgentSettings()
	cfg.Name = name
	return cfg
}

// SecretVars returns the secrets of the recipe's config.yaml.
func (r *Recipe) SecretVars() []Var {
	out := make([]Var, 0, len(r.Config.Secrets))
	for _, name := range r.Config.Secrets {
		out = append(out, Var{Name: name, Description: r.Manifest.Vars[name].Description, Secret: true})
	}
	return out
}

// ServerVars returns the variables of the MCP server s that have no default
// and that provided doesn't have a value for.
func (r *Recipe) ServerVars(s project.MCPServer, provided func(name string) bool) []Var {
	_, missing := s.Resolve(func(name string) (string, bool) { return "", provided(name) })
	out := make([]Var, 0, len(missing))
	for _, name := range missing {
		info := r.Manifest.Vars[name]
		out = append(out, Var{Name: name, Description: info.Description, Secret: info.IsSecret()})
	}
	return out
}

// MissingMounts returns the sources of the mounts of cfg tied to the MCP
// server, which don't exist.
func (r *Recipe) MissingMounts(cfg *agentdir.Config, server string) []string {
	var out []string
	for _, spec := range cfg.Mounts {
		m, err := agentdir.ParseMount(spec)
		if err != nil || r.Manifest.MountServer(m.Dst) != server {
			continue
		}
		if _, err := os.Stat(m.Src); err != nil {
			out = append(out, m.Src)
		}
	}
	return out
}

// RestoreMounts adds to local the mounts of base that recipe.yaml ties to
// the MCP servers off, which the agent leaves out, so that a merge sees
// them unchanged and doesn't take them for local removals.
func (r *Recipe) RestoreMounts(local, base *agentdir.Config, off []string) {
	have := map[string]bool{}
	for _, spec := range local.Mounts {
		if m, err := agentdir.ParseMount(spec); err == nil {
			have[m.Dst] = true
		}
	}
	for _, spec := range base.Mounts {
		m, err := agentdir.ParseMount(spec)
		if err != nil || have[m.Dst] || !slices.Contains(off, r.Manifest.MountServer(m.Dst)) {
			continue
		}
		local.Mounts = append(local.Mounts, spec)
	}
}

// DropMounts removes the mounts of cfg that recipe.yaml ties to the MCP
// servers off, which then don't need their sources.
func (r *Recipe) DropMounts(cfg *agentdir.Config, off []string) {
	cfg.Mounts = slices.DeleteFunc(cfg.Mounts, func(spec string) bool {
		m, err := agentdir.ParseMount(spec)
		return err == nil && slices.Contains(off, r.Manifest.MountServer(m.Dst))
	})
	if len(cfg.Mounts) == 0 {
		cfg.Mounts = nil
	}
}
