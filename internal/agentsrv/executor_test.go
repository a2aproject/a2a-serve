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

package agentsrv

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestEventBusKeepsEventsOfSlowFollower(t *testing.T) {
	t.Parallel()
	bus := newEventBus()
	sub, unsubscribe := bus.subscribe("t1")
	defer unsubscribe()
	task := &a2a.Task{ID: "t1"}
	const n = 200
	for range n - 1 {
		bus.publish("t1", a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, nil))
	}
	bus.publish("t1", a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil))

	var last a2a.Event
	for i := range n {
		ev, ok := sub.next(context.Background(), time.After(time.Second))
		if !ok {
			t.Fatalf("subscription.next() after %d events ok = false, want %d events", i, n)
		}
		last = ev
	}
	if got := last.(*a2a.TaskStatusUpdateEvent).Status.State; got != a2a.TaskStateCompleted {
		t.Fatalf("last event state = %v, want %v", got, a2a.TaskStateCompleted)
	}
}
