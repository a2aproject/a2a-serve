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

package contact

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"
)

func TestGuardRefusesWhatTheConversationDidNotStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name      string
		contextID string
		call      func(g a2asrv.RequestHandler) error
		want      string
		wantErr   error
	}{
		{name: "get its task", contextID: "c1", want: "GetTask", call: func(g a2asrv.RequestHandler) error {
			_, err := g.GetTask(ctx, &a2a.GetTaskRequest{ID: "t1"})
			return err
		}},
		{name: "get another conversation's task", contextID: "c2", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			_, err := g.GetTask(ctx, &a2a.GetTaskRequest{ID: "t1"})
			return err
		}},
		{name: "get a task nobody started", contextID: "c1", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			_, err := g.GetTask(ctx, &a2a.GetTaskRequest{ID: "t2"})
			return err
		}},
		{name: "cancel its task", contextID: "c1", want: "CancelTask", call: func(g a2asrv.RequestHandler) error {
			_, err := g.CancelTask(ctx, &a2a.CancelTaskRequest{ID: "t1"})
			return err
		}},
		{name: "cancel another conversation's task", contextID: "c2", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			_, err := g.CancelTask(ctx, &a2a.CancelTaskRequest{ID: "t1"})
			return err
		}},
		{name: "subscribe to its task", contextID: "c1", want: "SubscribeToTask", call: func(g a2asrv.RequestHandler) error {
			for _, err := range g.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: "t1"}) {
				return err
			}
			return nil
		}},
		{name: "subscribe to another conversation's task", contextID: "c2", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			for _, err := range g.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: "t1"}) {
				return err
			}
			return nil
		}},
		{name: "get push config of its task", contextID: "c1", want: "GetTaskPushConfig", call: func(g a2asrv.RequestHandler) error {
			_, err := g.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{TaskID: "t1"})
			return err
		}},
		{name: "get push config of another conversation's task", contextID: "c2", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			_, err := g.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{TaskID: "t1"})
			return err
		}},
		{name: "list push configs of another conversation's task", contextID: "c2", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			_, err := g.ListTaskPushConfigs(ctx, &a2a.ListTaskPushConfigRequest{TaskID: "t1"})
			return err
		}},
		{name: "create push config of another conversation's task", contextID: "c2", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			_, err := g.CreateTaskPushConfig(ctx, &a2a.PushConfig{TaskID: "t1"})
			return err
		}},
		{name: "delete push config of another conversation's task", contextID: "c2", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			return g.DeleteTaskPushConfig(ctx, &a2a.DeleteTaskPushConfigRequest{TaskID: "t1"})
		}},
		{name: "continue its task", contextID: "c1", want: "SendStreamingMessage", call: func(g a2asrv.RequestHandler) error {
			_, err := g.SendMessage(ctx, message("t1", ""))
			return err
		}},
		{name: "continue another conversation's task", contextID: "c2", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			_, err := g.SendMessage(ctx, message("t1", "r1"))
			return err
		}},
		{name: "join its context", contextID: "c1", want: "SendStreamingMessage", call: func(g a2asrv.RequestHandler) error {
			_, err := g.SendMessage(ctx, message("", "r1"))
			return err
		}},
		{name: "join another conversation's context", contextID: "c2", wantErr: a2a.ErrUnauthorized, call: func(g a2asrv.RequestHandler) error {
			_, err := g.SendMessage(ctx, message("", "r1"))
			return err
		}},
		{name: "join a context nobody started", contextID: "c1", wantErr: a2a.ErrUnauthorized, call: func(g a2asrv.RequestHandler) error {
			_, err := g.SendMessage(ctx, message("", "r2"))
			return err
		}},
		{name: "reference its task", contextID: "c1", want: "SendStreamingMessage", call: func(g a2asrv.RequestHandler) error {
			req := message("", "")
			req.Message.ReferenceTasks = []a2a.TaskID{"t1"}
			_, err := g.SendMessage(ctx, req)
			return err
		}},
		{name: "reference another conversation's task", contextID: "c2", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			req := message("", "")
			req.Message.ReferenceTasks = []a2a.TaskID{"t1"}
			_, err := g.SendMessage(ctx, req)
			return err
		}},
		{name: "reference its task and a task nobody started", contextID: "c1", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			req := message("", "")
			req.Message.ReferenceTasks = []a2a.TaskID{"t1", "t2"}
			_, err := g.SendMessage(ctx, req)
			return err
		}},
		{name: "push config for its task", contextID: "c1", want: "SendStreamingMessage", call: func(g a2asrv.RequestHandler) error {
			req := message("t1", "")
			req.Config = &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{TaskID: "t1", URL: "https://hook.example"}}
			_, err := g.SendMessage(ctx, req)
			return err
		}},
		{name: "push config without a task", contextID: "c2", want: "SendStreamingMessage", call: func(g a2asrv.RequestHandler) error {
			req := message("", "")
			req.Config = &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: "https://hook.example"}}
			_, err := g.SendMessage(ctx, req)
			return err
		}},
		{name: "push config for another conversation's task", contextID: "c2", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			req := message("", "")
			req.Config = &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{TaskID: "t1", URL: "https://hook.example"}}
			_, err := g.SendMessage(ctx, req)
			return err
		}},
		{name: "stream with a reference to another conversation's task", contextID: "c2", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			req := message("", "")
			req.Message.ReferenceTasks = []a2a.TaskID{"t1"}
			for _, err := range g.SendStreamingMessage(ctx, req) {
				return err
			}
			return nil
		}},
		{name: "start a new task", contextID: "c2", want: "SendStreamingMessage", call: func(g a2asrv.RequestHandler) error {
			_, err := g.SendMessage(ctx, message("", ""))
			return err
		}},
		{name: "stream to another conversation's task", contextID: "c2", wantErr: a2a.ErrTaskNotFound, call: func(g a2asrv.RequestHandler) error {
			for _, err := range g.SendStreamingMessage(ctx, message("t1", "r1")) {
				return err
			}
			return nil
		}},
		{name: "stream into another conversation's context", contextID: "c2", wantErr: a2a.ErrUnauthorized, call: func(g a2asrv.RequestHandler) error {
			for _, err := range g.SendStreamingMessage(ctx, message("", "r1")) {
				return err
			}
			return nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := newMemoryCalls()
			recordStarted(t, calls, "c1", "t1", "r1")
			agent := &recordingAgent{task: completedTask("t1", "r1")}

			err := tc.call(guarded(t, calls, agent, tc.contextID))

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("guard call error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if diff := cmp.Diff([]string(nil), agent.calls()); diff != "" {
					t.Fatalf("calls that reached the agent after a refusal wrong result (-want +got) diff = %s", diff)
				}
				return
			}
			if got := agent.calls(); len(got) == 0 || got[len(got)-1] != tc.want {
				t.Fatalf("calls that reached the agent = %v, want %s last", got, tc.want)
			}
		})
	}
}

func TestGuardRefusesTaskOfAnotherAgent(t *testing.T) {
	t.Parallel()
	calls := newMemoryCalls()
	recordStarted(t, calls, "c1", "t1", "r1")
	g := guardOf(t, calls, &recordingAgent{task: completedTask("t1", "r1")}, "c1", "grok")

	_, err := g.GetTask(context.Background(), &a2a.GetTaskRequest{ID: "t1"})

	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("guard.GetTask() of a task started at another agent error = %v, want %v", err, a2a.ErrTaskNotFound)
	}
}

func TestGuardListsOnlyItsTasks(t *testing.T) {
	t.Parallel()
	calls := newMemoryCalls()
	recordStarted(t, calls, "c1", "t1", "r1")
	recordStarted(t, calls, "c2", "t2", "r2")
	agent := &recordingAgent{listed: []*a2a.Task{completedTask("t1", "r1"), completedTask("t2", "r2"), completedTask("t3", "r3")}}
	tests := []struct {
		name      string
		contextID string
		want      []a2a.TaskID
	}{
		{name: "first conversation", contextID: "c1", want: []a2a.TaskID{"t1"}},
		{name: "second conversation", contextID: "c2", want: []a2a.TaskID{"t2"}},
		{name: "conversation without tasks", contextID: "c3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := guarded(t, calls, agent, tc.contextID).ListTasks(context.Background(), &a2a.ListTasksRequest{})
			if err != nil {
				t.Fatalf("guard.ListTasks() error = %v", err)
			}
			var got []a2a.TaskID
			for _, task := range res.Tasks {
				got = append(got, task.ID)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("guard.ListTasks() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestGuardListTasksPagesOnlyItsTasks(t *testing.T) {
	t.Parallel()
	calls := newMemoryCalls()
	recordStarted(t, calls, "c1", "t2", "r2")
	recordStarted(t, calls, "c1", "t5", "r5")
	recordStarted(t, calls, "c1", "t6", "r6")
	var listed []*a2a.Task
	for _, id := range []a2a.TaskID{"t1", "t2", "t3", "t4", "t5", "t6", "t7"} {
		listed = append(listed, completedTask(id, "r"+string(id[1:])))
	}
	listed[5].Status.State = a2a.TaskStateWorking
	agent := &recordingAgent{listed: listed}
	g := guarded(t, calls, agent, "c1")
	tests := []struct {
		name          string
		req           a2a.ListTasksRequest
		wantIDs       []a2a.TaskID
		wantTotalSize int
		wantNextPage  bool
	}{
		{name: "everything on one page", wantIDs: []a2a.TaskID{"t6", "t5", "t2"}, wantTotalSize: 3},
		{name: "first page is not empty when others' tasks come first", req: a2a.ListTasksRequest{PageSize: 2}, wantIDs: []a2a.TaskID{"t6", "t5"}, wantTotalSize: 3, wantNextPage: true},
		{name: "page of one", req: a2a.ListTasksRequest{PageSize: 1}, wantIDs: []a2a.TaskID{"t6"}, wantTotalSize: 3, wantNextPage: true},
		{name: "remote context", req: a2a.ListTasksRequest{ContextID: "r5"}, wantIDs: []a2a.TaskID{"t5"}, wantTotalSize: 1},
		{name: "status", req: a2a.ListTasksRequest{Status: a2a.TaskStateWorking}, wantIDs: []a2a.TaskID{"t6"}, wantTotalSize: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := g.ListTasks(context.Background(), &tc.req)
			if err != nil {
				t.Fatalf("guard.ListTasks() error = %v", err)
			}
			var got []a2a.TaskID
			for _, task := range res.Tasks {
				got = append(got, task.ID)
			}
			if diff := cmp.Diff(tc.wantIDs, got); diff != "" {
				t.Fatalf("guard.ListTasks() wrong tasks (-want +got) diff = %s", diff)
			}
			if res.TotalSize != tc.wantTotalSize {
				t.Fatalf("guard.ListTasks() TotalSize = %d, want %d", res.TotalSize, tc.wantTotalSize)
			}
			if (res.NextPageToken != "") != tc.wantNextPage {
				t.Fatalf("guard.ListTasks() NextPageToken = %q, want a token = %v", res.NextPageToken, tc.wantNextPage)
			}
		})
	}
}

func TestGuardListTasksFollowsPageTokens(t *testing.T) {
	t.Parallel()
	calls := newMemoryCalls()
	var listed []*a2a.Task
	for i := range 7 {
		id := a2a.TaskID(fmt.Sprintf("t%d", i))
		if i%2 == 0 {
			recordStarted(t, calls, "c1", string(id), "r"+string(id))
		}
		listed = append(listed, completedTask(id, "r"+string(id)))
	}
	g := guarded(t, calls, &recordingAgent{listed: listed}, "c1")

	var got []a2a.TaskID
	req := &a2a.ListTasksRequest{PageSize: 3}
	for pages := 0; ; pages++ {
		if pages > 3 {
			t.Fatalf("guard.ListTasks() kept returning pages, want it to end")
		}
		res, err := g.ListTasks(context.Background(), req)
		if err != nil {
			t.Fatalf("guard.ListTasks() error = %v", err)
		}
		for _, task := range res.Tasks {
			got = append(got, task.ID)
		}
		if res.NextPageToken == "" {
			break
		}
		req = &a2a.ListTasksRequest{PageSize: 3, PageToken: res.NextPageToken}
	}

	if diff := cmp.Diff([]a2a.TaskID{"t6", "t4", "t2", "t0"}, got); diff != "" {
		t.Fatalf("guard.ListTasks() pages wrong result (-want +got) diff = %s", diff)
	}
}

func TestGuardListTasksGetsOnlyThePage(t *testing.T) {
	t.Parallel()
	calls := newMemoryCalls()
	var listed []*a2a.Task
	for i := range 5 {
		id := a2a.TaskID(fmt.Sprintf("t%d", i))
		recordStarted(t, calls, "c1", string(id), "r"+string(id))
		task := completedTask(id, "r"+string(id))
		task.Artifacts = []*a2a.Artifact{{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart("result")}}}
		listed = append(listed, task)
	}
	agent := &recordingAgent{listed: listed}
	g := guarded(t, calls, agent, "c1")

	res, err := g.ListTasks(context.Background(), &a2a.ListTasksRequest{PageSize: 2, HistoryLength: new(3)})
	if err != nil {
		t.Fatalf("guard.ListTasks() error = %v", err)
	}

	if diff := cmp.Diff([]string{"GetTask", "GetTask"}, agent.calls()); diff != "" {
		t.Fatalf("calls that reached the agent wrong result (-want +got) diff = %s", diff)
	}
	agent.mu.Lock()
	gotLengths := agent.historyLengths
	agent.mu.Unlock()
	if diff := cmp.Diff([]int{3, 3}, gotLengths); diff != "" {
		t.Fatalf("GetTask() history lengths wrong result (-want +got) diff = %s", diff)
	}
	for _, task := range res.Tasks {
		if task.Artifacts != nil {
			t.Fatalf("guard.ListTasks() task %s artifacts = %v, want none without IncludeArtifacts", task.ID, task.Artifacts)
		}
	}
	if listed[4].Artifacts == nil {
		t.Fatalf("guard.ListTasks() dropped the artifacts of the agent's task, want them kept")
	}
}

func TestGuardListTasksAsksForNoHistoryByDefault(t *testing.T) {
	t.Parallel()
	calls := newMemoryCalls()
	recordStarted(t, calls, "c1", "t1", "r1")
	agent := &recordingAgent{listed: []*a2a.Task{completedTask("t1", "r1")}}
	g := guarded(t, calls, agent, "c1")

	if _, err := g.ListTasks(context.Background(), &a2a.ListTasksRequest{}); err != nil {
		t.Fatalf("guard.ListTasks() error = %v", err)
	}

	agent.mu.Lock()
	gotLengths := agent.historyLengths
	agent.mu.Unlock()
	if diff := cmp.Diff([]int{0}, gotLengths); diff != "" {
		t.Fatalf("GetTask() history lengths wrong result (-want +got) diff = %s", diff)
	}
}

func TestGuardListTasksRejectsBadPaging(t *testing.T) {
	t.Parallel()
	calls := newMemoryCalls()
	g := guarded(t, calls, &recordingAgent{}, "c1")
	tests := []struct {
		name string
		req  a2a.ListTasksRequest
	}{
		{name: "page size too large", req: a2a.ListTasksRequest{PageSize: 101}},
		{name: "negative page size", req: a2a.ListTasksRequest{PageSize: -1}},
		{name: "page token that is not a number", req: a2a.ListTasksRequest{PageToken: "abc"}},
		{name: "negative page token", req: a2a.ListTasksRequest{PageToken: "-3"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := g.ListTasks(context.Background(), &tc.req); !errors.Is(err, a2a.ErrInvalidRequest) {
				t.Fatalf("guard.ListTasks() error = %v, want %v", err, a2a.ErrInvalidRequest)
			}
		})
	}
}
