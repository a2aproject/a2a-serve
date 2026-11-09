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
	limits := Limits{Parts: 3, Text: 5, Metadata: 12}
	tests := []struct {
		name     string
		parts    []*a2a.Part
		metadata map[string]any
		wantErr  string
	}{
		{name: "within the limits", parts: []*a2a.Part{a2a.NewTextPart("ab"), a2a.NewTextPart("cde")}},
		{name: "files and data don't count as text", parts: []*a2a.Part{a2a.NewTextPart("abcde"), a2a.NewRawPart([]byte("123456")), a2a.NewDataPart(map[string]any{"k": "123456"})}},
		{
			name:    "too many parts",
			parts:   []*a2a.Part{a2a.NewTextPart("a"), a2a.NewRawPart([]byte("b")), a2a.NewDataPart(1), a2a.NewFileURLPart("https://example.com/f", "text/plain")},
			wantErr: "a message may have at most 3 parts, got 4",
		},
		{
			name:    "too much text",
			parts:   []*a2a.Part{a2a.NewTextPart("abc"), a2a.NewTextPart("def")},
			wantErr: "the text of a message may have at most 5 bytes, got 6",
		},
		{name: "metadata within the limit", parts: []*a2a.Part{a2a.NewTextPart("a")}, metadata: map[string]any{"k": "v"}},
		{
			name:     "too much metadata",
			parts:    []*a2a.Part{{Content: a2a.Text("a"), Metadata: map[string]any{"k": "v"}}},
			metadata: map[string]any{"k": "v"},
			wantErr:  "the metadata of a message and its parts may have at most 12 bytes, got 18",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msg := a2a.NewMessage(a2a.MessageRoleUser, tt.parts...)
			msg.Metadata = tt.metadata
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
