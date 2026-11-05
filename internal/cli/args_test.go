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
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-serve/internal/clierr"
)

func TestNamedArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		use         string
		args        []string
		wantMessage string
		wantHint    string
	}{
		{name: "all required given", use: "join <agent-url> <code>", args: []string{"u", "c"}},
		{name: "none given", use: "join <agent-url> <code>", args: nil, wantMessage: "root join: missing <agent-url> and <code>", wantHint: "usage: root join <agent-url> <code>"},
		{name: "second missing", use: "join <agent-url> <code>", args: []string{"u"}, wantMessage: "root join: missing <code>", wantHint: "usage: root join <agent-url> <code>"},
		{name: "extra", use: "add <name>", args: []string{"a", "b"}, wantMessage: `root add: unexpected argument "b"`, wantHint: "usage: root add <name>"},
		{name: "alternatives in one placeholder", use: "rm <name|agent-url>", args: []string{"anna"}},
		{name: "optional omitted", use: "start [<agent>]", args: nil},
		{name: "optional given", use: "start [<agent>]", args: []string{"coder"}},
		{name: "optional exceeded", use: "start [<agent>]", args: []string{"a", "b"}, wantMessage: `root start: unexpected argument "b"`, wantHint: "usage: root start [<agent>]"},
		{name: "variadic with none", use: "unset <setting> [<entry>...]", args: []string{"env"}},
		{name: "variadic with many", use: "unset <setting> [<entry>...]", args: []string{"env", "A", "B"}},
		{name: "variadic missing required", use: "unset <setting> [<entry>...]", args: nil, wantMessage: "root unset: missing <setting>", wantHint: "usage: root unset <setting> [<entry>...]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := &cobra.Command{Use: "root"}
			cmd := &cobra.Command{Use: tc.use}
			root.AddCommand(cmd)
			err := namedArgs(cmd, tc.args)
			if tc.wantMessage == "" {
				if err != nil {
					t.Fatalf("namedArgs(%q, %q) error = %v, want nil", tc.use, tc.args, err)
				}
				return
			}
			got := clierr.Classify(err)
			if got == nil || got.Code != clierr.CodeUsage {
				t.Fatalf("namedArgs(%q, %q) error = %v, want a usage error", tc.use, tc.args, err)
			}
			if diff := cmp.Diff([]string{tc.wantMessage, tc.wantHint}, []string{got.Message, got.Hint}); diff != "" {
				t.Fatalf("namedArgs(%q, %q) message and hint wrong result (-want +got) diff = %s", tc.use, tc.args, diff)
			}
		})
	}
}
