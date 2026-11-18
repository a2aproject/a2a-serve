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

package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/core/agent"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
	"github.com/a2aproject/a2a-serve/internal/workspaces"
)

// sessionParams passes the MCP servers of the project's .mcp.json to each
// new ACP session. It reads the file for every session, so edits apply to
// the next conversation. Disabled servers are left out, and so is a server
// with a variable that has no value in the container of the conversation,
// such as a secret its grant doesn't give, since it would only fail inside
// the agent.
func (r *Runner) sessionParams(ctx context.Context, execCtx *a2asrv.ExecutorContext) (a2aacp.SessionParams, error) {
	spec, err := r.cfg.Spec()
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	ws, err := workspaceOf(spec)
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	key, err := r.keys.of(ctx, execCtx.ContextID)
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	proc, err := r.cfg.Processes.Process(ctx, key)
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	params, found, err := r.sessionMCP(ctx, spec, ws, proc)
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	// The agent reads its policy when the session starts, so the policy
	// follows the rules and gets the MCP servers added since the agent
	// process started.
	if spec.Sandbox != agent.SandboxLocal {
		names := make([]string, len(found))
		for i, srv := range found {
			names[i] = srv.Name
		}
		if _, err := r.policies.write(spec, key != "", names); err != nil {
			return a2aacp.SessionParams{}, err
		}
	}
	return params, nil
}

// sessionMCP returns the params of a session of the agent process proc in
// the workspace ws, with the MCP servers of the workspace it gets, and the
// servers it found.
func (r *Runner) sessionMCP(ctx context.Context, spec Spec, ws string, proc Process) (a2aacp.SessionParams, []projectfiles.MCPServer, error) {
	// The servers come from the workspace the container gets: none for an
	// empty one, and those of its copy for its own.
	src := ws
	if spec.Sandbox != agent.SandboxLocal {
		switch proc.Grant.Storage {
		case workspaces.StorageEmpty:
			return a2aacp.SessionParams{Cwd: ws}, nil, nil
		case workspaces.StorageOwn:
			src = r.copyDir(proc, workspaces.StorageOwn)
		}
	}
	found, err := projectfiles.LoadMCP(src)
	if err != nil {
		slog.WarnContext(ctx, "ignoring the project's MCP servers", "error", err)
		return a2aacp.SessionParams{Cwd: ws}, nil, nil
	}
	// The variables are the container's own: the secrets that the boundary
	// adds stay out of them.
	granted, _, _, err := secretEnv(spec.Inject, proc.Grant, r.cfg.Secrets())
	if err != nil {
		return a2aacp.SessionParams{}, nil, err
	}
	vars := projectfiles.EnvVars(spec.Env, granted)
	var servers []json.RawMessage
	for _, srv := range found {
		if slices.Contains(spec.DisabledMCP, srv.Name) {
			continue
		}
		resolved, missing := srv.Resolve(vars)
		if len(missing) > 0 {
			if hook := r.cfg.Hooks.LeftOutMCP; hook != nil {
				hook(ctx, srv.Name, missing)
			} else {
				slog.WarnContext(ctx, "leaving out an MCP server with variables that the conversation's container doesn't get", "server", srv.Name, "missing", missing)
			}
			continue
		}
		raw, err := resolved.ACP()
		if err != nil {
			return a2aacp.SessionParams{}, nil, err
		}
		servers = append(servers, raw)
	}
	return a2aacp.SessionParams{Cwd: ws, MCPServers: servers}, found, nil
}

// mcpServerNames returns the names of the MCP servers of the project's
// .mcp.json, or none if it can't be read.
func (r *Runner) mcpServerNames() []string {
	ws, err := workspaceOf(r.spec())
	if err != nil {
		return nil
	}
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
