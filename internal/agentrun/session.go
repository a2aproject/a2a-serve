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

package agentrun

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/projectfiles"
)

// sessionParams passes the MCP servers of the project's .mcp.json to each
// new ACP session. It reads the file for every session, so edits apply to
// the next conversation. Disabled servers are left out, and so is a server
// with a variable that has no value in the process of the conversation,
// such as a secret its Environment doesn't give, since it would only fail
// inside the agent.
func (r *Runner) sessionParams(ctx context.Context, execCtx *a2asrv.ExecutorContext) (a2aacp.SessionParams, error) {
	key, err := r.keys.of(ctx, execCtx.ContextID)
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	env, err := r.environment(ctx, key)
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	if err := env.process().SessionError; err != nil {
		return a2aacp.SessionParams{}, err
	}
	ws, err := r.workspaceOf(key)
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	params, names, err := r.sessionMCP(ctx, ws, env)
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	// The agent reads its policy when the session starts, so the policy
	// follows the rules and gets the MCP servers added since the agent
	// process started.
	if r.rt.isolated() {
		if _, err := r.policies.write(r.cfg.Profile, r.askCommands(), key != "", names); err != nil {
			return a2aacp.SessionParams{}, err
		}
	}
	return params, nil
}

// sessionMCP returns the params of a session in the workspace ws of a
// process with env, with the MCP servers it gets: those of env, then those
// of the workspace. It returns the names of the servers.
func (r *Runner) sessionMCP(ctx context.Context, ws string, env Environment) (a2aacp.SessionParams, []string, error) {
	params := a2aacp.SessionParams{Cwd: ws}
	var names []string
	for _, srv := range env.MCPServers {
		if err := checkCallerMCP(srv, names); err != nil {
			return a2aacp.SessionParams{}, nil, err
		}
		raw, err := srv.ACP()
		if err != nil {
			return a2aacp.SessionParams{}, nil, err
		}
		params.MCPServers = append(params.MCPServers, raw)
		names = append(names, srv.Name)
	}
	r.callerMCP.add(names...)
	found := r.workspaceMCP(ctx, ws, env)
	vars := env.process().MCPVars
	if vars == nil {
		vars = projectfiles.EnvVars(env.Env, nil)
	}
	var disabled []string
	if r.cfg.Options.DisabledMCP != nil {
		disabled = r.cfg.Options.DisabledMCP()
	}
	callers := len(names)
	for _, srv := range found {
		if slices.Contains(names[:callers], srv.Name) {
			r.log().WarnContext(ctx, "leaving out an MCP server of the workspace with the name of a server the caller gives", "server", srv.Name)
			continue
		}
		names = append(names, srv.Name)
		if slices.Contains(disabled, srv.Name) {
			continue
		}
		resolved, missing := srv.Resolve(vars)
		if len(missing) > 0 {
			if hook := r.cfg.Hooks.LeftOutMCP; hook != nil {
				hook(ctx, srv.Name, missing)
			} else {
				r.log().WarnContext(ctx, "leaving out an MCP server with variables that the conversation's container doesn't get", "server", srv.Name, "missing", missing)
			}
			continue
		}
		raw, err := resolved.ACP()
		if err != nil {
			return a2aacp.SessionParams{}, nil, err
		}
		params.MCPServers = append(params.MCPServers, raw)
	}
	return params, names, nil
}

// workspaceMCP returns the MCP servers of the .mcp.json that sessions of
// a process with env get, in the workspace ws.
func (r *Runner) workspaceMCP(ctx context.Context, ws string, env Environment) []projectfiles.MCPServer {
	proc := env.process()
	if proc.NoMCP {
		return nil
	}
	src := ws
	if proc.MCPDir != "" {
		src = proc.MCPDir
	}
	found, err := projectfiles.LoadMCP(src)
	if err != nil {
		r.log().WarnContext(ctx, "ignoring the project's MCP servers", "error", err)
		return nil
	}
	return found
}

var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// checkCallerMCP refuses an MCP server of an Environment that agents
// can't name in their tool names, that isn't served over HTTP, or whose
// name is in taken.
func checkCallerMCP(srv projectfiles.MCPServer, taken []string) error {
	if !mcpServerName.MatchString(srv.Name) {
		return fmt.Errorf("invalid MCP server name %q: want 1 to 64 letters, digits, _ or -", srv.Name)
	}
	if slices.Contains(taken, srv.Name) {
		return fmt.Errorf("two MCP servers named %s", srv.Name)
	}
	u, err := url.Parse(srv.URL)
	if srv.Type != projectfiles.MCPHTTP || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid MCP server %s: want an http or https URL", srv.Name)
	}
	return nil
}

// toolServerNames returns the names of the MCP servers that tool calls
// may come from: those of the workspace of the agent's settings, and
// those that sessions got from Environments.
func (r *Runner) toolServerNames() []string {
	return append(r.mcpServerNames(), r.callerMCP.list()...)
}

// nameSet is a set of names that only grows, as sessions with them may
// live as long as the runner.
type nameSet struct {
	mu    sync.Mutex
	names []string
}

func (s *nameSet) add(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range names {
		if !slices.Contains(s.names, n) {
			s.names = append(s.names, n)
		}
	}
}

func (s *nameSet) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.names)
}

// mcpServerNames returns the names of the MCP servers of the workspace of
// the agent's settings.
func (r *Runner) mcpServerNames() []string {
	if len(r.cfg.Options.Mounts) == 0 {
		return nil
	}
	return MCPServerNames(r.cfg.Options.Mounts[0].Src)
}

// MCPServerNames returns the names of the MCP servers of the .mcp.json in
// ws, or none if it can't be read.
func MCPServerNames(ws string) []string {
	found, err := projectfiles.LoadMCP(ws)
	if err != nil {
		return nil
	}
	names := make([]string, len(found))
	for i, srv := range found {
		names[i] = srv.Name
	}
	return names
}
