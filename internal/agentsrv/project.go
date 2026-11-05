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
	found, err := project.LoadMCP(ws.Src)
	if err != nil {
		return nil, nil, err
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
	return servers, skills, err
}

// sessionParams passes the MCP servers of the project's .mcp.json to each
// new ACP session. It reads the file for every session, so edits apply to
// the next conversation. Servers the config turns off are left out, and so
// is a server with a variable that has no value, since it would only fail
// inside the harness.
func (s *Server) sessionParams(ctx context.Context, _ *a2asrv.ExecutorContext) (a2aacp.SessionParams, error) {
	cfg := s.config()
	ws, err := s.dir.WorkspaceMount(cfg)
	if err != nil {
		return a2aacp.SessionParams{}, err
	}
	found, err := project.LoadMCP(ws.Src)
	if err != nil {
		slog.WarnContext(ctx, "ignoring the project's MCP servers", "error", err)
		return a2aacp.SessionParams{Cwd: ws.Src}, nil
	}
	vars := project.Vars(cfg, s.secrets())
	var servers []json.RawMessage
	for _, srv := range found {
		if cfg.MCPDisabled(srv.Name) {
			continue
		}
		resolved, missing := srv.Resolve(vars)
		if len(missing) > 0 {
			slog.WarnContext(ctx, "leaving out an MCP server with unset variables; pass them with --secret", "server", srv.Name, "missing", missing)
			continue
		}
		raw, err := resolved.ACP()
		if err != nil {
			return a2aacp.SessionParams{}, err
		}
		servers = append(servers, raw)
	}
	return a2aacp.SessionParams{Cwd: ws.Src, MCPServers: servers}, nil
}
