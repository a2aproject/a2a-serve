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

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
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

// fold materializes a task from its events, the first of which is the task
// as created.
func fold(events []a2a.Event) (*a2a.Task, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("task has no events")
	}
	task, ok := events[0].(*a2a.Task)
	if !ok {
		return nil, fmt.Errorf("first task event is %T, want a task", events[0])
	}
	for _, ev := range events[1:] {
		next, err := a2aevent.ApplyShallowUpdate(task, ev)
		if err != nil {
			return nil, err
		}
		task = next
	}
	return task, nil
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
