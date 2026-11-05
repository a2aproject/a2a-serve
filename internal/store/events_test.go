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

package store

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
)

// decodedCopies returns copies of events as read back from the store.
func decodedCopies(t *testing.T, events []a2a.Event) []a2a.Event {
	t.Helper()
	out := make([]a2a.Event, 0, len(events))
	for _, ev := range events {
		stored, err := encodeEvent(ev)
		if err != nil {
			t.Fatalf("encodeEvent(%T) error = %v", ev, err)
		}
		decoded, err := decodeEvent(stored.kind, stored.data)
		if err != nil {
			t.Fatalf("decodeEvent(%s) error = %v", stored.kind, err)
		}
		out = append(out, decoded)
	}
	return out
}

func applyShallowUpdates(events []a2a.Event) (*a2a.Task, error) {
	task := events[0].(*a2a.Task)
	for _, ev := range events[1:] {
		next, err := a2aevent.ApplyShallowUpdate(task, ev)
		if err != nil {
			return nil, err
		}
		task = next
	}
	return task, nil
}

func TestFoldMatchesApplyShallowUpdate(t *testing.T) {
	t.Parallel()
	task := newTask("t1", "c1")
	task.History = []*a2a.Message{a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("start"))}
	task.Status.Message = a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart("on it"))
	data := func(v string) *a2a.Part { return a2a.NewDataPart(map[string]any{"v": v}) }
	artifact := func(id string, appendTo bool, metadata map[string]any, parts ...*a2a.Part) *a2a.TaskArtifactUpdateEvent {
		return &a2a.TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1", Append: appendTo,
			Artifact: &a2a.Artifact{ID: a2a.ArtifactID(id), Parts: parts, Metadata: metadata}}
	}
	followUp := a2a.NewMessageForTask(a2a.MessageRoleUser, task, a2a.NewTextPart("more"))
	withMetadata := a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, nil)
	withMetadata.Metadata = map[string]any{"step": "2"}
	noID := a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart("no id"))
	noID.ID = ""
	replaced := newTask("t1", "c1")
	replaced.History = []*a2a.Message{followUp}
	agentMessage := a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart("hi"))

	tests := []struct {
		name    string
		events  []a2a.Event
		wantErr bool
	}{
		{
			name: "status messages, history and metadata",
			events: []a2a.Event{
				task,
				a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, a2a.NewMessageForTask(a2a.MessageRoleAgent, task, a2a.NewTextPart("thinking"))),
				followUp,
				a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, followUp),
				withMetadata,
				a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, noID),
				a2a.NewStatusUpdateEvent(task, a2a.TaskStateInputRequired, noID),
				a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil),
			},
		},
		{
			name: "artifacts added, appended and replaced",
			events: []a2a.Event{
				task,
				artifact("a1", false, nil, data("1")),
				artifact("a1", true, map[string]any{"k": "v"}, data("2"), data("3")),
				artifact("a2", false, nil, data("x")),
				artifact("a2", false, nil, data("y")),
				artifact("a1", true, map[string]any{"k2": "v2"}, data("4")),
			},
		},
		{
			name:   "task replaced",
			events: []a2a.Event{task, artifact("a1", false, nil, data("1")), replaced, artifact("a2", false, nil, data("2"))},
		},
		{name: "agent message", events: []a2a.Event{task, agentMessage}, wantErr: true},
		{name: "append to a missing artifact", events: []a2a.Event{task, artifact("a1", true, nil, data("1"))}, wantErr: true},
		{name: "empty artifact", events: []a2a.Event{task, artifact("a1", false, nil)}, wantErr: true},
		{name: "other task", events: []a2a.Event{task, a2a.NewStatusUpdateEvent(newTask("t2", "c1"), a2a.TaskStateWorking, nil)}, wantErr: true},
		{
			name:    "update after a terminal state",
			events:  []a2a.Event{task, a2a.NewStatusUpdateEvent(task, a2a.TaskStateFailed, nil), a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, nil)},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want, wantErr := applyShallowUpdates(decodedCopies(t, tc.events))
			got, err := fold(nil, decodedCopies(t, tc.events))
			if (err != nil) != tc.wantErr || (wantErr != nil) != tc.wantErr {
				t.Fatalf("fold() error = %v, ApplyShallowUpdate() error = %v, want errors %v", err, wantErr, tc.wantErr)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("fold() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestFoldMergesAppendedText(t *testing.T) {
	t.Parallel()
	task := newTask("t1", "c1")
	chunk := func(appendTo bool, parts ...*a2a.Part) a2a.Event {
		return &a2a.TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1", Append: appendTo, Artifact: &a2a.Artifact{ID: "a1", Parts: parts}}
	}
	text, data := a2a.NewTextPart, a2a.NewDataPart(map[string]any{"k": "v"})

	got, err := fold(nil, decodedCopies(t, []a2a.Event{
		task,
		chunk(false, text("title"), text("Once ")),
		chunk(true, text("upon ")),
		chunk(true, text("a time"), data),
		chunk(true, text("The "), text("end")),
	}))
	if err != nil {
		t.Fatalf("fold() error = %v", err)
	}

	want := a2a.ContentParts{text("title"), text("Once upon a time"), data, text("The end")}
	if diff := cmp.Diff(want, got.Artifacts[0].Parts); diff != "" {
		t.Fatalf("fold() artifact parts wrong result (-want +got) diff = %s", diff)
	}
}
