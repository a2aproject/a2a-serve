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

// Package approval is the A2A wire format of what an agent asks in an
// input-required status, such as an approval of a tool call, and of the
// replies that answer it. Agents built on a2aserve ask this way. See
// docs/reference/approval-messages.md.
package approval

import (
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/question"
)

// Request types.
const (
	// TypePermission asks to allow a tool call.
	TypePermission = question.TypePermission
	// TypeForm asks for the fields of a form.
	TypeForm = question.TypeForm
)

// IDs of the options of a permission.
const (
	// OptionOnce allows the tool call this time.
	OptionOnce = "once"
	// OptionAlways allows the tool call, and calls like it, for the rest of
	// the conversation.
	OptionAlways = "always"
	// OptionReject denies the tool call.
	OptionReject = "reject"
)

// Request is what a task asks in an input-required status.
type Request struct {
	// ID is the ID of the structured input request.
	ID    string
	Type  string
	Title string
	// Text is the text of the status message: the question, and the replies
	// it takes.
	Text    string
	Options []Option
	// Seq is the number of the question in its task, or 0 for a question
	// without one.
	Seq int
	// ToolCall is the tool call that a permission asks about, if its
	// message tells.
	ToolCall *ToolCall
}

// ToolCall is the tool call that a permission asks about.
type ToolCall struct {
	ID    string
	Title string
	Kind  string
	Input any
}

// Option is an answer to a request.
type Option struct {
	ID    string
	Label string
}

// FromStatus returns the request of an input-required status, or nil if the
// status asks nothing.
func FromStatus(st a2a.TaskStatus) *Request {
	q := question.FromStatus(st)
	if q == nil {
		return nil
	}
	r := &Request{ID: q.ID, Type: q.Type, Title: q.Title, Text: q.Text, Seq: q.Seq}
	for _, o := range q.Options {
		r.Options = append(r.Options, Option{ID: o.ID, Label: o.Label})
	}
	if q.Kind != "" || q.ToolCallID != "" || q.ToolTitle != "" || q.ToolInput != nil {
		r.ToolCall = &ToolCall{ID: q.ToolCallID, Title: q.ToolTitle, Kind: q.Kind, Input: q.ToolInput}
	}
	return r
}

// Answer returns the reply of task that selects option of r, as structured
// input. It fails if r doesn't offer option.
func Answer(task *a2a.Task, r *Request, option string) (*a2a.Message, error) {
	if task == nil || r == nil {
		return nil, errors.New("answering a request needs its task and the request")
	}
	offered := false
	for _, o := range r.Options {
		offered = offered || o.ID == option
	}
	if !offered {
		return nil, fmt.Errorf("the request %q doesn't offer the option %q", r.ID, option)
	}
	return question.AnswerMessage(task.ID, task.ContextID, &question.Question{ID: r.ID, Seq: r.Seq}, option, "")
}
