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

package project

import (
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestVars(t *testing.T) {
	t.Parallel()
	cfg := &agentdir.Config{Env: []string{"REGION=eu", "REGION=us", "BAD"}, Secrets: []string{"GH_TOKEN", "EMPTY", "UNSTORED"}}
	secrets := agentdir.Secrets{"GH_TOKEN": "gh", "EMPTY": "", "BOT_TOKEN": "bot"}
	tests := []struct {
		name   string
		want   string
		wantOK bool
	}{
		{name: "REGION", want: "us", wantOK: true},
		{name: "GH_TOKEN", want: "gh", wantOK: true},
		{name: "EMPTY"},
		{name: "UNSTORED"},
		{name: "BOT_TOKEN"},
		{name: "BAD"},
	}
	vars := Vars(cfg, secrets)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := vars(tc.name)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("Vars()(%q) = %q, %v, want %q, %v", tc.name, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
