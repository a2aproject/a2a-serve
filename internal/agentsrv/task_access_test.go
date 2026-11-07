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
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// urlClient returns an A2A client of url authenticated with token.
func urlClient(t *testing.T, url, token string) *a2aclient.Client {
	t.Helper()
	hc := contacts.TokenClient(testHTTP, store.Contact{URL: url, Token: token})
	ctx := context.Background()
	card, err := agentcard.NewResolver(hc).Resolve(ctx, url)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	c, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(hc), a2aclient.WithRESTTransport(hc))
	if err != nil {
		t.Fatalf("NewFromCard() error = %v", err)
	}
	t.Cleanup(func() {
		if err := c.Destroy(); err != nil {
			t.Errorf("Destroy() error = %v", err)
		}
	})
	return c
}

func firstStreamError(stream func(yield func(a2a.Event, error) bool)) error {
	for ev, err := range stream {
		if err != nil {
			return err
		}
		return fmt.Errorf("got event %T, want an error", ev)
	}
	return errors.New("stream ended without events, want an error")
}

// wantNoAccessToTask checks that c can't reach the running task id with any
// request that names a task. A message to the task fails because the task is
// busy, so its error isn't checked.
func wantNoAccessToTask(t *testing.T, c *a2aclient.Client, id a2a.TaskID) {
	t.Helper()
	tests := []struct {
		name    string
		call    func(ctx context.Context) error
		wantErr error
	}{
		{name: "GetTask", call: func(ctx context.Context) error {
			_, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
			return err
		}, wantErr: a2a.ErrTaskNotFound},
		{name: "CancelTask", call: func(ctx context.Context) error {
			_, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: id})
			return err
		}, wantErr: a2a.ErrTaskNotFound},
		{name: "SubscribeToTask", call: func(ctx context.Context) error {
			return firstStreamError(c.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: id}))
		}, wantErr: a2a.ErrTaskNotFound},
		{name: "SendMessage", call: func(ctx context.Context) error {
			_, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", string(id), "")})
			return err
		}},
		{name: "SendStreamingMessage", call: func(ctx context.Context) error {
			return firstStreamError(c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", string(id), "")}))
		}},
		{name: "GetTaskPushConfig", call: func(ctx context.Context) error {
			_, err := c.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{TaskID: id, ID: "push"})
			return err
		}, wantErr: a2a.ErrPushNotificationNotSupported},
		{name: "ListTaskPushConfigs", call: func(ctx context.Context) error {
			_, err := c.ListTaskPushConfigs(ctx, &a2a.ListTaskPushConfigRequest{TaskID: id})
			return err
		}, wantErr: a2a.ErrPushNotificationNotSupported},
		{name: "CreateTaskPushConfig", call: func(ctx context.Context) error {
			_, err := c.CreateTaskPushConfig(ctx, &a2a.PushConfig{TaskID: id, URL: "https://example.com/push"})
			return err
		}, wantErr: a2a.ErrPushNotificationNotSupported},
		{name: "DeleteTaskPushConfig", call: func(ctx context.Context) error {
			return c.DeleteTaskPushConfig(ctx, &a2a.DeleteTaskPushConfigRequest{TaskID: id, ID: "push"})
		}, wantErr: a2a.ErrPushNotificationNotSupported},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := tc.call(ctx)
			if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("%s() error = %v, want %v", tc.name, err, tc.wantErr)
			}
		})
	}
}

func TestCallerCantReachRunningTaskOfOthers(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	anna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	ben := ts.client(ts.pair("ben", auth.RoleAuthenticated))
	task := sendAsync(t, anna, userText("slow", "", ""))
	waitState(t, anna, task.ID, a2a.TaskStateWorking)

	wantNoAccessToTask(t, ben, task.ID)
	waitState(t, anna, task.ID, a2a.TaskStateWorking)
}

func TestNodeCallerCantReachRunningTaskOfOthers(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	node := addNode(t, ts, "grok", 0)
	node.mu.Lock()
	node.silent = true
	node.mu.Unlock()
	on := true
	if _, err := controlapi.NewClient(ts.dir).UpdatePairing(context.Background(), "grok", controlapi.PairingUpdate{Advertised: &on}); err != nil {
		t.Fatalf("UpdatePairing() error = %v", err)
	}
	url := ts.localURL() + "/n/grok"
	anna := urlClient(t, url, ts.pair("anna", auth.RoleAuthenticated))
	ben := urlClient(t, url, ts.pair("ben", auth.RoleAuthenticated))
	task := sendAsync(t, anna, userText("hello", "", ""))
	waitState(t, anna, task.ID, a2a.TaskStateSubmitted)

	wantNoAccessToTask(t, ben, task.ID)
	waitState(t, anna, task.ID, a2a.TaskStateSubmitted)
}

func TestMessageToAnotherUsersTaskLooksUnknown(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	anna := ts.client(ts.pair("anna", auth.RoleAuthenticated))
	ben := ts.client(ts.pair("ben", auth.RoleAuthenticated))
	running := sendAsync(t, anna, userText("slow 3s", "", ""))
	ctx := context.Background()

	_, unknown := ben.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", string(a2a.NewTaskID()), "")})
	_, foreign := ben.SendMessage(ctx, &a2a.SendMessageRequest{Message: userText("hello", string(running.ID), "")})

	if !errors.Is(foreign, a2a.ErrTaskNotFound) || unknown == nil || foreign.Error() != unknown.Error() {
		t.Fatalf("SendMessage() to anna's running task error = %v, want the error of an unknown task %v", foreign, unknown)
	}
}
