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
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/store"
)

func numberedMessage(t *testing.T, id string, seq int) *a2a.Message {
	t.Helper()
	msg := questionMessage(t, id, "git push", nil, [2]string{"once", "Allow"}, [2]string{"reject", "Deny"})
	if seq == 0 {
		return msg
	}
	msg, err := withSeq(msg, seq)
	if err != nil {
		t.Fatalf("withSeq() error = %v", err)
	}
	return msg
}

func TestQuestionCheck(t *testing.T) {
	t.Parallel()
	numbered := &Question{ID: "tool2", Seq: 2}
	unnumbered := &Question{ID: "tool2"}
	tests := []struct {
		name string
		q    *Question
		ref  Ref
		want Mismatch
	}{
		{name: "no reference", q: numbered, want: Matches},
		{name: "same number", q: numbered, ref: Ref{Seq: 2}, want: Matches},
		{name: "same ID and number", q: numbered, ref: Ref{ID: "tool2", Seq: 2}, want: Matches},
		{name: "same ID", q: numbered, ref: Ref{ID: "tool2"}, want: Matches},
		{name: "earlier number", q: numbered, ref: Ref{Seq: 1}, want: Earlier},
		{name: "earlier number with the same ID", q: numbered, ref: Ref{ID: "tool2", Seq: 1}, want: Earlier},
		{name: "later number", q: numbered, ref: Ref{Seq: 3}, want: Later},
		{name: "other ID", q: numbered, ref: Ref{ID: "tool1"}, want: Other},
		{name: "bad number", q: numbered, ref: Ref{Seq: -1}, want: BadSeq},
		{name: "number for a question without one", q: unnumbered, ref: Ref{Seq: 1}, want: Other},
		{name: "no number for a question without one", q: unnumbered, ref: Ref{ID: "tool2"}, want: Matches},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.q.Check(tc.ref); got != tc.want {
				t.Fatalf("Question.Check(%+v) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

func TestReplyRef(t *testing.T) {
	t.Parallel()
	tagged := func(id string, data map[string]any) *a2a.Part {
		p, err := structuredinput.NewResponsePart(id, data)
		if err != nil {
			t.Fatalf("structuredinput.NewResponsePart() error = %v", err)
		}
		return p
	}
	tests := []struct {
		name  string
		parts []*a2a.Part
		want  Ref
	}{
		{name: "typed reply", parts: []*a2a.Part{a2a.NewTextPart("once")}, want: Ref{}},
		{name: "data part without a number", parts: []*a2a.Part{a2a.NewDataPart(map[string]any{"option": "once"})}, want: Ref{}},
		{name: "data part with a number", parts: []*a2a.Part{a2a.NewDataPart(map[string]any{"option": "once", "seq": float64(2)})}, want: Ref{Seq: 2}},
		{name: "data part with an int", parts: []*a2a.Part{a2a.NewDataPart(map[string]any{"option": "once", "seq": 2})}, want: Ref{Seq: 2}},
		{name: "data part with a number and no option", parts: []*a2a.Part{a2a.NewDataPart(map[string]any{"seq": float64(2)})}, want: Ref{}},
		{name: "data part with a text number", parts: []*a2a.Part{a2a.NewDataPart(map[string]any{"option": "once", "seq": "2"})}, want: Ref{Seq: -1}},
		{name: "data part with a fraction", parts: []*a2a.Part{a2a.NewDataPart(map[string]any{"option": "once", "seq": 1.5})}, want: Ref{Seq: -1}},
		{name: "data part with zero", parts: []*a2a.Part{a2a.NewDataPart(map[string]any{"option": "once", "seq": float64(0)})}, want: Ref{Seq: -1}},
		{name: "structured input", parts: []*a2a.Part{tagged("tool2", map[string]any{"option": "once"})}, want: Ref{ID: "tool2"}},
		{name: "structured input with a number", parts: []*a2a.Part{tagged("tool2", map[string]any{"option": "once", "seq": 2})}, want: Ref{ID: "tool2", Seq: 2}},
		{
			name:  "structured input after a data part",
			parts: []*a2a.Part{a2a.NewDataPart(map[string]any{"option": "once", "seq": float64(1)}), tagged("tool2", map[string]any{"option": "once", "seq": 2})},
			want:  Ref{ID: "tool2", Seq: 2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ReplyRef(a2a.NewMessage(a2a.MessageRoleUser, tc.parts...))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("ReplyRef() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestFromMessageReadsTheNumber(t *testing.T) {
	t.Parallel()
	q := FromMessage(numberedMessage(t, "tool2", 2))
	if q == nil || q.Seq != 2 || q.Data["seq"] != 2 {
		t.Fatalf("FromMessage() = %+v, want question 2", q)
	}
	raw, err := json.Marshal(q.Data)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got := FromRecord(store.Question{Data: stored}); got.Seq != 2 {
		t.Fatalf("FromRecord() seq = %d, want 2", got.Seq)
	}
}

func TestNumbering(t *testing.T) {
	t.Parallel()
	asking := func(id string) a2a.Event {
		return a2a.NewStatusUpdateEvent(&a2a.Task{ID: "t1", ContextID: "c1"}, a2a.TaskStateInputRequired, numberedMessage(t, id, 0))
	}
	working := a2a.NewStatusUpdateEvent(&a2a.Task{ID: "t1", ContextID: "c1"}, a2a.TaskStateWorking, nil)
	waiting := func(id string, seq int, count any) *a2a.Task {
		task := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: numberedMessage(t, id, seq)}}
		if count != nil {
			task.Metadata = map[string]any{questionsKey: count}
		}
		return task
	}
	tests := []struct {
		name   string
		stored *a2a.Task
		events []a2a.Event
		want   []int
	}{
		{name: "new task", events: []a2a.Event{working, asking("tool1")}, want: []int{0, 1}},
		{name: "question asked again in one execution", events: []a2a.Event{asking("tool1"), asking("tool1"), asking("tool2")}, want: []int{1, 1, 2}},
		{name: "answer to the stored question", stored: waiting("tool1", 1, float64(1)), events: []a2a.Event{working, asking("tool2")}, want: []int{0, 2}},
		{name: "stored question asked again", stored: waiting("tool1", 1, float64(1)), events: []a2a.Event{asking("tool1")}, want: []int{1}},
		{name: "count beyond the stored question", stored: &a2a.Task{ID: "t1", Metadata: map[string]any{questionsKey: float64(3)}}, events: []a2a.Event{asking("tool4")}, want: []int{4}},
		{name: "stored question without a number asked again", stored: waiting("tool1", 0, nil), events: []a2a.Event{asking("tool1"), asking("tool2")}, want: []int{0, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := NewNumbering(tc.stored)
			var got []int
			for _, ev := range tc.events {
				numbered, err := n.Number(ev)
				if err != nil {
					t.Fatalf("Numbering.Number() error = %v", err)
				}
				u := numbered.(*a2a.TaskStatusUpdateEvent)
				q := FromStatus(u.Status)
				if q == nil {
					got = append(got, 0)
					continue
				}
				got = append(got, q.Seq)
				if count, _ := seqOf(u.Metadata[questionsKey]); q.Seq > 0 && count < q.Seq {
					t.Fatalf("Numbering.Number() task metadata = %v, want a count of at least %d", u.Metadata, q.Seq)
				}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Numbering.Number() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestNumberingKeepsTheRestOfTheQuestion(t *testing.T) {
	t.Parallel()
	msg := withToolCall(numberedMessage(t, "tool1", 0), "execute")
	ev := a2a.NewStatusUpdateEvent(&a2a.Task{ID: "t1", ContextID: "c1"}, a2a.TaskStateInputRequired, msg)
	ev.Metadata = map[string]any{"other": "kept"}

	numbered, err := NewNumbering(nil).Number(ev)

	if err != nil {
		t.Fatalf("Numbering.Number() error = %v", err)
	}
	u := numbered.(*a2a.TaskStatusUpdateEvent)
	want := FromMessage(msg)
	want.Seq, want.Data["seq"] = 1, 1
	if diff := cmp.Diff(want, FromStatus(u.Status)); diff != "" {
		t.Fatalf("Numbering.Number() question wrong result (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff(map[string]any{"other": "kept", questionsKey: 1}, u.Metadata); diff != "" {
		t.Fatalf("Numbering.Number() metadata wrong result (-want +got) diff = %s", diff)
	}
	if FromStatus(ev.Status).Seq != 0 || len(ev.Metadata) != 1 {
		t.Fatalf("Numbering.Number() changed the event it was given")
	}
}
