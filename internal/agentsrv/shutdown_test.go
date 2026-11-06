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
	"os"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/auth"
)

func TestStopSendsSubscribersFinalEvent(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	task := sendAsync(t, c, userText("slow", "", ""))
	waitState(t, c, task.ID, a2a.TaskStateWorking)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	subscribed := make(chan struct{})
	last := make(chan a2a.Event, 1)
	go func() {
		var got a2a.Event
		for ev, err := range c.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: task.ID}) {
			if err != nil {
				break
			}
			if got == nil {
				close(subscribed)
			}
			got = ev
		}
		last <- got
	}()
	select {
	case <-subscribed:
	case <-ctx.Done():
		t.Fatal("SubscribeToTask() sent no event")
	}

	ts.stop()

	ev := <-last
	status, ok := ev.(*a2a.TaskStatusUpdateEvent)
	if !ok || status.Status.State != a2a.TaskStateFailed || status.Status.Message == nil {
		t.Fatalf("SubscribeToTask() last event = %#v, want a failed status", ev)
	}
	if got := a2atext.Message(status.Status.Message); got != stoppedMessage {
		t.Fatalf("SubscribeToTask() last message = %q, want %q", got, stoppedMessage)
	}
}

func TestServerStopsWhenStateDirIsDeleted(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})

	// The server writes to its state directory until it notices, so a
	// removal can find a file it didn't list.
	deadline := time.Now().Add(10 * time.Second)
	for err := os.RemoveAll(ts.dir.Path()); err != nil; err = os.RemoveAll(ts.dir.Path()) {
		if time.Now().After(deadline) {
			t.Fatalf("os.RemoveAll() error = %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case err := <-ts.done:
		ts.srv = nil
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() kept serving after its state directory was deleted")
	}
}
