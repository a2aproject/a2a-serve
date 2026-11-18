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

	"github.com/a2aproject/a2a-serve/core/runner"
	"github.com/a2aproject/a2a-serve/core/toolrules"
	"github.com/a2aproject/a2a-serve/internal/agentcfg"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// toolDecider answers the tool permissions that the tool rules don't with
// the preferences of the person who started the task and the grants of the
// conversation for that person. The runner asks the rules first.
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

// hook answers call for the person who started the task of execCtx. It
// implements the Decide hook of the runner.
func (d *toolDecider) hook(ctx context.Context, execCtx *a2asrv.ExecutorContext, call toolrules.Call) (runner.Decision, bool, error) {
	user, err := d.starter(ctx, execCtx)
	if err != nil {
		slog.Warn("looking up who started a task, asking only the rules", "task", execCtx.TaskID, "error", err)
		return runner.Decision{}, false, nil
	}
	return d.decide(ctx, execCtx.ContextID, user, call)
}

// decide answers call with the preferences of user, who started its task,
// then the grants of the conversation for user. It returns false if none
// decides.
func (d *toolDecider) decide(ctx context.Context, contextID, user string, call toolrules.Call) (runner.Decision, bool, error) {
	if user == "" || d.store == nil {
		return runner.Decision{}, false, nil
	}
	prefs, err := d.store.ToolPreferences(ctx, user)
	if err != nil {
		return runner.Decision{}, false, err
	}
	if rule, ok := toolrules.Match(prefs, call); ok {
		return runner.Decision{Decision: toolrules.Decision{Allow: true, Entry: rule}, By: byPreference, User: user}, true, nil
	}
	grants, err := d.store.ToolGrants(ctx, contextID, user)
	if err != nil {
		return runner.Decision{}, false, err
	}
	if rule, ok := toolrules.Match(grants, call); ok {
		return runner.Decision{Decision: toolrules.Decision{Allow: true, Entry: rule}, By: byGrant, User: user}, true, nil
	}
	return runner.Decision{}, false, nil
}

// decideWithRules answers call with the rules of the config, then as
// decide does.
func (s *Server) decideWithRules(ctx context.Context, contextID, user string, call toolrules.Call) (runner.Decision, bool, error) {
	if dec, ok := agentcfg.ToolRules(s.rt.Config()).Decide(call); ok {
		return runner.Decision{Decision: dec, By: runner.ByRule}, true, nil
	}
	return s.decider.decide(ctx, contextID, user, call)
}
