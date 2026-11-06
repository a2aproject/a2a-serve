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

package discord

import "sync"

// orderedQueue runs the tasks of each key one after another in the order they
// were added, on a goroutine that exists only while the key has tasks. Tasks
// of different keys run independently.
type orderedQueue struct {
	mu    sync.Mutex
	tasks map[string][]func()
}

func newOrderedQueue() *orderedQueue { return &orderedQueue{tasks: map[string][]func(){}} }

// do queues f for key and returns at once.
func (q *orderedQueue) do(key string, f func()) {
	q.mu.Lock()
	pending, running := q.tasks[key]
	q.tasks[key] = append(pending, f)
	q.mu.Unlock()
	if !running {
		go q.drain(key)
	}
}

func (q *orderedQueue) drain(key string) {
	for {
		q.mu.Lock()
		pending := q.tasks[key]
		if len(pending) == 0 {
			delete(q.tasks, key)
			q.mu.Unlock()
			return
		}
		f := pending[0]
		pending[0] = nil
		q.tasks[key] = pending[1:]
		q.mu.Unlock()
		f()
	}
}
