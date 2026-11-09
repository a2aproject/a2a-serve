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

package shellquote

import (
	"os/exec"
	"testing"
)

func TestQuote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "nas.lan:22", want: "nas.lan:22"},
		{name: "IPv6 address", in: "[fd00::1]:22", want: "'[fd00::1]:22'"},
		{name: "glob", in: "*.lan", want: "'*.lan'"},
		{name: "single quote", in: "it's", want: `'it'\''s'`},
		{name: "empty", in: "", want: "''"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Quote(tc.in); got != tc.want {
				t.Fatalf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestQuoteRunsInShells(t *testing.T) {
	t.Parallel()
	for _, shell := range []string{"sh", "zsh"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			t.Logf("%s not on PATH", shell)
			continue
		}
		for _, arg := range []string{"[fd00::1]:22", "*.lan", "it's", "$HOME"} {
			out, err := exec.Command(path, "-c", "printf %s "+Quote(arg)).Output()
			if err != nil {
				t.Fatalf("%s -c printf %s error = %v", shell, Quote(arg), err)
			}
			if string(out) != arg {
				t.Fatalf("%s -c printf %s = %q, want %q", shell, Quote(arg), out, arg)
			}
		}
	}
}
