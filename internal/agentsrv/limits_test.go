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
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/a2aproject/a2a-serve/internal/a2alimit"
	"github.com/a2aproject/a2a-serve/internal/auth"
)

func TestMessageOverLimitsIsRefused(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	addNode(t, ts, "grok", 0)
	token := ts.pair("me", auth.RoleOwner)
	manyParts := make([]*a2a.Part, a2alimit.MaxParts+1)
	for i := range manyParts {
		manyParts[i] = a2a.NewTextPart("x")
	}
	longText := a2a.NewTextPart(strings.Repeat("x", a2alimit.MaxText+1))
	clients := []struct {
		name   string
		client *a2aclient.Client
	}{
		{name: "JSON-RPC", client: ts.clientOver(token, a2a.TransportProtocolJSONRPC)},
		{name: "REST", client: ts.clientOver(token, a2a.TransportProtocolHTTPJSON)},
		{name: "node", client: nodeClient(t, ts, "grok", token)},
	}
	messages := []struct {
		name    string
		parts   []*a2a.Part
		wantMsg string
	}{
		{name: "too many parts", parts: manyParts, wantMsg: "at most 1000 parts"},
		{name: "too much text", parts: []*a2a.Part{longText}, wantMsg: "at most 1048576 bytes"},
	}
	for _, c := range clients {
		for _, m := range messages {
			t.Run(c.name+" "+m.name, func(t *testing.T) {
				t.Parallel()
				req := &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, m.parts...)}

				_, err := c.client.SendMessage(context.Background(), req)
				if !errors.Is(err, a2a.ErrInvalidParams) || !strings.Contains(err.Error(), m.wantMsg) {
					t.Fatalf("SendMessage() error = %v, want invalid params that names %q", err, m.wantMsg)
				}
				var streamErr error
				for _, err := range c.client.SendStreamingMessage(context.Background(), req) {
					streamErr = err
					break
				}
				if !errors.Is(streamErr, a2a.ErrInvalidParams) || !strings.Contains(streamErr.Error(), m.wantMsg) {
					t.Fatalf("SendStreamingMessage() error = %v, want invalid params that names %q", streamErr, m.wantMsg)
				}
			})
		}
	}
}

func TestSendMessageReturnsHistoryOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	tests := []struct {
		name   string
		config *a2a.SendMessageConfig
		want   int
	}{
		{name: "no history length", want: 0},
		{name: "history length", config: &a2a.SendMessageConfig{HistoryLength: new(1)}, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: userText("hello", "", ""), Config: tt.config})
			if err != nil {
				t.Fatalf("SendMessage() error = %v", err)
			}

			task, ok := res.(*a2a.Task)
			if !ok {
				t.Fatalf("SendMessage() = %T, want *a2a.Task", res)
			}
			if len(task.History) != tt.want {
				t.Fatalf("SendMessage() history = %d messages, want %d", len(task.History), tt.want)
			}
		})
	}
}
