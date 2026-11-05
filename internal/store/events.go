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
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
)

// Kinds of task events, stored next to their JSON.
const (
	kindTask     = "task"
	kindMessage  = "message"
	kindStatus   = "status-update"
	kindArtifact = "artifact-update"
)

// storedEvent is a task event with its raw parts moved to blobs.
type storedEvent struct {
	event a2a.Event
	kind  string
	data  string
	blobs []blob
}

func encodeEvent(ev a2a.Event) (*storedEvent, error) {
	x := &externalizer{}
	out := &storedEvent{}
	switch v := ev.(type) {
	case *a2a.Task:
		out.event, out.kind = x.task(v), kindTask
	case *a2a.Message:
		out.event, out.kind = x.message(v), kindMessage
	case *a2a.TaskStatusUpdateEvent:
		ref := *v
		ref.Status.Message = x.message(v.Status.Message)
		out.event, out.kind = &ref, kindStatus
	case *a2a.TaskArtifactUpdateEvent:
		ref := *v
		ref.Artifact = x.artifact(v.Artifact)
		out.event, out.kind = &ref, kindArtifact
	default:
		return nil, fmt.Errorf("unsupported task event %T", ev)
	}
	data, err := json.Marshal(out.event)
	if err != nil {
		return nil, err
	}
	out.data, out.blobs = string(data), x.blobs
	return out, nil
}

func decodeEvent(kind, data string) (a2a.Event, error) {
	var ev a2a.Event
	switch kind {
	case kindTask:
		ev = &a2a.Task{}
	case kindMessage:
		ev = &a2a.Message{}
	case kindStatus:
		ev = &a2a.TaskStatusUpdateEvent{}
	case kindArtifact:
		ev = &a2a.TaskArtifactUpdateEvent{}
	default:
		return nil, fmt.Errorf("unknown task event kind %q", kind)
	}
	if err := json.Unmarshal([]byte(data), ev); err != nil {
		return nil, fmt.Errorf("decoding %s event: %w", kind, err)
	}
	return ev, nil
}

// fold materializes a task from its events. base is the task before the
// events, from a snapshot, or nil when the first event is the task as
// created. fold changes base and the events in place, so they must not be
// used elsewhere.
func fold(base *a2a.Task, events []a2a.Event) (*a2a.Task, error) {
	if base == nil {
		if len(events) == 0 {
			return nil, fmt.Errorf("task has no events")
		}
		first, ok := events[0].(*a2a.Task)
		if !ok {
			return nil, fmt.Errorf("first task event is %T, want a task", events[0])
		}
		base, events = first, events[1:]
	}
	f := &folder{mergeFrom: map[*a2a.Artifact]int{}}
	f.reset(base)
	for _, ev := range events {
		if err := f.apply(ev); err != nil {
			return nil, err
		}
	}
	for _, a := range f.task.Artifacts {
		if from, ok := f.mergeFrom[a]; ok {
			a.Parts = a2atext.MergeChunks(a.Parts, from)
		}
	}
	return f.task, nil
}

// folder applies events to a task in place, as [a2aevent.ApplyShallowUpdate]
// does to a copy.
type folder struct {
	task *a2a.Task
	// inHistory holds the IDs of the messages of the history.
	inHistory map[string]bool
	// mergeFrom is the index of the first part of each artifact which chunks
	// were appended to that may merge with the chunks.
	mergeFrom map[*a2a.Artifact]int
}

func (f *folder) reset(task *a2a.Task) {
	f.task = task
	f.inHistory = map[string]bool{}
	for _, m := range task.History {
		if m != nil {
			f.inHistory[m.ID] = true
		}
	}
}

func (f *folder) apply(ev a2a.Event) error {
	if err := validateEvent(f.task, ev); err != nil {
		return err
	}
	switch v := ev.(type) {
	case *a2a.Task:
		f.reset(v)
	case *a2a.Message:
		f.addStatusToHistory()
		f.addToHistory(v)
	case *a2a.TaskStatusUpdateEvent:
		f.addStatusToHistory()
		if v.Metadata != nil {
			if f.task.Metadata == nil {
				f.task.Metadata = make(map[string]any, len(v.Metadata))
			}
			maps.Copy(f.task.Metadata, v.Metadata)
		}
		f.task.Status = v.Status
	case *a2a.TaskArtifactUpdateEvent:
		return f.applyArtifact(v)
	default:
		return fmt.Errorf("unexpected event type %T", v)
	}
	return nil
}

func (f *folder) addToHistory(m *a2a.Message) {
	f.task.History = append(f.task.History, m)
	if m != nil {
		f.inHistory[m.ID] = true
	}
}

func (f *folder) addStatusToHistory() {
	m := f.task.Status.Message
	if m != nil && (m.ID == "" || !f.inHistory[m.ID]) {
		f.addToHistory(m)
	}
}

func (f *folder) applyArtifact(ev *a2a.TaskArtifactUpdateEvent) error {
	art := ev.Artifact
	if art == nil || len(art.Parts) == 0 {
		return fmt.Errorf("artifact cannot be empty")
	}
	i := slices.IndexFunc(f.task.Artifacts, func(a *a2a.Artifact) bool { return a.ID == art.ID })
	switch {
	case i < 0 && ev.Append:
		return fmt.Errorf("no artifact found for update")
	case i < 0:
		f.task.Artifacts = append(f.task.Artifacts, art)
		return nil
	case !ev.Append:
		f.task.Artifacts[i] = art
		return nil
	}
	dst := f.task.Artifacts[i]
	if _, ok := f.mergeFrom[dst]; !ok {
		f.mergeFrom[dst] = max(len(dst.Parts)-1, 0)
	}
	dst.Parts = append(dst.Parts, art.Parts...)
	if art.Metadata != nil {
		if dst.Metadata == nil {
			dst.Metadata = make(map[string]any, len(art.Metadata))
		}
		maps.Copy(dst.Metadata, art.Metadata)
	}
	return nil
}

// validateEvent checks that ev applies to task, as
// [a2aevent.ApplyShallowUpdate] does.
func validateEvent(task *a2a.Task, ev a2a.Event) error {
	msg, isMsg := ev.(*a2a.Message)
	if isMsg && msg.Role != a2a.MessageRoleUser {
		return fmt.Errorf("only user messages can be applied to the task state, got %q", msg.Role)
	}
	if task.Status.State.Terminal() {
		return fmt.Errorf("%q task state updates are not allowed", task.Status.State)
	}
	have, got := task.TaskInfo(), ev.TaskInfo()
	if have.TaskID != got.TaskID && !(isMsg && msg.TaskID == "") {
		return fmt.Errorf("task IDs don't match: %s != %s", have.TaskID, got.TaskID)
	}
	if have.ContextID != got.ContextID && !(isMsg && msg.ContextID == "") {
		return fmt.Errorf("context IDs don't match: %s != %s", have.ContextID, got.ContextID)
	}
	return nil
}

// header returns task without its history and artifacts, which only its
// events keep.
func header(task *a2a.Task) *a2a.Task {
	h := *task
	h.History, h.Artifacts = nil, nil
	return &h
}

// applyToHeader returns the header h after ev. Only tasks and status
// updates change the header.
func applyToHeader(h *a2a.Task, ev a2a.Event) (*a2a.Task, error) {
	switch ev.(type) {
	case *a2a.Task, *a2a.TaskStatusUpdateEvent:
		next, err := a2aevent.ApplyShallowUpdate(h, ev)
		if err != nil {
			return nil, err
		}
		return header(next), nil
	}
	return h, nil
}
