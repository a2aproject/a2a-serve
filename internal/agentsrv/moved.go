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
	"log/slog"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// earlierPrefix starts the prompt part that gives a new session the
// earlier messages of its conversation.
const earlierPrefix = "[a2a earlier conversation]"

// movedNote tells the caller that the agent lost its session.
const movedNote = "Note: the agent's workspace moved, so it started a new session with the earlier messages of this conversation."

const (
	// earlierTasks is how many of the latest tasks the earlier messages
	// come from.
	earlierTasks = 20
	// earlierHistory is how many of the latest messages of each task they
	// include.
	earlierHistory = 20
	// maxEarlierBytes bounds the earlier messages; the latest are kept.
	maxEarlierBytes = 16 << 10
)

// movedConversations continues the conversations whose ACP session was
// created in another workspace, after the agent was restored elsewhere or
// its directory moved. The agent can't continue such a session, so it
// gets a new one, with the earlier messages of the conversation.
type movedConversations struct {
	sessions store.SessionStore
	tasks    *store.TaskStore
}

// withEarlier returns execCtx with the earlier messages of its conversation
// added before the caller's message if its session was created in another
// workspace, and whether it was. Answers to the agent's own questions are
// left alone, because the agent parses them.
func (m *movedConversations) withEarlier(ctx context.Context, execCtx *a2asrv.ExecutorContext) (*a2asrv.ExecutorContext, bool) {
	if m == nil || execCtx.Message == nil || (execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2a.TaskStateInputRequired) {
		return execCtx, false
	}
	moved, err := m.sessions.Moved(ctx, execCtx.ContextID)
	if err != nil {
		slog.Warn("looking up the session of a conversation", "context", execCtx.ContextID, "error", err)
		return execCtx, false
	}
	if !moved {
		return execCtx, false
	}
	earlier, err := m.earlier(ctx, execCtx)
	if err != nil {
		slog.Warn("reading the earlier messages of a conversation", "context", execCtx.ContextID, "error", err)
	}
	if earlier == "" {
		return execCtx, true
	}
	text := earlierPrefix + " This conversation continues in a new session, because the workspace moved. The earlier messages of the conversation:\n\n" + earlier
	ec := *execCtx
	msg := *execCtx.Message
	msg.Parts = append([]*a2a.Part{a2a.NewTextPart(text)}, msg.Parts...)
	ec.Message = &msg
	return &ec, true
}

// earlier returns the messages of the latest tasks in the conversation
// before this one, at most maxEarlierBytes of them.
func (m *movedConversations) earlier(ctx context.Context, execCtx *a2asrv.ExecutorContext) (string, error) {
	history := earlierHistory
	resp, err := m.tasks.List(ctx, &a2a.ListTasksRequest{ContextID: execCtx.ContextID, PageSize: earlierTasks, HistoryLength: &history, IncludeArtifacts: true})
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, t := range slices.Backward(resp.Tasks) {
		if t.ID == execCtx.TaskID {
			continue
		}
		for _, msg := range t.History {
			if msg.Role != a2a.MessageRoleUser {
				continue
			}
			if text := a2atext.Message(msg); text != "" {
				sb.WriteString("User: " + text + "\n\n")
			}
		}
		if text := a2atext.Result(t); text != "" {
			sb.WriteString("Agent: " + text + "\n\n")
		}
	}
	return lastBytes(strings.TrimSpace(sb.String()), maxEarlierBytes), nil
}

// lastBytes returns the end of s, at most n bytes of it, cut at a line.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if _, rest, ok := strings.Cut(s, "\n"); ok {
		return strings.TrimLeft(rest, "\n")
	}
	return strings.ToValidUTF8(s, "")
}
