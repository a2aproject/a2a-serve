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

import (
	"os"
	"testing"
)

const scriptCmd = "curl -fsSL https://raw.githubusercontent.com/a2aproject/a2a-serve/main/install.sh | "

func writableDirs(dirs ...string) func(string) bool {
	return func(dir string) bool {
		for _, d := range dirs {
			if d == dir {
				return true
			}
		}
		return false
	}
}

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
			loc:  a2aLocations{home: "/home/u", homebrewPrefix: "/opt/homebrew", writable: writableDirs("/opt/homebrew-other/bin")},
			path: "/opt/homebrew-other/bin/a2a",
			want: scriptCmd + "INSTALL_DIR=/opt/homebrew-other/bin A2A_CLI_ONLY=1 sh",
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
			loc:  a2aLocations{home: "/home/u", gobin: "/opt/gobin", writable: writableDirs("/home/u/go/bin")},
			path: "/home/u/go/bin/a2a",
			want: scriptCmd + "INSTALL_DIR=/home/u/go/bin A2A_CLI_ONLY=1 sh",
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
			want: scriptCmd + "A2A_CLI_ONLY=1 sh",
		},
		{
			name: "writable other dir",
			loc:  a2aLocations{home: "/home/u", writable: writableDirs("/usr/local/bin")},
			path: "/usr/local/bin/a2a",
			want: scriptCmd + "INSTALL_DIR=/usr/local/bin A2A_CLI_ONLY=1 sh",
		},
		{
			name: "writable other dir with a space",
			loc:  a2aLocations{home: "/home/u", writable: writableDirs("/opt/my tools")},
			path: "/opt/my tools/a2a",
			want: scriptCmd + "INSTALL_DIR='/opt/my tools' A2A_CLI_ONLY=1 sh",
		},
		{
			name: "unwritable other dir",
			loc:  a2aLocations{home: "/home/u", writable: writableDirs()},
			path: "/usr/bin/a2a",
			want: "run `" + scriptCmd + "A2A_CLI_ONLY=1 sh`, then remove /usr/bin/a2a or put ~/.local/bin before /usr/bin on PATH",
		},
		{
			name: "other dir without a writability check",
			loc:  a2aLocations{home: "/home/u"},
			path: "/usr/local/bin/a2a",
			want: "run `" + scriptCmd + "A2A_CLI_ONLY=1 sh`, then remove /usr/local/bin/a2a or put ~/.local/bin before /usr/local/bin on PATH",
		},
		{
			name: "not installed",
			loc:  a2aLocations{home: "/home/u"},
			want: scriptCmd + "A2A_CLI_ONLY=1 sh",
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
			want:  scriptCmd + "A2A_CLI_ONLY=1 sh",
		},
		{
			name: "unwritable dir",
			setup: func(m *fakeMachine) {
				m.paths["a2a"] = "/usr/local/bin/a2a"
				m.outputs["/usr/local/bin/a2a version -o json"] = `{"version":"0.3.1"}`
			},
			want: "run `" + scriptCmd + "A2A_CLI_ONLY=1 sh`, then remove /usr/local/bin/a2a or put ~/.local/bin before /usr/local/bin on PATH",
		},
		{
			name: "writable dir",
			setup: func(m *fakeMachine) {
				m.writable = map[string]bool{"/usr/local/bin": true}
				m.paths["a2a"] = "/usr/local/bin/a2a"
				m.outputs["/usr/local/bin/a2a version -o json"] = `{"version":"0.3.1"}`
			},
			want: scriptCmd + "INSTALL_DIR=/usr/local/bin A2A_CLI_ONLY=1 sh",
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

func TestDirWritable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{name: "writable", dir: dir, want: true},
		{name: "missing", dir: dir + "/missing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := dirWritable(tc.dir); got != tc.want {
				t.Fatalf("dirWritable(%q) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("dirWritable() left %d files in %s", len(entries), dir)
	}
}
