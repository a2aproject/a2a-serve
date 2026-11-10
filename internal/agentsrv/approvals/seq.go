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

package approvals

import (
	"encoding/json"
	"maps"
	"math"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/google/jsonschema-go/jsonschema"
)

// seqField is the property of the input schema of a question that holds
// its number in its task, and the field of an answer that names it.
const seqField = "seq"

// questionsKey is the task metadata that counts the questions the task
// asked.
const questionsKey = "a2a-serve/questions"

// Ref names the question an answer is for: by the ID of its structured
// input request, by its number in the task, or both. Zero fields name
// nothing.
type Ref struct {
	ID  string
	Seq int
}

// Mismatch tells why an answer isn't for a question.
type Mismatch int

const (
	// Matches means that the answer is for the question, or names none.
	Matches Mismatch = iota
	// Earlier means that the answer names a question the task asked
	// before, which was answered.
	Earlier
	// Later means that the answer names a number the task hasn't reached.
	Later
	// Other means that the answer names another question by ID, or a
	// number for a question that has none.
	Other
	// BadSeq means that the number of the answer isn't a question number.
	BadSeq
)

// Check reports whether an answer that names ref is for q.
func (q *Question) Check(ref Ref) Mismatch {
	switch {
	case ref.Seq < 0:
		return BadSeq
	case ref.Seq > 0 && q.Seq == 0:
		return Other
	case ref.Seq > 0 && ref.Seq < q.Seq:
		return Earlier
	case ref.Seq > q.Seq:
		return Later
	case ref.ID != "" && ref.ID != q.ID:
		return Other
	}
	return Matches
}

// ReplyRef returns the question that a reply names: the ID of its
// structured input, and the "seq" of its structured input or of its data
// part with an "option". A "seq" that isn't a positive integer gives a
// negative Seq.
func ReplyRef(msg *a2a.Message) Ref {
	if msg == nil {
		return Ref{}
	}
	var untagged *a2a.Part
	for _, p := range msg.Parts {
		meta, ok := structuredinput.GetPartMetadata(p)
		if ok && meta.Type == structuredinput.PartTypeStructuredInput {
			return Ref{ID: meta.ID, Seq: dataSeq(p)}
		}
		data, _ := p.Data().(map[string]any)
		if _, option := data["option"]; !ok && option && untagged == nil {
			untagged = p
		}
	}
	if untagged == nil {
		return Ref{}
	}
	return Ref{Seq: dataSeq(untagged)}
}

func dataSeq(p *a2a.Part) int {
	data, _ := p.Data().(map[string]any)
	v, ok := data[seqField]
	if !ok {
		return 0
	}
	if n, ok := seqOf(v); ok && n > 0 {
		return n
	}
	return -1
}

// seqOf returns the integer of a decoded JSON number.
func seqOf(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != math.Trunc(n) || n > math.MaxInt32 || n < math.MinInt32 {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// Numbering numbers the questions of a task in the events of an
// execution. A question gets the next number of its task, unless it is the
// question asked last, asked again. The count goes in the task's metadata
// with the event that asks the question, so it is stored with it.
type Numbering struct {
	last  *Question
	count int
}

// NewNumbering continues the numbering of stored, the task as an execution
// starts, or nil for a new task.
func NewNumbering(stored *a2a.Task) *Numbering {
	n := &Numbering{}
	if stored == nil {
		return n
	}
	n.count, _ = seqOf(stored.Metadata[questionsKey])
	if n.last = FromStatus(stored.Status); n.last != nil {
		n.count = max(n.count, n.last.Seq)
	}
	return n
}

// Number returns ev with the number of the question it asks, if it asks
// one. On error, ev is to be used unchanged, and the numbering doesn't
// move.
func (n *Numbering) Number(ev a2a.Event) (a2a.Event, error) {
	var status a2a.TaskStatus
	switch v := ev.(type) {
	case *a2a.TaskStatusUpdateEvent:
		status = v.Status
	case *a2a.Task:
		status = v.Status
	default:
		return ev, nil
	}
	q := FromStatus(status)
	if q == nil {
		return ev, nil
	}
	count := n.count
	if n.last == nil || n.last.ID != q.ID {
		count++
		q.Seq = count
	} else {
		q.Seq = n.last.Seq
	}
	if q.Seq == 0 {
		n.last = q
		return ev, nil
	}
	msg, err := withSeq(status.Message, q.Seq)
	if err != nil {
		return ev, err
	}
	n.last, n.count = q, count
	switch v := ev.(type) {
	case *a2a.TaskStatusUpdateEvent:
		c := *v
		c.Status.Message = msg
		c.Metadata = withCount(v.Metadata, count)
		return &c, nil
	case *a2a.Task:
		c := *v
		c.Status.Message = msg
		c.Metadata = withCount(v.Metadata, count)
		return &c, nil
	}
	return ev, nil
}

func withCount(meta map[string]any, count int) map[string]any {
	out := maps.Clone(meta)
	if out == nil {
		out = map[string]any{}
	}
	out[questionsKey] = count
	return out
}

// withSeq returns msg, which asks a question, with seq as the number in the
// input schema of the question.
func withSeq(msg *a2a.Message, seq int) (*a2a.Message, error) {
	req, err := structuredinput.ParseRequest(msg)
	if err != nil || req == nil || req.Schema == nil {
		return msg, err
	}
	schema := *req.Schema
	schema.Properties = maps.Clone(req.Schema.Properties)
	schema.Properties[seqField] = &jsonschema.Schema{
		Type:        "integer",
		Const:       jsonschema.Ptr[any](seq),
		Description: "The number of this question in the task. Send it with the answer, so that a repeated answer can't answer a later question.",
	}
	part, err := structuredinput.NewSchemaPart(req.ID, &schema)
	if err != nil {
		return nil, err
	}
	out := *msg
	out.Parts = make([]*a2a.Part, len(msg.Parts))
	for i, p := range msg.Parts {
		out.Parts[i] = p
		if meta, ok := structuredinput.GetPartMetadata(p); ok && meta.Type == structuredinput.PartTypeInputSchema {
			out.Parts[i] = part
		}
	}
	return &out, nil
}
