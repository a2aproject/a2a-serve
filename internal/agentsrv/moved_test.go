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

package agentsrv

import "testing"

func TestLastBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{name: "short", s: "User: hi", n: 100, want: "User: hi"},
		{name: "cut at a line", s: "User: one\n\nAgent: two", n: 12, want: "Agent: two"},
		{name: "one long line", s: "Agent: ééé", n: 5, want: "éé"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lastBytes(tc.s, tc.n); got != tc.want {
				t.Fatalf("lastBytes(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
			}
		})
	}
}
