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
	"testing"

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
