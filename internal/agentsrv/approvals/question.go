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

// Package approvals handles the questions an agent asks while working:
// permission requests and forms, asked as structured input requests in
// input-required status messages. It stores them so buttons and pages can
// answer them, decides who may answer, and sends answers to the task, here
// or at another agent.
package approvals

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"

	"github.com/a2aproject/a2a-serve/internal/store"
)

// Question types.
const (
	TypePermission = "permission"
	TypeForm       = "form"
)

// Question is a question from an input-required status message.
type Question struct {
	// ID is the ID of the structured input request.
	ID      string
	Type    string
	Title   string
	Options []Option
	// Kind is the ACP tool kind of a permission, if its message tells.
	Kind string
	// Text is the text of the status message.
	Text string
	// Data is the question as stored in records: id, type, title and options.
	Data map[string]any
}

// Option is an answer to a question.
type Option struct {
	ID    string
	Label string
}

// FromStatus returns the question of an input-required status, or nil.
func FromStatus(st a2a.TaskStatus) *Question {
	if st.State != a2a.TaskStateInputRequired || st.Message == nil {
		return nil
	}
	return FromMessage(st.Message)
}

// FromMessage returns the question of a status message, or nil if it has no
// structured input request with options to choose from, as a2aacp asks.
func FromMessage(msg *a2a.Message) *Question {
	req, err := structuredinput.ParseRequest(msg)
	if err != nil {
		slog.Warn("reading the input request of a message", "error", err)
		return nil
	}
	if req == nil || req.Schema == nil || req.Schema.Properties["option"] == nil {
		return nil
	}
	typ := TypePermission
	if req.Schema.Properties["content"] != nil {
		typ = TypeForm
	}
	var options []any
	for _, o := range req.Schema.Properties["option"].OneOf {
		if o.Const == nil {
			continue
		}
		if id, ok := (*o.Const).(string); ok {
			options = append(options, map[string]any{"id": id, "label": o.Title})
		}
	}
	q := fromData(map[string]any{"id": req.ID, "type": typ, "title": req.Schema.Title, "options": options})
	var texts []string
	for _, p := range msg.Parts {
		if t := p.Text(); t != "" {
			texts = append(texts, t)
		}
		if data, ok := p.Data().(map[string]any); ok {
			tool, _ := data["toolCall"].(map[string]any)
			if kind, ok := tool["kind"].(string); ok {
				q.Kind = kind
			}
		}
	}
	q.Text = strings.Join(texts, "\n")
	return q
}

// FromRecord rebuilds the question of a stored record.
func FromRecord(rec store.Question) *Question {
	q := fromData(rec.Data)
	q.Text, _ = rec.Data["text"].(string)
	return q
}

func fromData(data map[string]any) *Question {
	q := &Question{Data: data}
	q.ID, _ = data["id"].(string)
	q.Type, _ = data["type"].(string)
	q.Title, _ = data["title"].(string)
	opts, _ := data["options"].([]any)
	for _, o := range opts {
		m, _ := o.(map[string]any)
		id, _ := m["id"].(string)
		label, _ := m["label"].(string)
		if label == "" {
			label = id
		}
		if id != "" {
			q.Options = append(q.Options, Option{ID: id, Label: label})
		}
	}
	return q
}

// Labels returns the button labels of the question.
func (q *Question) Labels() []string {
	out := make([]string, len(q.Options))
	for i, o := range q.Options {
		out[i] = o.Label
	}
	return out
}

// hiddenWarning heads the text of a question in which Display escaped
// hidden characters.
const hiddenWarning = "⚠ This request has hidden formatting characters, shown as [U+XXXX].\n"

// Display is the text shown for the question: its message, or its title.
// Characters that can make a command look like other text are escaped, and
// a warning line is added.
func (q *Question) Display() string {
	text := q.Text
	if text == "" {
		text = q.Title
	}
	if !strings.ContainsFunc(text, hidden) {
		return text
	}
	var b strings.Builder
	b.WriteString(hiddenWarning)
	for _, r := range text {
		if hidden(r) {
			fmt.Fprintf(&b, "[U+%04X]", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// hidden reports whether r is a bidi embedding, override or isolate
// control, or a zero-width space. Joiners, which emoji sequences use, and
// directional marks are shown as they are.
func hidden(r rune) bool {
	switch {
	case r >= '\u202A' && r <= '\u202E', r >= '\u2066' && r <= '\u2069':
		return true
	}
	return r == '\u200B' || r == '\u2060' || r == '\uFEFF'
}

// Match maps a typed reply to an option, accepting option IDs and labels
// and the words of an email approval. It accepts every word a2aacp does.
func (q *Question) Match(text string) (Option, bool) {
	reply := strings.ToLower(strings.Trim(strings.TrimSpace(text), ".!\"'`"))
	for _, o := range q.Options {
		if reply == strings.ToLower(o.ID) || reply == strings.ToLower(o.Label) {
			return o, true
		}
	}
	var id string
	switch reply {
	case "approve", "approved", "yes", "y", "ok", "allow", "lgtm":
		id = a2aacp.OptionOnce
	case "always", "always allow", "allow always":
		id = a2aacp.OptionAlways
	case "deny", "denied", "no", "n", "reject", "decline":
		id = a2aacp.OptionReject
	}
	for _, o := range q.Options {
		if o.ID == id {
			return o, true
		}
	}
	return Option{}, false
}

// Words lists the replies Match accepts, for channels without buttons.
func (q *Question) Words() string {
	var words []string
	for _, o := range q.Options {
		switch o.ID {
		case a2aacp.OptionOnce:
			words = append(words, "approve")
		case a2aacp.OptionAlways:
			words = append(words, "always")
		case a2aacp.OptionReject:
			words = append(words, "deny")
		default:
			words = append(words, o.ID)
		}
	}
	return strings.Join(words, ", ")
}

// AnswerMessage is the reply which selects an option, as structured input.
func AnswerMessage(taskID a2a.TaskID, contextID, questionID, option string) (*a2a.Message, error) {
	part, err := structuredinput.NewResponsePart(questionID, a2aacp.Answer{Option: option})
	if err != nil {
		return nil, fmt.Errorf("building the answer: %w", err)
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, part)
	msg.TaskID, msg.ContextID = taskID, contextID
	return msg, nil
}
