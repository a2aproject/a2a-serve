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

package doctor

import "testing"

const scriptCmd = "curl -fsSL https://raw.githubusercontent.com/a2aproject/a2a-serve/main/install.sh | "

func TestA2ALocationsInstall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		loc  a2aLocations
		path string
		want string
	}{
		{
			name: "homebrew cellar",
			loc:  a2aLocations{home: "/home/u"},
			path: "/usr/local/Cellar/a2a/0.3.0/bin/a2a",
			want: "brew upgrade a2aproject/a2a-cli/a2a",
		},
		{
			name: "homebrew prefix",
			loc:  a2aLocations{home: "/Users/u", homebrewPrefix: "/opt/homebrew"},
			path: "/opt/homebrew/bin/a2a",
			want: "brew upgrade a2aproject/a2a-cli/a2a",
		},
		{
			name: "linuxbrew prefix",
			loc:  a2aLocations{home: "/home/u", homebrewPrefix: "/home/linuxbrew/.linuxbrew"},
			path: "/home/linuxbrew/.linuxbrew/bin/a2a",
			want: "brew upgrade a2aproject/a2a-cli/a2a",
		},
		{
			name: "outside the homebrew prefix",
			loc:  a2aLocations{home: "/home/u", homebrewPrefix: "/opt/homebrew"},
			path: "/opt/homebrew-other/bin/a2a",
			want: scriptCmd + "INSTALL_DIR=/opt/homebrew-other/bin sh",
		},
		{
			name: "default go bin",
			loc:  a2aLocations{home: "/home/u"},
			path: "/home/u/go/bin/a2a",
			want: "GOBIN=/home/u/go/bin go install github.com/a2aproject/a2a-cli@latest && mv /home/u/go/bin/a2a-cli /home/u/go/bin/a2a",
		},
		{
			name: "GOBIN",
			loc:  a2aLocations{home: "/home/u", gobin: "/opt/gobin"},
			path: "/opt/gobin/a2a",
			want: "GOBIN=/opt/gobin go install github.com/a2aproject/a2a-cli@latest && mv /opt/gobin/a2a-cli /opt/gobin/a2a",
		},
		{
			name: "GOBIN replaces the default go bin",
			loc:  a2aLocations{home: "/home/u", gobin: "/opt/gobin"},
			path: "/home/u/go/bin/a2a",
			want: scriptCmd + "INSTALL_DIR=/home/u/go/bin sh",
		},
		{
			name: "second GOPATH entry",
			loc:  a2aLocations{home: "/home/u", gopath: "/a:/b"},
			path: "/b/bin/a2a",
			want: "GOBIN=/b/bin go install github.com/a2aproject/a2a-cli@latest && mv /b/bin/a2a-cli /b/bin/a2a",
		},
		{
			name: "go bin with a space",
			loc:  a2aLocations{home: "/home/u", gobin: "/opt/my go/bin"},
			path: "/opt/my go/bin/a2a",
			want: "GOBIN='/opt/my go/bin' go install github.com/a2aproject/a2a-cli@latest && mv '/opt/my go/bin/a2a-cli' '/opt/my go/bin/a2a'",
		},
		{
			name: "install script default dir",
			loc:  a2aLocations{home: "/home/u"},
			path: "/home/u/.local/bin/a2a",
			want: scriptCmd + "sh",
		},
		{
			name: "other dir",
			loc:  a2aLocations{home: "/home/u"},
			path: "/usr/local/bin/a2a",
			want: scriptCmd + "INSTALL_DIR=/usr/local/bin sh",
		},
		{
			name: "not installed",
			loc:  a2aLocations{home: "/home/u"},
			want: scriptCmd + "sh",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.loc.install(tc.path); got != tc.want {
				t.Fatalf("a2aLocations.install(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestCheckA2AFixFollowsInstallLocation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(m *fakeMachine)
		want  string
	}{
		{
			name: "homebrew",
			setup: func(m *fakeMachine) {
				m.env["HOMEBREW_PREFIX"] = "/opt/homebrew"
				m.paths["a2a"] = "/opt/homebrew/bin/a2a"
				m.outputs["/opt/homebrew/bin/a2a version -o json"] = `{"version":"0.3.1"}`
			},
			want: "brew upgrade a2aproject/a2a-cli/a2a",
		},
		{
			name: "go bin outside PATH",
			setup: func(m *fakeMachine) {
				m.env["GOBIN"] = "/opt/gobin"
				m.paths["a2a"] = "/opt/gobin/a2a"
				m.outputs["/opt/gobin/a2a version -o json"] = `{"version":"0.3.1"}`
			},
			want: "GOBIN=/opt/gobin go install github.com/a2aproject/a2a-cli@latest && mv /opt/gobin/a2a-cli /opt/gobin/a2a",
		},
		{
			name:  "missing",
			setup: func(m *fakeMachine) { delete(m.paths, "a2a") },
			want:  scriptCmd + "sh",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newFakeMachine(t)
			m.outputs["/usr/bin/a2a version -o json"] = `{"version":"0.3.1"}`
			tc.setup(m)
			got := checkA2A(t.Context(), m.Env())
			if got.Status != StatusFail {
				t.Fatalf("checkA2A().Status = %v, want %v", got.Status, StatusFail)
			}
			if got.Fix != tc.want {
				t.Fatalf("checkA2A().Fix = %q, want %q", got.Fix, tc.want)
			}
		})
	}
}
