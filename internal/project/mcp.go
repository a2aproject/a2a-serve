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

// Package project finds what a project brings for its harness: the MCP
// servers of its .mcp.json and the skills in its skill folders.
package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

// MCPFile is the name of the project's MCP server list, in the format
// Claude Code and other harnesses read.
const MCPFile = ".mcp.json"

// MCP server types.
const (
	MCPStdio = "stdio"
	MCPHTTP  = "http"
	MCPSSE   = "sse"
)

// MCPServer is an MCP server of a project.
type MCPServer struct {
	Name    string            `json:"-"`
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// LoadMCP reads the MCP servers of the .mcp.json in dir, sorted by name. A
// missing file has none. The agent can write the workspace, so a symlink
// that leads out of dir is refused.
func LoadMCP(dir string) ([]MCPServer, error) {
	path := filepath.Join(dir, MCPFile)
	data, err := readIn(dir, MCPFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil && linksOut(dir, path) {
		return nil, fmt.Errorf("%s links out of the workspace", path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var file struct {
		MCPServers map[string]MCPServer `json:"mcpServers"`
	}
	err = json.Unmarshal(data, &file)
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return nil, fmt.Errorf("%s isn't valid JSON (%w)", path, err)
	}
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	out := make([]MCPServer, 0, len(file.MCPServers))
	for _, name := range slices.Sorted(maps.Keys(file.MCPServers)) {
		s := file.MCPServers[name]
		s.Name = name
		switch s.Type {
		case "":
			s.Type = MCPStdio
			if s.Command == "" && s.URL != "" {
				s.Type = MCPHTTP
			}
		case "streamable-http", "streamableHttp":
			s.Type = MCPHTTP
		}
		if err := s.validate(); err != nil {
			return nil, fmt.Errorf("%s: MCP server %q: %w", path, name, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// readIn reads the file name in dir without leaving dir.
// linksOut reports whether path is a symlink whose target is out of dir.
func linksOut(dir, path string) bool {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, target)
	return err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func readIn(dir, name string) (data []byte, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return root.ReadFile(name)
}

func (s MCPServer) validate() error {
	switch s.Type {
	case MCPStdio:
		if s.Command == "" {
			return errors.New("no command")
		}
	case MCPHTTP, MCPSSE:
		if s.URL == "" {
			return errors.New("no url")
		}
	default:
		return fmt.Errorf("unknown type %q: want stdio, http or sse", s.Type)
	}
	return nil
}

var varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// Resolve replaces ${VAR} and ${VAR:-default} with the values of lookup. It
// returns the names of the variables which have neither a value nor a
// default, sorted.
func (s MCPServer) Resolve(lookup func(name string) (string, bool)) (MCPServer, []string) {
	missing := map[string]bool{}
	expand := func(v string) string {
		return varRef.ReplaceAllStringFunc(v, func(ref string) string {
			m := varRef.FindStringSubmatch(ref)
			if val, ok := lookup(m[1]); ok {
				return val
			}
			if m[2] != "" {
				return m[3]
			}
			missing[m[1]] = true
			return ""
		})
	}
	expandMap := func(in map[string]string) map[string]string {
		if in == nil {
			return nil
		}
		out := make(map[string]string, len(in))
		for k, v := range in {
			out[k] = expand(v)
		}
		return out
	}
	out := s
	out.Command, out.URL = expand(s.Command), expand(s.URL)
	out.Args = slices.Clone(s.Args)
	for i, a := range out.Args {
		out.Args[i] = expand(a)
	}
	out.Env, out.Headers = expandMap(s.Env), expandMap(s.Headers)
	return out, slices.Sorted(maps.Keys(missing))
}

// Vars returns the names of the variables the server refers to, with or
// without a default, sorted.
func (s MCPServer) Vars() []string {
	names := map[string]bool{}
	add := func(v string) {
		for _, m := range varRef.FindAllStringSubmatch(v, -1) {
			names[m[1]] = true
		}
	}
	add(s.Command)
	add(s.URL)
	for _, a := range s.Args {
		add(a)
	}
	for _, v := range s.Env {
		add(v)
	}
	for _, v := range s.Headers {
		add(v)
	}
	return slices.Sorted(maps.Keys(names))
}

type acpPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func acpPairs(m map[string]string) []acpPair {
	out := make([]acpPair, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, acpPair{Name: k, Value: m[k]})
	}
	return out
}

// ACP returns the server in the JSON form of the ACP McpServer schema.
func (s MCPServer) ACP() (json.RawMessage, error) {
	if s.Type == MCPStdio {
		args := s.Args
		if args == nil {
			args = []string{}
		}
		return json.Marshal(struct {
			Name    string    `json:"name"`
			Command string    `json:"command"`
			Args    []string  `json:"args"`
			Env     []acpPair `json:"env"`
		}{s.Name, s.Command, args, acpPairs(s.Env)})
	}
	return json.Marshal(struct {
		Type    string    `json:"type"`
		Name    string    `json:"name"`
		URL     string    `json:"url"`
		Headers []acpPair `json:"headers"`
	}{s.Type, s.Name, s.URL, acpPairs(s.Headers)})
}

// Summary describes the server in one line: its command line or URL. Call it
// before Resolve, so it shows variable references instead of their values.
func (s MCPServer) Summary() string {
	if s.Type == MCPStdio {
		return strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
	}
	u := s.URL
	if i := strings.Index(u, "?"); i >= 0 {
		u = u[:i] + "?…"
	}
	return s.Type + " " + u
}

// UnknownServer is a name of a list of disabled servers that .mcp.json has
// no server for.
type UnknownServer struct {
	Name string
	// Suggestion is the server whose name is closest, or "".
	Suggestion string
	// Known are the names of the servers .mcp.json has.
	Known []string
}

func (u UnknownServer) String() string {
	msg := fmt.Sprintf("--disable-mcp %s names no server of %s", u.Name, MCPFile)
	switch {
	case u.Suggestion != "":
		return msg + ": did you mean " + u.Suggestion + "?"
	case len(u.Known) == 0:
		return msg + "; it has none"
	}
	return msg + "; it has " + strings.Join(u.Known, ", ")
}

// UnknownDisabled returns the names of disabled that servers don't have.
func UnknownDisabled(servers []MCPServer, disabled []string) []UnknownServer {
	known := make([]string, len(servers))
	for i, s := range servers {
		known[i] = s.Name
	}
	var out []UnknownServer
	for _, name := range disabled {
		if slices.Contains(known, name) {
			continue
		}
		out = append(out, UnknownServer{Name: name, Suggestion: agentdir.Closest(name, known), Known: known})
	}
	return out
}
