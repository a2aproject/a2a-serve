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

//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/google/go-cmp/cmp"
)

func TestScheduleFiresAndPostsTurn(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")

	coder.serve("schedule", "add", "soon", "--at", time.Now().Add(3*time.Second).Format(time.RFC3339), "--prompt", "scheduled hello")

	want := taskResult{State: a2a.TaskStateCompleted, Text: "ran Scheduled run of \"soon\""}
	eventually(t, "waiting for the scheduled turn", func() (bool, string) {
		tasks := owner.mustListTasks(coder.url, "--with-artifacts")
		if len(tasks) != 1 {
			return false, fmt.Sprintf("%d tasks", len(tasks))
		}
		got := resultOf(tasks[0])
		got.Text, _, _ = strings.Cut(got.Text, " (")
		return got == want, fmt.Sprintf("%+v", got)
	})
	if all := jsonl[struct{}](t, coder.serve("schedule", "list", "-o", "jsonl")); len(all) != 0 {
		t.Fatalf("a2a-serve schedule list after a one-time run = %d schedules, want none", len(all))
	}
}

func TestContactLateResultArrivesAsNewTurn(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder, reviewer := owner.startAgent("coder"), owner.startAgent("reviewer")
	coder.serve("contact", "add", "reviewer", "--url", reviewer.url, "--code", reviewer.invite("coder"))

	first := owner.mustSend(coder.url, "acall reviewer hello")
	reviewerTask := calledTaskID(t, first)

	turn := waitNewTurn(t, owner, coder, first)
	want := fmt.Sprintf("[reviewer finished task %s: completed]\nHello from fakeacp.", reviewerTask)
	if got := historyText(turn); got != want {
		t.Fatalf("posted turn message = %q, want %q", got, want)
	}
}

func TestRelayedApprovalAnsweredByUser(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder, reviewer := owner.startAgent("coder"), owner.startAgent("reviewer")
	coder.serve("contact", "add", "reviewer", "--url", reviewer.url, "--code", reviewer.invite("coder"))

	first := owner.mustSend(coder.url, "acall reviewer ask")
	reviewerTask := calledTaskID(t, first)
	in := coder.inspect()
	a := in.waitApproval("reviewer", "git push")
	if got := in.answer(a, "once", a.CSRF); got != http.StatusOK {
		t.Fatalf("POST %s = %d, want %d after the redirect to the overview", a.Action, got, http.StatusOK)
	}

	eventually(t, "waiting for the reviewer's task", func() (bool, string) {
		got := resultOf(owner.mustGetTask(reviewer.url, reviewerTask))
		return got == taskResult{State: a2a.TaskStateCompleted, Text: "Pushed (allow)."}, fmt.Sprintf("%+v", got)
	})
	turn := waitNewTurn(t, owner, coder, first)
	want := fmt.Sprintf("[reviewer finished task %s: completed]\nPushed (allow).", reviewerTask)
	if got := historyText(turn); got != want {
		t.Fatalf("posted turn message = %q, want %q", got, want)
	}
}

func TestRestartFailsOpenTasks(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	task := owner.mustSend(coder.url, "ask")

	coder.serve("restart")

	got := owner.mustGetTask(coder.url, string(task.ID))
	if got.Status.State != a2a.TaskStateFailed || !strings.Contains(a2atext.Message(got.Status.Message), "restarted") {
		t.Fatalf("a2a task get after the restart = %v %q, want failed by the restart", got.Status.State, a2atext.Message(got.Status.Message))
	}
	if all := coder.inspect().approvals(); len(all) != 0 {
		t.Fatalf("inspector approvals after the restart = %v, want none", texts(all))
	}
	if _, err := owner.send(coder.url, "--task-id", string(task.ID), "once"); err == nil {
		t.Fatalf("a2a send --task-id of a failed task error = nil, want error")
	}
}

func TestAgentProcessCrash(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	first := owner.mustSend(coder.url, "remember banana")

	crashed := owner.mustSend(coder.url, "--context-id", first.ContextID, "crash")
	if crashed.Status.State != a2a.TaskStateFailed {
		t.Fatalf("a2a send crash state = %v, want %v", crashed.Status.State, a2a.TaskStateFailed)
	}

	got := resultOf(owner.mustSend(coder.url, "--context-id", first.ContextID, "recall"))
	want := taskResult{State: a2a.TaskStateCompleted, Text: "You said: banana"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("a2a send recall after the crash wrong result (-want +got) diff = %s", diff)
	}
}

// waitNewTurn waits for a finished turn in the conversation of first that
// first's sender didn't send, and returns it.
func waitNewTurn(t *testing.T, u *user, a *agent, first *a2a.Task) *a2a.Task {
	t.Helper()
	var turn *a2a.Task
	eventually(t, "waiting for a posted turn", func() (bool, string) {
		tasks := u.mustListTasks(a.url, "--context", first.ContextID, "--history", "1")
		for _, task := range tasks {
			if task.ID != first.ID && task.Status.State.Terminal() {
				turn = task
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d tasks", len(tasks))
	})
	return turn
}

// historyText is the text of the first history message of task, the message
// that started it.
func historyText(task *a2a.Task) string {
	if len(task.History) == 0 {
		return ""
	}
	return a2atext.Message(task.History[0])
}
