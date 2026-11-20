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
	"encoding/json"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// waitingArtifact is an artifact that waits for its task to end.
type waitingArtifact struct {
	art  *a2a.Artifact
	size int
}

// attachArtifact adds art to the task id: now if the task ended, or else
// when it ends. Until then [memory.takeArtifact] can take it to send it
// with the task's events. Past the limits on waiting artifacts, the oldest
// is dropped.
func (m *memory) attachArtifact(id a2a.TaskID, art *a2a.Artifact) {
	data, err := json.Marshal(art)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.tasks[id]; st != nil && st.state.Terminal() {
		var task a2a.Task
		if json.Unmarshal(st.data, &task) != nil {
			return
		}
		task.Artifacts = append(task.Artifacts, art)
		updated, err := json.Marshal(&task)
		if err != nil {
			return
		}
		now := m.now()
		m.bytes += len(updated) - len(st.data)
		st.data, st.updated = updated, now
		st.version++
		m.trim(st.contextID, id, now)
		return
	}
	m.dropWaitingLocked(id)
	m.waiting[id] = waitingArtifact{art: art, size: len(data)}
	m.waitingOrder = append(m.waitingOrder, id)
	m.waitingBytes += len(data)
	for len(m.waitingOrder) > 0 && (len(m.waitingOrder) > m.limits.WaitingArtifacts || m.waitingBytes > m.limits.WaitingArtifactBytes) {
		m.dropWaitingLocked(m.waitingOrder[0])
	}
}

// takeArtifact returns and forgets the artifact that waits for the task
// id, or nil.
func (m *memory) takeArtifact(id a2a.TaskID) *a2a.Artifact {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.waiting[id]
	if !ok {
		return nil
	}
	m.dropWaitingLocked(id)
	return w.art
}

// withWaitingLocked returns task with the artifact that waits for it, if
// task ended, and forgets the artifact. It runs with m.mu held.
func (m *memory) withWaitingLocked(task *a2a.Task) *a2a.Task {
	w, ok := m.waiting[task.ID]
	if !ok || !task.Status.State.Terminal() {
		return task
	}
	m.dropWaitingLocked(task.ID)
	out := *task
	out.Artifacts = append(slices.Clone(task.Artifacts), w.art)
	return &out
}

// dropWaitingLocked forgets the artifact that waits for the task id. It
// runs with m.mu held.
func (m *memory) dropWaitingLocked(id a2a.TaskID) {
	w, ok := m.waiting[id]
	if !ok {
		return
	}
	delete(m.waiting, id)
	m.waitingOrder = slices.DeleteFunc(m.waitingOrder, func(o a2a.TaskID) bool { return o == id })
	m.waitingBytes -= w.size
}
