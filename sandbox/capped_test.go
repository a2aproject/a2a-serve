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

package sandbox

import "testing"

func TestCappedKeepsTheStart(t *testing.T) {
	t.Parallel()
	c := &capped{max: 5}
	for _, p := range []string{"abc", "defg", "hij"} {
		if n, err := c.Write([]byte(p)); n != len(p) || err != nil {
			t.Fatalf("capped.Write(%q) = %d, %v, want %d, nil", p, n, err, len(p))
		}
	}
	if got := c.buf.String(); got != "abcde" || !c.truncated {
		t.Fatalf("capped = %q, truncated %v, want %q, true", got, c.truncated, "abcde")
	}
}

func TestGone(t *testing.T) {
	t.Parallel()
	for out, want := range map[string]bool{
		"Error response from daemon: No such container: a2a-x":                                         true,
		"Error response from daemon: removal of container a2a-x is already in progress":                true,
		"Error response from daemon: cannot remove container a2a-x: could not kill: permission denied": false,
	} {
		if got := gone([]byte(out)); got != want {
			t.Errorf("gone(%q) = %v, want %v", out, got, want)
		}
	}
}
