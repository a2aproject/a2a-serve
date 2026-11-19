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
	"errors"
	"log/slog"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/core/tools"
	"github.com/a2aproject/a2a-serve/internal/agentcfg"
	"github.com/a2aproject/a2a-serve/internal/agentrun"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// toolDecider answers the tool permissions that the tool rules don't with
// the preferences of the person who started the task and the grants of the
// conversation for that person. The agent asks the rules first.
type toolDecider struct {
	store *store.Store
	// taskUser returns the user who started a stored task.
	taskUser func(ctx context.Context, id a2a.TaskID) (string, error)
}

// starter returns the user who started the task of execCtx: the caller of
// its first execution, or else the user stored with the task. It fails
// rather than guess, so that nobody's grants apply to another's task.
func (d *toolDecider) starter(ctx context.Context, execCtx *a2asrv.ExecutorContext) (string, error) {
	if execCtx.StoredTask == nil {
		p, ok := auth.Caller(ctx)
		if !ok {
			return "", errors.New("the task has no caller")
		}
		return p.ID, nil
	}
	return d.taskUser(ctx, execCtx.TaskID)
}

// What decides a call besides the rules.
const (
	byPreference = "preference"
	byGrant      = "grant of the conversation"
)

// toolDecider decides the tool calls of the agent: by the tool rules of the
// config in force, then by the preferences and grants of the person who
// started the task. Each decider records what decided for the log.
func (s *Server) toolDecider() tools.Decider {
	rules := tools.DeciderFunc(func(ctx context.Context, c tools.Call) (tools.Decision, bool, error) {
		cfg := s.rt.Config()
		return tools.Rules{Allow: cfg.Approvals.Allow, Deny: cfg.Approvals.Deny}.Decide(ctx, c)
	})
	return tools.Chain(rules, tools.DeciderFunc(s.decider.byPeople))
}

// byPeople decides c in a turn for the person who started its task.
func (d *toolDecider) byPeople(ctx context.Context, c tools.Call) (tools.Decision, bool, error) {
	tc := agentrun.ToolCallOf(ctx)
	if tc == nil || tc.ExecCtx == nil {
		return tools.Decision{}, false, nil
	}
	dec, ok, err := d.hook(ctx, tc.ExecCtx, toolrules.Call{Kind: c.Kind, Tool: c.Tool, MCP: c.MCP, Command: c.Command})
	if err != nil || !ok {
		return tools.Decision{}, false, err
	}
	tc.By, tc.User = dec.By, dec.User
	return tools.Decision{Allow: dec.Allow, Rule: dec.Rule}, true, nil
}

// hook answers call for the person who started the task of execCtx.
func (d *toolDecider) hook(ctx context.Context, execCtx *a2asrv.ExecutorContext, call toolrules.Call) (agentrun.Decision, bool, error) {
	user, err := d.starter(ctx, execCtx)
	if err != nil {
		slog.Warn("looking up who started a task, asking only the rules", "task", execCtx.TaskID, "error", err)
		return agentrun.Decision{}, false, nil
	}
	return d.decide(ctx, execCtx.ContextID, user, call)
}

// decide answers call with the preferences of user, who started its task,
// then the grants of the conversation for user. It returns false if none
// decides.
func (d *toolDecider) decide(ctx context.Context, contextID, user string, call toolrules.Call) (agentrun.Decision, bool, error) {
	if user == "" || d.store == nil {
		return agentrun.Decision{}, false, nil
	}
	prefs, err := d.store.ToolPreferences(ctx, user)
	if err != nil {
		return agentrun.Decision{}, false, err
	}
	if rule, ok := toolrules.Match(prefs, call); ok {
		return agentrun.Decision{Allow: true, Rule: rule, By: byPreference, User: user}, true, nil
	}
	grants, err := d.store.ToolGrants(ctx, contextID, user)
	if err != nil {
		return agentrun.Decision{}, false, err
	}
	if rule, ok := toolrules.Match(grants, call); ok {
		return agentrun.Decision{Allow: true, Rule: rule, By: byGrant, User: user}, true, nil
	}
	return agentrun.Decision{}, false, nil
}

// decideWithRules answers call with the rules of the config, then as
// decide does.
func (s *Server) decideWithRules(ctx context.Context, contextID, user string, call toolrules.Call) (agentrun.Decision, bool, error) {
	if dec, ok := agentcfg.ToolRules(s.rt.Config()).Decide(call); ok {
		return agentrun.Decision{Allow: dec.Allow, Rule: dec.Entry, By: agentrun.ByRule}, true, nil
	}
	return s.decider.decide(ctx, contextID, user, call)
}

// call returns the call of a permission, with the MCP servers of the
// workspace.
func (s *Server) call(kind, title string, input any) toolrules.Call {
	var servers []string
	if mounts, err := s.dir.Mounts(s.rt.Config()); err == nil {
		servers = agentrun.MCPServerNames(mounts[0].Src)
	}
	return toolrules.NewCall(kind, title, input, servers)
}
