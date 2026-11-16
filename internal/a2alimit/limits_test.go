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

package a2alimit

import (
	"errors"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

func TestLimitsCheck(t *testing.T) {
	t.Parallel()
	limits := Limits{Parts: 3, Text: 12, Metadata: 12, Extensions: 2, References: 2, ID: 4}
	tests := []struct {
		name       string
		parts      []*a2a.Part
		metadata   map[string]any
		extensions []string
		references []a2a.TaskID
		wantErr    string
	}{
		{name: "within the limits", parts: []*a2a.Part{a2a.NewTextPart("ab"), a2a.NewTextPart("cde")}},
		{name: "files don't count as text", parts: []*a2a.Part{a2a.NewTextPart("abcdefghijkl"), a2a.NewRawPart([]byte("123456"))}},
		{
			name:    "too many parts",
			parts:   []*a2a.Part{a2a.NewTextPart("a"), a2a.NewRawPart([]byte("b")), a2a.NewDataPart(1), a2a.NewFileURLPart("https://example.com/f", "text/plain")},
			wantErr: "a message may have at most 3 parts, got 4",
		},
		{
			name:    "too much text",
			parts:   []*a2a.Part{a2a.NewTextPart("abcdef"), a2a.NewTextPart("ghijklm")},
			wantErr: "the text and data of a message may have at most 12 bytes, got 13",
		},
		{name: "data within the limit", parts: []*a2a.Part{a2a.NewTextPart("ab"), a2a.NewDataPart(map[string]any{"k": "1"})}},
		{
			name:    "data counts toward the text",
			parts:   []*a2a.Part{a2a.NewTextPart("ab"), a2a.NewDataPart(map[string]any{"k": "1234"})},
			wantErr: "the text and data of a message may have at most 12 bytes, got 14",
		},
		{name: "metadata within the limit", parts: []*a2a.Part{a2a.NewTextPart("a")}, metadata: map[string]any{"k": "v"}},
		{
			name:     "too much metadata",
			parts:    []*a2a.Part{{Content: a2a.Text("a"), Metadata: map[string]any{"k": "v"}}},
			metadata: map[string]any{"k": "v"},
			wantErr:  "the metadata of a message and its parts may have at most 12 bytes, got 18",
		},
		{name: "null part", parts: []*a2a.Part{nil}, wantErr: "part 1 of a message is null"},
		{name: "null part after a text part", parts: []*a2a.Part{a2a.NewTextPart("hi"), nil}, wantErr: "part 2 of a message is null"},
		{name: "extensions and references within the limits", parts: []*a2a.Part{a2a.NewTextPart("a")}, extensions: []string{"e1", "e2"}, references: []a2a.TaskID{"t1", "t2"}},
		{
			name:       "too many extensions",
			parts:      []*a2a.Part{a2a.NewTextPart("a")},
			extensions: []string{"e1", "e2", "e3"},
			wantErr:    "a message may have at most 2 extensions, got 3",
		},
		{
			name:       "too long extension",
			parts:      []*a2a.Part{a2a.NewTextPart("a")},
			extensions: []string{"e1", "e2345"},
			wantErr:    "each of the extensions of a message may have at most 4 bytes, got 5",
		},
		{
			name:       "too many referenced tasks",
			parts:      []*a2a.Part{a2a.NewTextPart("a")},
			references: []a2a.TaskID{"t1", "t2", "t3"},
			wantErr:    "a message may have at most 2 referenceTaskIds, got 3",
		},
		{
			name:       "too long referenced task",
			parts:      []*a2a.Part{a2a.NewTextPart("a")},
			references: []a2a.TaskID{"t12345"},
			wantErr:    "each of the referenceTaskIds of a message may have at most 4 bytes, got 6",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msg := a2a.NewMessage(a2a.MessageRoleUser, tt.parts...)
			msg.Metadata = tt.metadata
			msg.Extensions = tt.extensions
			msg.ReferenceTasks = tt.references
			err := limits.Check(msg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Limits.Check() error = %v, want nil", err)
				}
				return
			}
			var a2aErr *a2a.Error
			if !errors.As(err, &a2aErr) || !errors.Is(err, a2a.ErrInvalidParams) || a2aErr.Message != tt.wantErr {
				t.Fatalf("Limits.Check() error = %v, want invalid params %q", err, tt.wantErr)
			}
		})
	}
}

func TestLimitsCheckParts(t *testing.T) {
	t.Parallel()
	limits := Limits{Parts: 1, Text: 5, Metadata: 20}

	err := limits.CheckParts("a reply", []*a2a.Part{a2a.NewTextPart("a")}, map[string]any{"k": "v"}, map[string]any{"key": "value"})

	var a2aErr *a2a.Error
	if want := "the metadata of a reply and its parts may have at most 20 bytes, got 24"; !errors.As(err, &a2aErr) || !errors.Is(err, a2a.ErrInvalidParams) || a2aErr.Message != want {
		t.Fatalf("Limits.CheckParts() error = %v, want invalid params %q", err, want)
	}
}

func TestMessageCheck(t *testing.T) {
	t.Parallel()
	check := NewMessageCheck(Limits{Parts: 1, Text: 10})
	tooMany := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("a"), a2a.NewTextPart("b"))
	tests := []struct {
		name    string
		payload any
		wantErr error
	}{
		{name: "message over the limits", payload: &a2a.SendMessageRequest{Message: tooMany}, wantErr: a2a.ErrInvalidParams},
		{name: "message within the limits", payload: &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("a"))}},
		{name: "message of the agent role", payload: &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("a"))}, wantErr: a2a.ErrInvalidParams},
		{name: "message of an unknown role", payload: &a2a.SendMessageRequest{Message: a2a.NewMessage("ROLE_SYSTEM", a2a.NewTextPart("a"))}, wantErr: a2a.ErrInvalidParams},
		{name: "request without a message", payload: &a2a.SendMessageRequest{}},
		{name: "other request", payload: &a2a.GetTaskRequest{ID: "t1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, res, err := check.Before(t.Context(), &a2asrv.CallContext{}, &a2asrv.Request{Payload: tt.payload})
			if res != nil {
				t.Fatalf("MessageCheck.Before() = %v, want no result", res)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("MessageCheck.Before() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
