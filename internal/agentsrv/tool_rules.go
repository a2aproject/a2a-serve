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
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// toolDecider answers tool permissions with the allow and deny rules of the
// current config, so that changing them doesn't need a restart, and then
// with the preferences of the person who started the task and the grants
// of the conversation for that person. It logs each answer by what decided
// it, and notes the denied calls.
type toolDecider struct {
	config func() *agentdir.Config
	// servers returns the names of the MCP servers of the project, which
	// name the MCP tools of calls.
	servers func() []string
	notes   *toolNotes
	store   *store.Store
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

// call describes a tool call of a permission request.
func (d *toolDecider) call(kind, title string, input any) toolrules.Call {
	var servers []string
	if d.servers != nil {
		servers = d.servers()
	}
	return toolrules.NewCall(kind, title, input, servers)
}

// rules returns the allow and deny rules of the current config.
func (d *toolDecider) rules() toolrules.Rules {
	cfg := d.config()
	return toolrules.Rules{Allow: cfg.Approvals.Allow, Deny: cfg.Approvals.Deny}
}

// decision is how a call is answered without asking.
type decision struct {
	allow bool
	// entry is the rule, preference or grant that decided, and by what it
	// is.
	entry string
	by    string
	// user is the person whose preference or grant decided.
	user string
}

// What decides a call.
const (
	byRule       = "rule"
	byPreference = "preference"
	byGrant      = "grant of the conversation"
)

// decide answers call with the rules, then the preferences of user, who
// started its task, then the grants of the conversation for user. It
// returns false if none decides; without a user, only the rules decide.
func (d *toolDecider) decide(ctx context.Context, contextID, user string, call toolrules.Call) (decision, bool, error) {
	if dec, ok := d.rules().Decide(call); ok {
		return decision{allow: dec.Allow, entry: dec.Entry, by: byRule}, true, nil
	}
	if user == "" || d.store == nil {
		return decision{}, false, nil
	}
	prefs, err := d.store.ToolPreferences(ctx, user)
	if err != nil {
		return decision{}, false, err
	}
	if rule, ok := toolrules.Match(prefs, call); ok {
		return decision{allow: true, entry: rule, by: byPreference, user: user}, true, nil
	}
	grants, err := d.store.ToolGrants(ctx, contextID, user)
	if err != nil {
		return decision{}, false, err
	}
	if rule, ok := toolrules.Match(grants, call); ok {
		return decision{allow: true, entry: rule, by: byGrant, user: user}, true, nil
	}
	return decision{}, false, nil
}

func (d *toolDecider) AutoAnswer(ctx context.Context, execCtx *a2asrv.ExecutorContext, q a2aacp.Question) (a2aacp.Answer, bool) {
	if q.Tool == nil {
		return a2aacp.Answer{}, false
	}
	var contextID, user string
	var taskID a2a.TaskID
	if execCtx != nil {
		contextID, taskID = execCtx.ContextID, execCtx.TaskID
		var err error
		if user, err = d.starter(ctx, execCtx); err != nil {
			slog.Warn("looking up who started a task, asking only the rules", "task", taskID, "error", err)
		}
	}
	dec, ok, err := d.decide(ctx, contextID, user, d.call(q.Tool.Kind, q.Tool.Title, q.Tool.Input))
	if err != nil {
		slog.Warn("reading what a person allowed, asking", "task", taskID, "error", err)
		return a2aacp.Answer{}, false
	}
	if !ok {
		return a2aacp.Answer{}, false
	}
	ans, ok := ruleAnswer(q.Options, dec.allow)
	if !ok || execCtx == nil {
		return ans, ok
	}
	if !dec.allow {
		d.notes.add(execCtx.TaskID, "Tool call denied by --deny-tools: "+q.Title+".")
	}
	attrs := []any{"task", execCtx.TaskID, "rule", dec.entry, "kind", q.Tool.Kind}
	if dec.user != "" {
		attrs = append(attrs, "user", dec.user)
	}
	slog.Info("approval answered by a "+dec.by, append(attrs, "option", ans.Option)...)
	return ans, true
}

// ruleAnswer returns the option that allows a tool call once, or always if
// the agent offers no other way to allow it, or the option that rejects
// it, canceling it if the agent offers no way to reject it.
func ruleAnswer(options []string, allow bool) (a2aacp.Answer, bool) {
	if !allow {
		if id := firstOffered(options, a2aacp.OptionReject, a2aacp.OptionRejectAlways); id != "" {
			return a2aacp.Answer{Option: id}, true
		}
		return a2aacp.Answer{Option: a2aacp.OptionCancel}, true
	}
	if id := firstOffered(options, a2aacp.OptionOnce, a2aacp.OptionAlways); id != "" {
		return a2aacp.Answer{Option: id}, true
	}
	return a2aacp.Answer{}, false
}

// firstOffered returns the first of ids that options has, or "".
func firstOffered(options []string, ids ...string) string {
	for _, id := range ids {
		if slices.Contains(options, id) {
			return id
		}
	}
	return ""
}
