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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
)

func TestOwnerServesAndCallsAgent(t *testing.T) {
	t.Parallel()
	dependsOnSandbox(t)
	owner := newUser(t)
	coder := owner.startAgent("coder")

	if !coder.running() {
		t.Fatalf("a2a-serve status running = false after start, want true")
	}
	got := resultOf(owner.mustSend(coder.url, "hello"))
	want := taskResult{State: a2a.TaskStateCompleted, Text: "Hello from fakeacp."}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("a2a send hello wrong result (-want +got) diff = %s", diff)
	}

	coder.stop()
	if coder.running() {
		t.Fatalf("a2a-serve status running = true after stop, want false")
	}
}

func TestRestartStartsStoppedAgent(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	coder.stop()

	coder.serve("restart")
	coder.stopped = false

	if !coder.running() {
		t.Fatalf("a2a-serve status running = false after restart, want true")
	}
}

// TestStopWithoutState covers stop in an agent directory whose
// .a2a/state is gone: the server exits, and stop returns at once.
func TestStopWithoutState(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	if err := os.RemoveAll(filepath.Join(coder.dir, ".a2a", "state")); err != nil {
		t.Fatalf("os.RemoveAll() error = %v", err)
	}
	eventually(t, "the server exits without its state", func() (bool, string) {
		st := coder.status()
		return !st.Running, fmt.Sprintf("%+v", st)
	})

	start := time.Now()
	out := coder.serve("stop")
	coder.stopped = true
	if !strings.Contains(out, "coder is not running") {
		t.Fatalf("a2a-serve stop without state = %q, want %q", out, "coder is not running")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("a2a-serve stop without state took %v, want it to return at once", elapsed)
	}
}

// TestConcurrentStarts covers several start --detach at once in a stopped
// agent directory: one server comes up, and every starter either reports
// it running or fails with "already running".
func TestConcurrentStarts(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	coder.stop()

	const starters = 3
	type result struct {
		out string
		err error
	}
	results := make(chan result, starters)
	for range starters {
		go func() {
			out, err := owner.exec(coder.dir, "a2a", "serve", "start", "--detach", "--quiet")
			results <- result{out, err}
		}()
	}
	started := 0
	for range starters {
		r := <-results
		coder.stopped = false
		switch {
		case r.err == nil && strings.Contains(r.out, "Error"):
			t.Errorf("a2a-serve start --detach = %q, want no error output", r.out)
		case r.err == nil:
			started++
		case !strings.Contains(r.err.Error(), "already running"):
			t.Errorf("a2a-serve start --detach error = %v, want success or an already running error", r.err)
		}
	}
	if started == 0 {
		t.Fatalf("a2a-serve start --detach succeeded for none of %d concurrent starters, want at least one", starters)
	}
	if !coder.running() {
		t.Fatalf("a2a-serve status running = false after the concurrent starts, want true")
	}
}

func TestConfigRestartAppliesPendingSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "set an unchanged value", args: []string{"config", "set", "--env", "DEBUG=1", "--restart"}},
		{name: "unset a setting that isn't set", args: []string{"config", "unset", "task-ttl", "--restart"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner := newUser(t)
			coder := owner.startAgent("coder")
			coder.serve("config", "set", "--env", "DEBUG=1")
			before := coder.status()
			if diff := cmp.Diff([]string{"env"}, before.Pending); diff != "" {
				t.Fatalf("a2a-serve status pending wrong result (-want +got) diff = %s", diff)
			}

			coder.serve(tc.args...)

			after := coder.status()
			if !after.Running || after.PID == before.PID || len(after.Pending) > 0 {
				t.Fatalf("a2a-serve status after a2a-serve %s = %+v, want a restarted agent (pid %d) with nothing pending", strings.Join(tc.args, " "), after, before.PID)
			}
		})
	}
}

func TestOwnerAnswersApproval(t *testing.T) {
	t.Parallel()
	dependsOnSandbox(t)
	tests := []struct {
		name   string
		answer string
		want   string
	}{
		{name: "allow once", answer: "once", want: "Pushed (allow)."},
		{name: "reject", answer: "reject", want: "Push denied."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner := newUser(t)
			coder := owner.startAgent("coder")

			task := owner.mustSend(coder.url, "ask")
			if task.Status.State != a2a.TaskStateInputRequired {
				t.Fatalf("a2a send ask state = %v, want %v", task.Status.State, a2a.TaskStateInputRequired)
			}
			got := resultOf(owner.mustSend(coder.url, "--task-id", string(task.ID), tc.answer))
			want := taskResult{State: a2a.TaskStateCompleted, Text: tc.want}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("a2a send --task-id %s wrong result (-want +got) diff = %s", tc.answer, diff)
			}
		})
	}
}

func TestRestartResumesConversation(t *testing.T) {
	t.Parallel()
	dependsOnSandbox(t)
	owner := newUser(t)
	coder := owner.startAgent("coder")
	task := owner.mustSend(coder.url, "remember banana")

	coder.serve("restart")

	got := resultOf(owner.mustSend(coder.url, "--context-id", task.ContextID, "recall"))
	want := taskResult{State: a2a.TaskStateCompleted, Text: "You said: banana"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("a2a send recall after restart wrong result (-want +got) diff = %s", diff)
	}
}
