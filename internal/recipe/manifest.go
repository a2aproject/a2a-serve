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
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/project"
)

// ManifestFile is the name of the file in .a2a that describes a recipe's
// MCP servers and their variables.
const ManifestFile = "recipe.yaml"

// Manifest is the content of .a2a/recipe.yaml.
type Manifest struct {
	MCP  map[string]MCPInfo `yaml:"mcp,omitempty"`
	Vars map[string]VarInfo `yaml:"vars,omitempty"`
}

// MCPInfo describes an MCP server of the recipe's .mcp.json.
type MCPInfo struct {
	Description string `yaml:"description,omitempty"`
	// Optional servers are ones the agent works without, which the owner
	// is asked about.
	Optional bool `yaml:"optional,omitempty"`
	// Mounts are the destinations of the mounts of config.yaml that only
	// this server uses.
	Mounts []string `yaml:"mounts,omitempty"`
}

// VarInfo describes a variable of .mcp.json or a secret of config.yaml.
type VarInfo struct {
	Description string `yaml:"description,omitempty"`
	// Secret is false for a variable whose value isn't secret, which is
	// shown as it is typed and saved with --env. Unset means true.
	Secret *bool `yaml:"secret,omitempty"`
}

// IsSecret reports whether the variable's value is secret.
func (v VarInfo) IsSecret() bool { return v.Secret == nil || *v.Secret }

// Optional reports whether the MCP server name is optional.
func (m *Manifest) Optional(name string) bool { return m.MCP[name].Optional }

// MountServer returns the server the mount destination dst is tied to, or
// "" if it isn't tied to one.
func (m *Manifest) MountServer(dst string) string {
	for _, name := range slices.Sorted(maps.Keys(m.MCP)) {
		if slices.Contains(m.MCP[name].Mounts, path.Clean(dst)) {
			return name
		}
	}
	return ""
}

// ParseManifest parses the content of recipe.yaml, which may only use the
// keys a2a-serve knows.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	for name, info := range m.MCP {
		for i, dst := range info.Mounts {
			if !path.IsAbs(dst) {
				return nil, fmt.Errorf("mcp.%s.mounts: %q is not an absolute path", name, dst)
			}
			info.Mounts[i] = path.Clean(dst)
		}
	}
	return &m, nil
}

// check checks that the manifest names only servers of .mcp.json, mounts of
// cfg, and variables the servers or the secrets of cfg have.
func (m *Manifest) check(cfg *agentdir.Config, servers []project.MCPServer) error {
	vars := map[string]bool{}
	known := map[string]bool{}
	for _, s := range servers {
		known[s.Name] = true
		for _, v := range s.Vars() {
			vars[v] = true
		}
	}
	dsts := map[string]bool{}
	for _, spec := range cfg.Mounts {
		mnt, err := agentdir.ParseMount(spec)
		if err != nil {
			return err
		}
		dsts[filepath.ToSlash(mnt.Dst)] = true
	}
	var errs []error
	tied := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(m.MCP)) {
		if !known[name] {
			errs = append(errs, fmt.Errorf("mcp.%s: .mcp.json has no server %q", name, name))
		}
		for _, dst := range m.MCP[name].Mounts {
			if !dsts[dst] {
				errs = append(errs, fmt.Errorf("mcp.%s.mounts: config.yaml has no mount at %s", name, dst))
			}
			if other, ok := tied[dst]; ok {
				errs = append(errs, fmt.Errorf("mcp.%s.mounts: the mount at %s is tied to %s too; a tied mount belongs to one server", name, dst, other))
			}
			tied[dst] = name
		}
	}
	for _, name := range slices.Sorted(maps.Keys(m.Vars)) {
		secret := slices.Contains(cfg.Secrets, name)
		switch {
		case !secret && !vars[name]:
			errs = append(errs, fmt.Errorf("vars.%s: neither .mcp.json nor the secrets of config.yaml use %s", name, name))
		case secret && !m.Vars[name].IsSecret():
			errs = append(errs, fmt.Errorf("vars.%s: %s is a secret of config.yaml, so it can't be secret: false", name, name))
		}
	}
	return errors.Join(errs...)
}
