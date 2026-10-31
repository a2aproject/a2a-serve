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
		{in: "", want: PinnedVersion},
		{in: "dev", want: PinnedVersion},
		{in: "(devel)", want: PinnedVersion},
		{in: "0.4.0", want: "v0.4.0"},
		{in: "v0.4.0", want: "v0.4.0"},
		{in: "v0.4.1-0.20261022125340-9a4ea7697c42", want: "v0.4.1-0.20261022125340-9a4ea7697c42"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := ModuleVersion(tc.in); got != tc.want {
				t.Fatalf("ModuleVersion(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestOutdated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		want    bool
	}{
		{version: "v0.3.1", want: true},
		{version: "v0.3.2-0.20261027071959-4abaf842d69f", want: true},
		{version: PinnedVersion, want: false},
		{version: "v0.3.2", want: false},
		{version: "v0.4.0", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.version, func(t *testing.T) {
			t.Parallel()
			if got := Outdated(tc.version); got != tc.want {
				t.Fatalf("Outdated(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}
