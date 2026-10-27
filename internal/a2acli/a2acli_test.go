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

package a2acli

import "testing"

func TestModuleVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{in: "", want: "latest"},
		{in: "dev", want: "latest"},
		{in: "(devel)", want: "latest"},
		{in: "0.4.0", want: "v0.4.0"},
		{in: "v0.4.0", want: "v0.4.0"},
		{in: "v0.4.1-0.20261022125340-9a4ea7697c42", want: "v0.4.1-0.20261022125340-9a4ea7697c42"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := moduleVersion(tc.in); got != tc.want {
				t.Fatalf("moduleVersion(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
