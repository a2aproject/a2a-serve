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

package service

import "testing"

func TestLaunchdPID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
		want int
	}{
		{
			name: "running",
			out:  "gui/501/org.a2aproject.a2a-coder = {\n\tactive count = 1\n\tstate = running\n\tpid = 4321\n\tendpoints = {\n\t\tpid = 7\n\t}\n}\n",
			want: 4321,
		},
		{
			name: "nested pid only",
			out:  "gui/501/org.a2aproject.a2a-coder = {\n\tstate = not running\n\tspawn = {\n\t\tpid = 7\n\t}\n}\n",
		},
		{
			name: "not running",
			out:  "gui/501/org.a2aproject.a2a-coder = {\n\tstate = not running\n\tlast exit code = 0\n}\n",
		},
		{
			name: "malformed pid",
			out:  "\tpid = x\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := launchdPID(tc.out); got != tc.want {
				t.Fatalf("launchdPID() = %d, want %d", got, tc.want)
			}
		})
	}
}
