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

package cli

import (
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/shlex"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestStartServiceWhileDetachedHintStopsTheAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not used on this platform")
	}
	tests := []struct {
		name string
		args []string
	}{
		{name: "saved agent", args: []string{"start", "--service"}},
		{name: "agent argument", args: []string{"start", "opencode", "--service"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})
			if err := d.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			f := serveFakeControl(t, d)
			t.Chdir(d.Root)

			_, err := runCMD(t, tc.args...)
			if err == nil {
				t.Fatalf("a2a-serve %s error = nil, want an error with a hint", strings.Join(tc.args, " "))
			}
			hint := classify(err, nil).Hint
			var printed []string
			for i, part := range strings.Split(hint, "`") {
				if i%2 == 1 {
					printed = append(printed, part)
				}
			}
			if diff := cmp.Diff([]string{"a2a-serve stop", "a2a-serve start --service"}, printed); diff != "" {
				t.Fatalf("a2a-serve %s hint commands wrong result (-want +got) diff = %s", strings.Join(tc.args, " "), diff)
			}
			words, err := shlex.Split(printed[0])
			if err != nil {
				t.Fatalf("shlex.Split(%q) error = %v", printed[0], err)
			}
			if _, err := runCMD(t, words[1:]...); err != nil {
				t.Fatalf("%s error = %v", printed[0], err)
			}
			if !slices.Contains(f.Calls(), "POST /stop") {
				t.Fatalf("%s didn't stop the agent: calls = %v", printed[0], f.Calls())
			}
		})
	}
}
