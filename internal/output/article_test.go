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

package output

import "testing"

func TestWithArticle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		noun string
		want string
	}{
		{noun: "opencode", want: "an opencode"},
		{noun: "acp", want: "an acp"},
		{noun: "claude", want: "a claude"},
	}
	for _, tc := range tests {
		t.Run(tc.noun, func(t *testing.T) {
			t.Parallel()
			if got := WithArticle(tc.noun); got != tc.want {
				t.Fatalf("WithArticle(%q) = %q, want %q", tc.noun, got, tc.want)
			}
		})
	}
}
