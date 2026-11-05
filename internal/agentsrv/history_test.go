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

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"
)

type historyHandler struct {
	a2asrv.RequestHandler
	result a2a.SendMessageResult
}

func (f *historyHandler) SendMessage(context.Context, *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	return f.result, nil
}

func TestLimitedHistory(t *testing.T) {
	t.Parallel()
	msg := func(id string) *a2a.Message { return &a2a.Message{ID: id, Role: a2a.MessageRoleUser} }
	length := func(n int) *a2a.SendMessageConfig { return &a2a.SendMessageConfig{HistoryLength: &n} }
	tests := []struct {
		name   string
		config *a2a.SendMessageConfig
		want   []string
	}{
		{name: "no config", want: []string{"m1", "m2", "m3"}},
		{name: "no length", config: &a2a.SendMessageConfig{}, want: []string{"m1", "m2", "m3"}},
		{name: "zero", config: length(0), want: []string{}},
		{name: "last two", config: length(2), want: []string{"m2", "m3"}},
		{name: "more than there are", config: length(5), want: []string{"m1", "m2", "m3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stored := &a2a.Task{ID: "t1", History: []*a2a.Message{msg("m1"), msg("m2"), msg("m3")}}
			h := withLimitedHistory(&historyHandler{result: stored})

			res, err := h.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: msg("m4"), Config: tt.config})
			if err != nil {
				t.Fatalf("withLimitedHistory().SendMessage() error = %v", err)
			}

			got := []string{}
			for _, m := range res.(*a2a.Task).History {
				got = append(got, m.ID)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("withLimitedHistory().SendMessage() wrong result (-want +got) diff = %s", diff)
			}
			if len(stored.History) != 3 {
				t.Fatalf("withLimitedHistory().SendMessage() changed the handler's task history to %d messages, want 3", len(stored.History))
			}
		})
	}
}
