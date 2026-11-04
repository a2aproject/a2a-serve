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
	"fmt"
	"io"
	"iter"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/shlex"
	"github.com/spf13/pflag"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/schedule"
)

// schedulesAgent is the built-in agent the harness creates, lists and
// removes schedules with, through the agents URL of a conversation. A
// schedule made there runs as the conversation's user and reports back in
// the same conversation.
const schedulesAgent = pairings.SchedulesAgent

const schedulesUsage = `Commands (one per message):
  add <name> (--cron "<expr>" | --at "<YYYY-MM-DD HH:MM>") [--tz <zone>] --prompt "<prompt>"
  list
  rm <name>
A run sends the prompt to you in this conversation, as the user who asked.`

// schedulesExecutor runs the commands the harness sends to the schedules
// agent for the conversation and user of claims.
type schedulesExecutor struct {
	sc     *schedules
	claims callerClaims
}

func (e *schedulesExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil && !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		state := a2a.TaskStateCompleted
		text, err := e.run(ctx, output.MessageText(execCtx.Message))
		if err != nil {
			state, text = a2a.TaskStateFailed, err.Error()
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, state, a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(text))), nil)
	}
}

func (e *schedulesExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// user is who the commands act for: the user of the conversation's turn.
// Conversations started through another agent and anonymous callers can't
// manage schedules.
func (e *schedulesExecutor) user() (auth.Principal, error) {
	u := e.claims.User
	if u.Hops > 0 || !mayCallAgents(u) {
		return auth.Principal{}, errors.New("schedules can only be managed in a conversation with a user of this agent")
	}
	return u, nil
}

func (e *schedulesExecutor) run(ctx context.Context, text string) (string, error) {
	args, err := shlex.Split(text)
	if err != nil {
		return "", fmt.Errorf("parsing the command: %w\n\n%s", err, schedulesUsage)
	}
	if len(args) == 0 || args[0] == "help" {
		return schedulesUsage, nil
	}
	who, err := e.user()
	if err != nil {
		return "", err
	}
	switch args[0] {
	case "add":
		return e.add(ctx, who, args[1:])
	case "list":
		return e.list(ctx, who)
	case "rm":
		if len(args) != 2 {
			return "", errors.New("usage: rm <name>")
		}
		if err := e.sc.remove(ctx, args[1], who); err != nil {
			return "", err
		}
		return "Removed " + args[1] + ".", nil
	}
	return "", fmt.Errorf("unknown command %q\n\n%s", args[0], schedulesUsage)
}

func (e *schedulesExecutor) add(ctx context.Context, who auth.Principal, args []string) (string, error) {
	var req schedule.Request
	fs := pflag.NewFlagSet("add", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	req.RegisterFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return "", fmt.Errorf("%w\n\n%s", err, schedulesUsage)
	}
	if fs.NArg() != 1 {
		return "", fmt.Errorf("add takes one name\n\n%s", schedulesUsage)
	}
	req.Name = fs.Arg(0)
	s, err := e.sc.add(ctx, req, who, e.claims.ContextID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Scheduled %s; the next run is %s.", s.Name, describeTime(s.NextRun, s.TZ)), nil
}

func (e *schedulesExecutor) list(ctx context.Context, who auth.Principal) (string, error) {
	all, err := e.sc.visible(ctx, who)
	if err != nil {
		return "", err
	}
	if len(all) == 0 {
		return "No schedules.", nil
	}
	var sb strings.Builder
	for _, s := range all {
		when := s.Cron
		if when == "" {
			when = "once"
		}
		fmt.Fprintf(&sb, "- %s (%s, next %s): %s\n", s.Name, when, describeTime(s.NextRun, s.TZ), s.Prompt)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// describeTime formats t in the time zone tz, or the server's.
func describeTime(t time.Time, tz string) string {
	if t.IsZero() {
		return "never"
	}
	loc, err := schedule.Location(tz)
	if err != nil {
		loc = time.Local
	}
	return t.In(loc).Format("Mon 2006-01-02 15:04 MST")
}
