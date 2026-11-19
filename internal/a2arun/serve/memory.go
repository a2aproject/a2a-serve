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
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
)

// memoryLimits bound what a [memory] keeps.
type memoryLimits struct {
	// Idle is how long a conversation whose tasks all ended is kept after
	// its last change.
	Idle time.Duration
	// Contexts, TasksPerContext and Bytes bound the conversations, the
	// tasks of each, and the size of all tasks in JSON.
	Contexts        int
	TasksPerContext int
	Bytes           int
	// WaitingArtifacts and WaitingArtifactBytes bound the artifacts that
	// wait for their task to end, by count and in JSON.
	WaitingArtifacts     int
	WaitingArtifactBytes int
}

// defaultMemoryLimits are the limits of a2a-run, which
// docs/reference/limits.md lists.
var defaultMemoryLimits = memoryLimits{Idle: time.Hour, Contexts: 1000, TasksPerContext: 100, Bytes: 256 << 20, WaitingArtifacts: 1024, WaitingArtifactBytes: 64 << 20}

// sweepInterval is how often a write looks for idle conversations.
const sweepInterval = time.Minute

// memory keeps the tasks and agent sessions of the conversations. It
// forgets a conversation whose tasks all ended once it was idle for
// limits.Idle, or earlier, oldest first, to stay within its limits. It
// never forgets a task that runs or waits for an answer.
type memory struct {
	limits memoryLimits
	now    func() time.Time

	mu       sync.Mutex
	tasks    map[a2a.TaskID]*storedTask
	contexts map[string]*conversation
	bytes    int
	swept    time.Time
	// waiting are the artifacts that wait for their task to end, oldest
	// first in waitingOrder.
	waiting      map[a2a.TaskID]waitingArtifact
	waitingOrder []a2a.TaskID
	waitingBytes int
}

type storedTask struct {
	// data is the task in JSON, which also copies it as the store must.
	data      []byte
	state     a2a.TaskState
	contextID string
	version   taskstore.TaskVersion
	updated   time.Time
}

type conversation struct {
	// tasks are in the order they were created.
	tasks   []a2a.TaskID
	session *a2aserve.Session
	used    time.Time
}

var _ taskstore.Store = (*memory)(nil)

func newMemory(limits memoryLimits, now func() time.Time) *memory {
	return &memory{limits: limits, now: now, tasks: map[a2a.TaskID]*storedTask{}, contexts: map[string]*conversation{}, swept: now(), waiting: map[a2a.TaskID]waitingArtifact{}}
}

// Create implements [taskstore.Store].
func (m *memory) Create(_ context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[task.ID]; ok {
		return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
	}
	data, err := json.Marshal(m.withWaitingLocked(task))
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	now := m.now()
	m.tasks[task.ID] = &storedTask{data: data, state: task.Status.State, contextID: task.ContextID, version: 1, updated: now}
	m.bytes += len(data)
	c := m.conversation(task.ContextID, now)
	c.tasks = append(c.tasks, task.ID)
	m.trim(task.ContextID, task.ID, now)
	return 1, nil
}

// Update implements [taskstore.Store].
func (m *memory) Update(_ context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.tasks[req.Task.ID]
	if st == nil {
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	}
	if req.PrevVersion != taskstore.TaskVersionMissing && st.version != req.PrevVersion {
		return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
	}
	data, err := json.Marshal(m.withWaitingLocked(req.Task))
	if err != nil {
		return taskstore.TaskVersionMissing, err
	}
	now := m.now()
	m.bytes += len(data) - len(st.data)
	st.data, st.state, st.updated = data, req.Task.Status.State, now
	st.version++
	m.conversation(st.contextID, now)
	m.trim(st.contextID, req.Task.ID, now)
	return st.version, nil
}

// Get implements [taskstore.Store].
func (m *memory) Get(_ context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	m.mu.Lock()
	st := m.tasks[id]
	var data []byte
	var version taskstore.TaskVersion
	if st != nil {
		data, version = st.data, st.version
	}
	m.mu.Unlock()
	if st == nil {
		return nil, a2a.ErrTaskNotFound
	}
	var task a2a.Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, err
	}
	return &taskstore.StoredTask{Task: &task, Version: version}, nil
}

const (
	defaultListPage = 50
	maxListPage     = 100
	// defaultListHistory is the history of a listed task when the request
	// doesn't say.
	defaultListHistory = 100
)

// listed is a task that a List selects.
type listed struct {
	id      a2a.TaskID
	data    []byte
	updated time.Time
}

// List implements [taskstore.Store], newest change first.
func (m *memory) List(_ context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	size := cmp.Or(req.PageSize, defaultListPage)
	if size < 1 || size > maxListPage {
		return nil, fmt.Errorf("page size must be between 1 and %d, got %d: %w", maxListPage, req.PageSize, a2a.ErrInvalidRequest)
	}
	var after *listed
	if req.PageToken != "" {
		t, err := decodePageToken(req.PageToken)
		if err != nil {
			return nil, err
		}
		after = &t
	}
	m.mu.Lock()
	var all []listed
	for id, st := range m.tasks {
		if req.ContextID != "" && st.contextID != req.ContextID || req.Status != a2a.TaskStateUnspecified && st.state != req.Status {
			continue
		}
		if req.StatusTimestampAfter != nil && st.updated.Before(*req.StatusTimestampAfter) {
			continue
		}
		all = append(all, listed{id: id, data: st.data, updated: st.updated})
	}
	m.mu.Unlock()
	slices.SortFunc(all, newestFirst)
	page := all
	if after != nil {
		i, _ := slices.BinarySearchFunc(all, *after, newestFirst)
		for i < len(all) && newestFirst(all[i], *after) <= 0 {
			i++
		}
		page = all[i:]
	}
	resp := &a2a.ListTasksResponse{Tasks: []*a2a.Task{}, TotalSize: len(all), PageSize: size}
	if len(page) > size {
		page = page[:size]
		resp.NextPageToken = encodePageToken(page[size-1])
	}
	history := defaultListHistory
	if req.HistoryLength != nil {
		history = max(*req.HistoryLength, 0)
	}
	for _, l := range page {
		var task a2a.Task
		if err := json.Unmarshal(l.data, &task); err != nil {
			return nil, err
		}
		task.History = task.History[max(len(task.History)-history, 0):]
		if !req.IncludeArtifacts {
			task.Artifacts = nil
		}
		resp.Tasks = append(resp.Tasks, &task)
	}
	return resp, nil
}

func newestFirst(a, b listed) int {
	if c := b.updated.Compare(a.updated); c != 0 {
		return c
	}
	return strings.Compare(string(b.id), string(a.id))
}

func encodePageToken(l listed) string {
	return base64.URLEncoding.EncodeToString([]byte(l.updated.Format(time.RFC3339Nano) + "_" + string(l.id)))
}

func decodePageToken(token string) (listed, error) {
	b, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return listed{}, a2a.ErrParseError
	}
	ts, id, ok := strings.Cut(string(b), "_")
	updated, err := time.Parse(time.RFC3339Nano, ts)
	if !ok || err != nil {
		return listed{}, a2a.ErrParseError
	}
	return listed{id: a2a.TaskID(id), updated: updated}, nil
}

// sessions returns the agent sessions of the conversations of m.
func (m *memory) sessions() a2aserve.Sessions { return memorySessions{m} }

type memorySessions struct{ m *memory }

func (s memorySessions) Get(_ context.Context, contextID string) (a2aserve.Session, bool, error) {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	c := s.m.contexts[contextID]
	if c == nil || c.session == nil {
		return a2aserve.Session{}, false, nil
	}
	return *c.session, true, nil
}

func (s memorySessions) Put(_ context.Context, contextID string, sess a2aserve.Session) error {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	now := s.m.now()
	s.m.conversation(contextID, now).session = &sess
	s.m.trim(contextID, "", now)
	return nil
}

// conversation returns the conversation of contextID, created if missing,
// used at now. It runs with m.mu held.
func (m *memory) conversation(contextID string, now time.Time) *conversation {
	c := m.contexts[contextID]
	if c == nil {
		c = &conversation{}
		m.contexts[contextID] = c
	}
	c.used = now
	return c
}

// trim forgets what m keeps past its limits. It keeps the conversation of
// contextID and its task keep, which just changed, so that a caller can
// read the end of the task. It runs with m.mu held.
func (m *memory) trim(contextID string, keep a2a.TaskID, now time.Time) {
	if c := m.contexts[contextID]; c != nil {
		for len(c.tasks) > m.limits.TasksPerContext {
			i := slices.IndexFunc(c.tasks, func(id a2a.TaskID) bool { return id != keep && m.tasks[id].state.Terminal() })
			if i < 0 {
				break
			}
			m.dropTask(c.tasks[i])
			c.tasks = slices.Delete(c.tasks, i, i+1)
		}
	}
	if now.Sub(m.swept) >= sweepInterval {
		m.swept = now
		for id, c := range m.contexts {
			if now.Sub(c.used) >= m.limits.Idle && m.ended(c) {
				m.dropConversation(id)
			}
		}
	}
	for len(m.contexts) > m.limits.Contexts || m.bytes > m.limits.Bytes {
		oldest := ""
		for id, c := range m.contexts {
			if id != contextID && m.ended(c) && (oldest == "" || c.used.Before(m.contexts[oldest].used)) {
				oldest = id
			}
		}
		if oldest == "" {
			return
		}
		m.dropConversation(oldest)
	}
}

// ended reports whether every task of c ended. It runs with m.mu held.
func (m *memory) ended(c *conversation) bool {
	return !slices.ContainsFunc(c.tasks, func(id a2a.TaskID) bool { return !m.tasks[id].state.Terminal() })
}

func (m *memory) dropConversation(contextID string) {
	for _, id := range m.contexts[contextID].tasks {
		m.dropTask(id)
	}
	delete(m.contexts, contextID)
}

func (m *memory) dropTask(id a2a.TaskID) {
	m.bytes -= len(m.tasks[id].data)
	delete(m.tasks, id)
	m.dropWaitingLocked(id)
}
