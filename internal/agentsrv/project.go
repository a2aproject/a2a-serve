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
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/project"
	"github.com/a2aproject/a2a-serve/internal/projectfiles"
)

// ProjectStatus lists the MCP servers and skills the project of cfg brings.
func ProjectStatus(d agentdir.Dir, cfg *agentdir.Config, secrets agentdir.Secrets) (servers []controlapi.MCPServerStatus, skills []projectfiles.Skill, err error) {
	ws, err := d.WorkspaceMount(cfg)
	if err != nil {
		return nil, nil, err
	}
	found, mcpErr := projectfiles.LoadMCP(ws.Src)
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
	skills, err = projectfiles.Skills(ws.Src)
	return servers, skills, errors.Join(mcpErr, err)
}

// workspace returns the working directory of the agent's sessions.
func (s *Server) workspace() (string, error) {
	ws, err := s.dir.WorkspaceMount(s.rt.Config())
	if err != nil {
		return "", err
	}
	return ws.Src, nil
}
