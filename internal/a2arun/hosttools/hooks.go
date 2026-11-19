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

package hosttools

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
)

// Hook methods.
const (
	MethodBeforeTool = "hooks/beforeTool"
	MethodAfterTool  = "hooks/afterTool"
)

// Decisions of hooks/beforeTool.
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
	DecisionAsk   = "ask"
)

// HookTimeouts bound the hook calls.
type HookTimeouts struct {
	BeforeTool time.Duration
	AfterTool  time.Duration
}

// DefaultHookTimeouts are the timeouts of a2a-run, which
// docs/reference/limits.md lists.
var DefaultHookTimeouts = HookTimeouts{BeforeTool: 30 * time.Second, AfterTool: 10 * time.Second}

const (
	// maxMessage bounds the message of a decision that a2a-run logs.
	maxDecisionMessage = 1 << 10
	// afterToolQueue bounds the afterTool notifications that wait to be
	// sent; more are dropped.
	afterToolQueue = 64
)

// BeforeToolParams are the params of hooks/beforeTool.
type BeforeToolParams struct {
	Context string     `json:"context"`
	Task    string     `json:"task"`
	Call    BeforeCall `json:"call"`
}

// BeforeCall is the tool call of hooks/beforeTool. Command is set for a
// shell command, and MCP for a tool of an MCP server, as <server>.<tool>.
type BeforeCall struct {
	ID      string  `json:"id"`
	Kind    string  `json:"kind"`
	Title   string  `json:"title"`
	Command *string `json:"command"`
	MCP     *string `json:"mcp"`
}

// BeforeToolResult is the result of hooks/beforeTool.
type BeforeToolResult struct {
	Decision string `json:"decision"`
	Message  string `json:"message,omitempty"`
}

// AfterToolParams are the params of hooks/afterTool.
type AfterToolParams struct {
	Context string    `json:"context"`
	Task    string    `json:"task"`
	Call    AfterCall `json:"call"`
}

// AfterCall is the tool call of hooks/afterTool.
type AfterCall struct {
	ID        string `json:"id"`
	Kind      string `json:"kind,omitempty"`
	Title     string `json:"title,omitempty"`
	Status    string `json:"status"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Hooks sends the hooks the host advertised: it asks hooks/beforeTool as
// an [a2aserve.ToolDecider], and sends hooks/afterTool for each tool call
// that ends, in order.
type Hooks struct {
	c        *Client
	support  HookSupport
	timeouts HookTimeouts
	log      *slog.Logger

	queue   chan AfterToolParams
	stop    context.CancelFunc
	stopped chan struct{}
	mu      sync.Mutex
	closed  bool
}

// NewHooks returns the hooks of c that the host advertised. Close stops
// them.
func NewHooks(c *Client, timeouts HookTimeouts, log *slog.Logger) *Hooks {
	ctx, stop := context.WithCancel(context.Background())
	h := &Hooks{c: c, support: c.Hooks(), timeouts: timeouts, log: log, queue: make(chan AfterToolParams, afterToolQueue), stop: stop, stopped: make(chan struct{})}
	go h.sendAfterTool(ctx)
	return h
}

// Support returns the hooks the host advertised.
func (h *Hooks) Support() HookSupport { return h.support }

// Decide implements [a2aserve.ToolDecider] with hooks/beforeTool. It
// leaves a call to the next decider when the host doesn't answer
// beforeTool, or answers ask. A failed hook denies the call.
func (h *Hooks) Decide(ctx context.Context, call a2aserve.ToolCall) (a2aserve.ToolDecision, bool, error) {
	if !h.support.BeforeTool {
		return a2aserve.ToolDecision{}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeouts.BeforeTool)
	defer cancel()
	params := BeforeToolParams{Context: call.ContextID, Task: call.TaskID, Call: BeforeCall{ID: call.ID, Kind: call.Kind, Title: call.Title}}
	if call.Command != "" {
		params.Call.Command = &call.Command
	}
	if call.MCP {
		params.Call.MCP = &call.Tool
	}
	var res BeforeToolResult
	err := h.c.Call(ctx, MethodBeforeTool, params, &res)
	if err == nil && res.Decision != DecisionAllow && res.Decision != DecisionDeny && res.Decision != DecisionAsk {
		err = fmt.Errorf("the tools endpoint answered %s with the decision %q, want allow, deny or ask", MethodBeforeTool, truncate(res.Decision, 64))
	}
	if err != nil {
		h.log.Warn("denying a tool call, since the host's beforeTool hook failed", "context", call.ContextID, "task", call.TaskID, "call", call.ID, "error", err)
		return a2aserve.ToolDecision{Allow: false, Rule: MethodBeforeTool + " failed"}, true, nil
	}
	msg := truncate(res.Message, maxDecisionMessage)
	h.log.Info("the host's beforeTool hook decided", "context", call.ContextID, "task", call.TaskID, "call", call.ID, "kind", call.Kind, "decision", res.Decision, "message", msg)
	switch res.Decision {
	case DecisionAllow:
		return a2aserve.ToolDecision{Allow: true, Rule: MethodBeforeTool}, true, nil
	case DecisionDeny:
		return a2aserve.ToolDecision{Allow: false, Rule: MethodBeforeTool}, true, nil
	}
	return a2aserve.ToolDecision{}, false, nil
}

// ToolDone queues hooks/afterTool for r, for a2aserve.Hooks.ToolDone. It
// drops r when the queue is full.
func (h *Hooks) ToolDone(_ context.Context, r a2aserve.ToolResult) {
	if !h.support.AfterTool {
		return
	}
	p := AfterToolParams{Context: r.ContextID, Task: r.TaskID, Call: AfterCall{ID: r.ID, Kind: r.Kind, Title: r.Title, Status: r.Status, Output: r.Output, Truncated: r.Truncated}}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	select {
	case h.queue <- p:
	default:
		h.log.Warn("dropping the host's afterTool hook of a tool call, since too many wait to be sent", "context", r.ContextID, "call", r.ID)
	}
}

func (h *Hooks) sendAfterTool(ctx context.Context) {
	defer close(h.stopped)
	dropped := 0
	defer func() {
		if dropped > 0 {
			h.log.Warn("dropping the host's afterTool hooks that a2a-run didn't send before it stopped", "count", dropped)
		}
	}()
	for p := range h.queue {
		if ctx.Err() != nil {
			dropped++
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, h.timeouts.AfterTool)
		if err := h.c.Notify(sctx, MethodAfterTool, p); err != nil {
			h.log.Warn("sending the host's afterTool hook", "context", p.Context, "call", p.Call.ID, "error", err)
		}
		cancel()
	}
}

// Close sends the queued afterTool notifications until ctx ends, then
// drops the rest.
func (h *Hooks) Close(ctx context.Context) {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		close(h.queue)
	}
	h.mu.Unlock()
	select {
	case <-h.stopped:
	case <-ctx.Done():
		h.stop()
		<-h.stopped
	}
	h.stop()
}

// truncate cuts s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
