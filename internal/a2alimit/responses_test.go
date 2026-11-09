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

package a2alimit

import (
	"context"
	"iter"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"
)

var testLimits = Limits{Parts: 10, Text: 100, Metadata: 100, History: 2}

// fakeHandler returns task from every method and records the history
// length each request asks for.
type fakeHandler struct {
	a2asrv.RequestHandler
	task          *a2a.Task
	historyLength *int
}

func (f *fakeHandler) GetTask(_ context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	f.historyLength = req.HistoryLength
	return f.task, nil
}

func (f *fakeHandler) ListTasks(_ context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	f.historyLength = req.HistoryLength
	return &a2a.ListTasksResponse{Tasks: []*a2a.Task{f.task}, TotalSize: 1}, nil
}

func (f *fakeHandler) SendMessage(context.Context, *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	return f.task, nil
}

func (f *fakeHandler) SendStreamingMessage(context.Context, *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) { yield(f.task, nil) }
}

func historyTask() *a2a.Task {
	msg := func(id string) *a2a.Message { return &a2a.Message{ID: id, Role: a2a.MessageRoleUser} }
	return &a2a.Task{ID: "t1", History: []*a2a.Message{msg("m1"), msg("m2"), msg("m3")}}
}

func historyIDs(task *a2a.Task) []string {
	ids := []string{}
	for _, m := range task.History {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestHandlerSendMessageHistory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config *a2a.SendMessageConfig
		want   []string
	}{
		{name: "no config", want: []string{}},
		{name: "no length", config: &a2a.SendMessageConfig{}, want: []string{}},
		{name: "zero", config: &a2a.SendMessageConfig{HistoryLength: new(0)}, want: []string{}},
		{name: "last one", config: &a2a.SendMessageConfig{HistoryLength: new(1)}, want: []string{"m3"}},
		{name: "over the limit", config: &a2a.SendMessageConfig{HistoryLength: new(5)}, want: []string{"m2", "m3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stored := historyTask()
			h := NewHandler(&fakeHandler{task: stored}, testLimits)

			res, err := h.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser), Config: tt.config})
			if err != nil {
				t.Fatalf("NewHandler().SendMessage() error = %v", err)
			}

			if diff := cmp.Diff(tt.want, historyIDs(res.(*a2a.Task))); diff != "" {
				t.Fatalf("NewHandler().SendMessage() wrong result (-want +got) diff = %s", diff)
			}
			if len(stored.History) != 3 {
				t.Fatalf("NewHandler().SendMessage() changed the handler's task history to %d messages, want 3", len(stored.History))
			}
		})
	}
}

func TestHandlerSendStreamingMessageHistory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config *a2a.SendMessageConfig
		want   []string
	}{
		{name: "no length", want: []string{"m1", "m2", "m3"}},
		{name: "last one", config: &a2a.SendMessageConfig{HistoryLength: new(1)}, want: []string{"m3"}},
		{name: "over the limit", config: &a2a.SendMessageConfig{HistoryLength: new(5)}, want: []string{"m2", "m3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := NewHandler(&fakeHandler{task: historyTask()}, testLimits)

			var got []string
			for ev, err := range h.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser), Config: tt.config}) {
				if err != nil {
					t.Fatalf("NewHandler().SendStreamingMessage() error = %v", err)
				}
				got = historyIDs(ev.(*a2a.Task))
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("NewHandler().SendStreamingMessage() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestHandlerClampsHistoryLength(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ask  *int
		want *int
	}{
		{name: "no length"},
		{name: "within the limit", ask: new(1), want: new(1)},
		{name: "over the limit", ask: new(1000), want: new(2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			get := &fakeHandler{task: historyTask()}
			if _, err := NewHandler(get, testLimits).GetTask(t.Context(), &a2a.GetTaskRequest{ID: "t1", HistoryLength: tt.ask}); err != nil {
				t.Fatalf("NewHandler().GetTask() error = %v", err)
			}
			list := &fakeHandler{task: historyTask()}
			if _, err := NewHandler(list, testLimits).ListTasks(t.Context(), &a2a.ListTasksRequest{HistoryLength: tt.ask}); err != nil {
				t.Fatalf("NewHandler().ListTasks() error = %v", err)
			}

			if diff := cmp.Diff(tt.want, get.historyLength); diff != "" {
				t.Fatalf("NewHandler().GetTask() history length wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tt.want, list.historyLength); diff != "" {
				t.Fatalf("NewHandler().ListTasks() history length wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestCutPart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		part *a2a.Part
		want *a2a.Part
	}{
		{name: "short text", part: a2a.NewTextPart("abcd"), want: a2a.NewTextPart("abcd")},
		{name: "long text", part: a2a.NewTextPart("abcdef"), want: a2a.NewTextPart("abcd\n[cut: 4 of 6 bytes shown; get task t1 for the whole text]")},
		{name: "text cut in a rune", part: a2a.NewTextPart("abcéd"), want: a2a.NewTextPart("abc\n[cut: 3 of 6 bytes shown; get task t1 for the whole text]")},
		{
			name: "long file",
			part: &a2a.Part{Content: a2a.Raw("12345"), Filename: "f.bin", MediaType: "application/octet-stream"},
			want: &a2a.Part{Content: a2a.Text("[file of 5 bytes left out of the list; get task t1 for it]"), Filename: "f.bin"},
		},
		{name: "long data", part: a2a.NewDataPart(map[string]any{"k": "v"}), want: a2a.NewTextPart("[data of 9 bytes left out of the list; get task t1 for it]")},
		{name: "file URL", part: a2a.NewFileURLPart("https://example.com/long", "text/plain"), want: a2a.NewFileURLPart("https://example.com/long", "text/plain")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := *tt.part

			got := CutPart("t1", tt.part, 4)

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("CutPart() wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(&in, tt.part); diff != "" {
				t.Fatalf("CutPart() changed its part (-want +got) diff = %s", diff)
			}
		})
	}
}
