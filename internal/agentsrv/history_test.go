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
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/auth"
)

func TestRejectedToolCallShowsAsRejectedInHistory(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := ts.client(ts.pair("me", auth.RoleOwner))
	asked := send(t, c, userText("ask", "", ""))
	send(t, c, userText("reject", string(asked.ID), asked.ContextID))

	task, err := c.GetTask(context.Background(), &a2a.GetTaskRequest{ID: asked.ID})
	if err != nil {
		t.Fatalf("GetTask() error = %v", err)
	}
	var texts []string
	for _, m := range task.History {
		if m.Role == a2a.MessageRoleAgent {
			texts = append(texts, a2atext.Message(m))
		}
	}
	history := strings.Join(texts, "\n")
	if !strings.Contains(history, "Rejected git push") || strings.Contains(history, "Failed git push") {
		t.Fatalf("agent messages of the task:\n%s\nwant the rejected call shown as Rejected", history)
	}
}
