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

package agentsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/project"
)

// ProjectStatus lists the MCP servers and skills the project of cfg brings.
func ProjectStatus(d agentdir.Dir, cfg *agentdir.Config, secrets agentdir.Secrets) (servers []controlapi.MCPServerStatus, skills []project.Skill, err error) {
	ws, err := d.WorkspaceMount(cfg)
	if err != nil {
		return nil, nil, err
	}
	found, mcpErr := project.LoadMCP(ws.Src)
	if mcpErr != nil {
		mcpErr = fmt.Errorf("%w; the agent gets no MCP servers from it", mcpErr)
	}
	vars := project.Vars(cfg, secrets)
	for _, srv := range found {
		st := controlapi.MCPServerStatus{Name: srv.Name, Type: srv.Type, Summary: srv.Summary(), Disabled: cfg.MCPDisabled(srv.Name)}
		if !st.Disabled {
			_, st.Missing = srv.Resolve(vars)
		}
		servers = append(servers, st)
	}
	skills, err = project.Skills(ws.Src)
	return servers, skills, errors.Join(mcpErr, err)
}

// sessionParams passes the MCP servers of the project's .mcp.json to each
// new ACP session. It reads the file for every session, so edits apply to
// the next conversation. Servers the config turns off are left out, and so
// is a server with a variable that has no value in the container of the
// conversation, such as a secret its sandbox rule doesn't give, since it
// would only fail inside the harness.
func (s *Server) sessionParams(ctx context.Context, execCtx *a2asrv.ExecutorContext) (a2aacp.SessionParams, error) {
	cfg := s.rt.Config()
	ws, err := s.workspace()
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	key, err := s.keys.of(ctx, execCtx.ContextID)
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	sb, err := s.sandboxes.of(ctx, key)
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	// The servers come from the workspace the container gets: none for an
	// empty one, and those of its copy for its own.
	src := ws
	if cfg.Sandbox != agentdir.SandboxLocal {
		switch sb.Storage {
		case agentdir.StorageEmpty:
			return a2aacp.SessionParams{Cwd: ws}, nil
		case agentdir.StorageOwn:
			src = s.workspaces.Dir(workspaceName(key), agentdir.StorageOwn)
		}
	}
	found, err := project.LoadMCP(src)
	if err != nil {
		slog.WarnContext(ctx, "ignoring the project's MCP servers", "error", err)
		return a2aacp.SessionParams{Cwd: ws}, nil
	}
	// The variables are the container's own: the secrets that the server
	// adds at the boundary stay out of them.
	granted, _, _, err := secretEnv(cfg, sb, s.rt.Secrets())
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	vars := project.EnvVars(cfg.Env, granted)
	var servers []json.RawMessage
	for _, srv := range found {
		if cfg.MCPDisabled(srv.Name) {
			continue
		}
		resolved, missing := srv.Resolve(vars)
		if len(missing) > 0 {
			slog.WarnContext(ctx, "leaving out an MCP server with variables that the conversation's container doesn't get; pass them with --secret, or give them in the sandbox rule of its user", "server", srv.Name, "missing", missing)
			continue
		}
		raw, err := resolved.ACP()
		if err != nil {
			return a2aacp.SessionParams{}, err
		}
		servers = append(servers, raw)
	}
	return a2aacp.SessionParams{Cwd: ws, MCPServers: servers}, nil
}

// mcpServerNames returns the names of the MCP servers of the project's
// .mcp.json, or none if it can't be read.
func (s *Server) mcpServerNames() []string {
	ws, err := s.workspace()
	if err != nil {
		return nil
	}
	found, err := project.LoadMCP(ws)
	if err != nil {
		return nil
	}
	names := make([]string, len(found))
	for i, srv := range found {
		names[i] = srv.Name
	}
	return names
}

// workspace returns the working directory of the agent's sessions.
func (s *Server) workspace() (string, error) {
	ws, err := s.dir.WorkspaceMount(s.rt.Config())
	if err != nil {
		return "", err
	}
	return ws.Src, nil
}
