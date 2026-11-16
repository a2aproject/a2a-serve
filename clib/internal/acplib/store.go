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

package acplib

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// MaxTasks bounds the tasks a [boundedStore] keeps. Past it, the task in a
// terminal state that changed least recently goes first.
const MaxTasks = 1000

// boundedStore keeps up to max tasks in memory.
type boundedStore struct {
	max int

	mu    sync.Mutex
	tasks map[a2a.TaskID]*storedTask
}

type storedTask struct {
	task    *a2a.Task
	version taskstore.TaskVersion
	updated time.Time
}

var _ taskstore.Store = (*boundedStore)(nil)

func newBoundedStore(max int) *boundedStore {
	return &boundedStore{max: max, tasks: map[a2a.TaskID]*storedTask{}}
}

func clone(t *a2a.Task) (*a2a.Task, error) {
	data, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	var out a2a.Task
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *boundedStore) Create(_ context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	c, err := clone(task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[task.ID]; ok {
		return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
	}
	if len(s.tasks) >= s.max && !s.evictLocked() {
		return taskstore.TaskVersionMissing, errors.New("too many tasks in progress")
	}
	s.tasks[task.ID] = &storedTask{task: c, version: 1, updated: time.Now()}
	return 1, nil
}

// evictLocked removes the terminal task that changed least recently.
func (s *boundedStore) evictLocked() bool {
	var oldest *storedTask
	for _, st := range s.tasks {
		if st.task.Status.State.Terminal() && (oldest == nil || st.updated.Before(oldest.updated)) {
			oldest = st
		}
	}
	if oldest == nil {
		return false
	}
	delete(s.tasks, oldest.task.ID)
	return true
}

func (s *boundedStore) Update(_ context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	c, err := clone(req.Task)
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tasks[req.Task.ID]
	if !ok {
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	}
	if req.PrevVersion != taskstore.TaskVersionMissing && st.version != req.PrevVersion {
		return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
	}
	st.task, st.version, st.updated = c, st.version+1, time.Now()
	return st.version, nil
}

func (s *boundedStore) Get(_ context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	s.mu.Lock()
	st, ok := s.tasks[id]
	var task *a2a.Task
	var version taskstore.TaskVersion
	if ok {
		task, version = st.task, st.version
	}
	s.mu.Unlock()
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	c, err := clone(task)
	if err != nil {
		return nil, err
	}
	return &taskstore.StoredTask{Task: c, Version: version}, nil
}

func (s *boundedStore) List(context.Context, *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	return nil, errors.New("the library doesn't list tasks")
}
