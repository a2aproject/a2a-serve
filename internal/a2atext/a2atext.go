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

// Package a2atext extracts the text of A2A messages and parts, merges the
// text chunks of streamed artifacts, and names task states.
package a2atext

import (
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Parts joins the text parts with newlines.
func Parts(parts []*a2a.Part) string {
	var out []string
	for _, p := range parts {
		if t := p.Text(); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

// Message joins the text parts of a message with newlines.
func Message(m *a2a.Message) string {
	if m == nil {
		return ""
	}
	return Parts(m.Parts)
}

// StateName is the short lowercase name of a task state, such as
// "input-required".
func StateName(s a2a.TaskState) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(string(s), "TASK_STATE_")), "_", "-")
}

// MergeChunks joins each run of adjacent text parts in parts[from:], such as
// the chunks of a streamed artifact, into one part. Parts in a run have the
// same file name and media type, and no metadata. It returns a new slice and
// doesn't change the parts.
func MergeChunks(parts []*a2a.Part, from int) []*a2a.Part {
	out := make([]*a2a.Part, 0, len(parts))
	out = append(out, parts[:from]...)
	for i := from; i < len(parts); {
		j, size := i+1, len(parts[i].Text())
		for j < len(parts) && sameChunk(parts[i], parts[j]) {
			size += len(parts[j].Text())
			j++
		}
		if j == i+1 {
			out = append(out, parts[i])
			i = j
			continue
		}
		var sb strings.Builder
		sb.Grow(size)
		for _, p := range parts[i:j] {
			sb.WriteString(p.Text())
		}
		merged := *parts[i]
		merged.Content = a2a.Text(sb.String())
		merged.Metadata = nil
		out = append(out, &merged)
		i = j
	}
	return out
}

func sameChunk(a, b *a2a.Part) bool {
	_, aText := a.Content.(a2a.Text)
	_, bText := b.Content.(a2a.Text)
	return aText && bText && a.Filename == b.Filename && a.MediaType == b.MediaType && len(a.Metadata) == 0 && len(b.Metadata) == 0
}

// Result is the text of a finished task: its artifacts, or its status
// message.
func Result(t *a2a.Task) string {
	var parts []string
	for _, a := range t.Artifacts {
		var sb strings.Builder
		for _, p := range a.Parts {
			sb.WriteString(p.Text())
		}
		if sb.Len() > 0 {
			parts = append(parts, sb.String())
		}
	}
	if len(parts) == 0 {
		return Message(t.Status.Message)
	}
	return strings.Join(parts, "\n\n")
}
