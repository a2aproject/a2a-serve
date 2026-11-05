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

package a2atext

import (
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
)

func TestMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  *a2a.Message
		want string
	}{
		{name: "nil", want: ""},
		{name: "no parts", msg: a2a.NewMessage(a2a.MessageRoleUser), want: ""},
		{
			name: "text parts joined",
			msg:  a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("one"), a2a.NewTextPart("two")),
			want: "one\ntwo",
		},
		{
			name: "non-text parts skipped",
			msg:  a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("one"), a2a.NewDataPart(map[string]any{"k": "v"})),
			want: "one",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Message(tc.msg); got != tc.want {
				t.Fatalf("Message() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMergeChunks(t *testing.T) {
	t.Parallel()
	text := a2a.NewTextPart
	typed := func(s, mediaType string) *a2a.Part {
		p := a2a.NewTextPart(s)
		p.MediaType = mediaType
		return p
	}
	withMeta := func(s string) *a2a.Part {
		p := a2a.NewTextPart(s)
		p.Metadata = map[string]any{"k": "v"}
		return p
	}
	raw := a2a.NewRawPart([]byte("png"))
	tests := []struct {
		name  string
		parts []*a2a.Part
		from  int
		want  []*a2a.Part
	}{
		{name: "text joined", parts: []*a2a.Part{text("a"), text("b"), text("c")}, want: []*a2a.Part{text("abc")}},
		{name: "parts before from kept", parts: []*a2a.Part{text("a"), text("b"), text("c")}, from: 1, want: []*a2a.Part{text("a"), text("bc")}},
		{name: "raw part splits runs", parts: []*a2a.Part{text("a"), raw, text("b"), text("c")}, want: []*a2a.Part{text("a"), raw, text("bc")}},
		{name: "same media type joined", parts: []*a2a.Part{typed("a", "text/markdown"), typed("b", "text/markdown")}, want: []*a2a.Part{typed("ab", "text/markdown")}},
		{name: "other media type kept", parts: []*a2a.Part{text("a"), typed("b", "text/markdown")}, want: []*a2a.Part{text("a"), typed("b", "text/markdown")}},
		{name: "metadata kept", parts: []*a2a.Part{text("a"), withMeta("b"), text("c")}, want: []*a2a.Part{text("a"), withMeta("b"), text("c")}},
		{name: "empty", parts: []*a2a.Part{}, want: []*a2a.Part{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tc.want, MergeChunks(tc.parts, tc.from)); diff != "" {
				t.Fatalf("MergeChunks() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
