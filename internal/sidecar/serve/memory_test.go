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

package serve

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
)

// fakeClock is a clock that the test moves.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// step is a change to a memory: a task created or updated at state, or a
// session put, after the clock moved by wait.
type step struct {
	wait    time.Duration
	context string
	task    string
	state   a2a.TaskState
	text    string
	session bool
}

func run(t *testing.T, m *memory, clock *fakeClock, steps []step) {
	t.Helper()
	ctx := context.Background()
	for _, s := range steps {
		clock.advance(s.wait)
		if s.session {
			if err := m.sessions().Put(ctx, s.context, a2aserve.Session{ID: "sess-" + s.context, Key: s.context}); err != nil {
				t.Fatalf("Sessions.Put(%q) error = %v", s.context, err)
			}
			continue
		}
		task := &a2a.Task{ID: a2a.TaskID(s.task), ContextID: s.context, Status: a2a.TaskStatus{State: s.state}}
		if s.text != "" {
			task.Artifacts = []*a2a.Artifact{{ID: "a", Parts: []*a2a.Part{a2a.NewTextPart(s.text)}}}
		}
		stored, err := m.Get(ctx, task.ID)
		if errors.Is(err, a2a.ErrTaskNotFound) {
			if _, err := m.Create(ctx, task); err != nil {
				t.Fatalf("memory.Create(%q) error = %v", s.task, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("memory.Get(%q) error = %v", s.task, err)
		}
		if _, err := m.Update(ctx, &taskstore.UpdateRequest{Task: task, PrevTask: stored.Task, PrevVersion: stored.Version}); err != nil {
			t.Fatalf("memory.Update(%q) error = %v", s.task, err)
		}
	}
}

// kept returns the tasks of m and the contexts that have a session, sorted.
func kept(t *testing.T, m *memory) (tasks, sessions []string) {
	t.Helper()
	m.mu.Lock()
	ids := make([]a2a.TaskID, 0, len(m.tasks))
	for id := range m.tasks {
		ids = append(ids, id)
	}
	var contexts []string
	for id := range m.contexts {
		contexts = append(contexts, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		if _, err := m.Get(context.Background(), id); err != nil {
			t.Fatalf("memory.Get(%q) error = %v", id, err)
		}
		tasks = append(tasks, string(id))
	}
	for _, id := range contexts {
		if _, ok, err := m.sessions().Get(context.Background(), id); err != nil || ok {
			if err != nil {
				t.Fatalf("Sessions.Get(%q) error = %v", id, err)
			}
			sessions = append(sessions, id)
		}
	}
	slices.Sort(tasks)
	slices.Sort(sessions)
	return tasks, sessions
}

func TestMemoryForgets(t *testing.T) {
	t.Parallel()
	limits := memoryLimits{Idle: time.Hour, Contexts: 2, TasksPerContext: 2, Bytes: 1 << 20}
	const (
		working   = a2a.TaskStateWorking
		input     = a2a.TaskStateInputRequired
		completed = a2a.TaskStateCompleted
		failed    = a2a.TaskStateFailed
	)
	tests := []struct {
		name         string
		limits       memoryLimits
		steps        []step
		wantTasks    []string
		wantSessions []string
	}{
		{
			name: "an ended context after its idle time",
			steps: []step{
				{context: "a", session: true},
				{context: "a", task: "a1", state: completed},
				{wait: 59 * time.Minute, context: "b", task: "b1", state: completed},
				{wait: 2 * time.Minute, context: "b", task: "b2", state: completed},
			},
			wantTasks: []string{"b1", "b2"},
		},
		{
			name: "no context before its idle time",
			steps: []step{
				{context: "a", session: true},
				{context: "a", task: "a1", state: completed},
				{wait: 59 * time.Minute, context: "b", task: "b1", state: completed},
			},
			wantTasks:    []string{"a1", "b1"},
			wantSessions: []string{"a"},
		},
		{
			name: "no context with a task that works or waits for an answer",
			steps: []step{
				{context: "a", task: "a1", state: working},
				{context: "b", task: "b1", state: input},
				{context: "c", session: true},
				{wait: 2 * time.Hour, context: "c", task: "c1", state: completed},
				{wait: 2 * time.Hour, context: "d", task: "d1", state: completed},
			},
			wantTasks: []string{"a1", "b1", "d1"},
		},
		{
			name: "the least recently changed ended context past the context limit",
			steps: []step{
				{context: "a", task: "a1", state: completed},
				{wait: time.Second, context: "b", task: "b1", state: failed},
				{wait: time.Second, context: "a", session: true},
				{wait: time.Second, context: "c", task: "c1", state: working},
			},
			wantTasks:    []string{"a1", "c1"},
			wantSessions: []string{"a"},
		},
		{
			name: "an ended context once it changed again",
			steps: []step{
				{context: "a", task: "a1", state: working},
				{wait: time.Second, context: "b", task: "b1", state: working},
				{wait: time.Second, context: "a", task: "a1", state: completed},
				{wait: time.Second, context: "c", task: "c1", state: working},
			},
			wantTasks: []string{"b1", "c1"},
		},
		{
			name: "the oldest ended tasks of a context past the task limit",
			steps: []step{
				{context: "a", task: "a1", state: completed},
				{context: "a", task: "a2", state: input},
				{context: "a", task: "a3", state: completed},
				{context: "a", task: "a4", state: working},
			},
			wantTasks: []string{"a2", "a4"},
		},
		{
			name:   "ended contexts past the size limit",
			limits: memoryLimits{Idle: time.Hour, Contexts: 10, TasksPerContext: 10, Bytes: 800},
			steps: []step{
				{context: "a", task: "a1", state: completed, text: strings.Repeat("x", 250)},
				{wait: time.Second, context: "b", task: "b1", state: completed, text: strings.Repeat("x", 250)},
				{wait: time.Second, context: "c", task: "c1", state: working, text: strings.Repeat("x", 250)},
			},
			wantTasks: []string{"b1", "c1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			m := newMemory(limits, clock.now)
			if tc.limits != (memoryLimits{}) {
				m = newMemory(tc.limits, clock.now)
			}

			run(t, m, clock, tc.steps)

			tasks, sessions := kept(t, m)
			if diff := cmp.Diff(tc.wantTasks, tasks); diff != "" {
				t.Fatalf("memory tasks wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantSessions, sessions); diff != "" {
				t.Fatalf("memory sessions wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestMemoryStoresTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := newMemory(defaultMemoryLimits, clock.now)
	task := &a2a.Task{ID: "t1", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	if _, err := m.Create(ctx, task); err != nil {
		t.Fatalf("memory.Create() error = %v", err)
	}

	if _, err := m.Create(ctx, task); !errors.Is(err, taskstore.ErrTaskAlreadyExists) {
		t.Fatalf("memory.Create() again error = %v, want %v", err, taskstore.ErrTaskAlreadyExists)
	}
	task.Status.State = a2a.TaskStateCompleted
	if _, err := m.Update(ctx, &taskstore.UpdateRequest{Task: task, PrevVersion: 7}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("memory.Update() with a stale version error = %v, want %v", err, taskstore.ErrConcurrentModification)
	}
	v, err := m.Update(ctx, &taskstore.UpdateRequest{Task: task, PrevVersion: 1})
	if err != nil || v != 2 {
		t.Fatalf("memory.Update() = %d, %v, want 2, nil", v, err)
	}
	task.Status.State = a2a.TaskStateFailed
	got, err := m.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("memory.Get() error = %v", err)
	}
	if diff := cmp.Diff(&taskstore.StoredTask{Task: &a2a.Task{ID: "t1", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}, Version: 2}, got); diff != "" {
		t.Fatalf("memory.Get() wrong result (-want +got) diff = %s", diff)
	}
	if _, err := m.Get(ctx, "missing"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("memory.Get(missing) error = %v, want %v", err, a2a.ErrTaskNotFound)
	}
}

func TestMemoryListsNewestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := newMemory(defaultMemoryLimits, clock.now)
	run(t, m, clock, []step{
		{context: "a", task: "t1", state: a2a.TaskStateCompleted},
		{wait: time.Second, context: "b", task: "t2", state: a2a.TaskStateWorking},
		{wait: time.Second, context: "a", task: "t3", state: a2a.TaskStateCompleted},
	})
	ids := func(resp *a2a.ListTasksResponse) []string {
		var out []string
		for _, task := range resp.Tasks {
			out = append(out, string(task.ID))
		}
		return out
	}

	first, err := m.List(ctx, &a2a.ListTasksRequest{PageSize: 2})
	if err != nil {
		t.Fatalf("memory.List() error = %v", err)
	}
	second, err := m.List(ctx, &a2a.ListTasksRequest{PageSize: 2, PageToken: first.NextPageToken})
	if err != nil {
		t.Fatalf("memory.List(page 2) error = %v", err)
	}
	inA, err := m.List(ctx, &a2a.ListTasksRequest{ContextID: "a"})
	if err != nil {
		t.Fatalf("memory.List(context a) error = %v", err)
	}

	got := [][]string{ids(first), ids(second), ids(inA)}
	if diff := cmp.Diff([][]string{{"t3", "t2"}, {"t1"}, {"t3", "t1"}}, got); diff != "" {
		t.Fatalf("memory.List() wrong result (-want +got) diff = %s", diff)
	}
	if second.NextPageToken != "" || first.TotalSize != 3 {
		t.Fatalf("memory.List() total = %d, last page token = %q, want 3 and none", first.TotalSize, second.NextPageToken)
	}
}
