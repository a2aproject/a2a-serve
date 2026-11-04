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

// Package a2atext extracts the text of A2A messages and parts.
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
