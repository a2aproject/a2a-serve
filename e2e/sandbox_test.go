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

//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerSandboxMounts(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	owner := newUser(t)
	docs := filepath.Join(owner.home, "docs")
	if err := os.Mkdir(docs, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(docs, "notes.md"), nil, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	coder := owner.startAgent("coder", "--mount", docs+":/docs:ro")

	tests := []struct {
		name string
		dir  string
		want string
	}{
		{name: "extra mount", dir: "/docs", want: "ls: notes.md"},
		{name: "host path of the extra mount", dir: docs, want: "ls error: open " + docs + ": no such file or directory"},
	}
	for _, tc := range tests {
		if got := resultOf(owner.mustSend(coder.url, "ls "+tc.dir)).Text; got != tc.want {
			t.Fatalf("a2a send ls %s (%s) = %q, want %q", tc.dir, tc.name, got, tc.want)
		}
	}
	if got := resultOf(owner.mustSend(coder.url, "ls "+coder.dir)).Text; !strings.Contains(got, ".a2a") {
		t.Fatalf("a2a send ls of the workspace = %q, want it mounted at its host path", got)
	}
	// The hidden .a2a is an empty tmpfs, which rootful Docker also makes
	// unreadable for the container user.
	hidden := filepath.Join(coder.dir, ".a2a")
	if got := resultOf(owner.mustSend(coder.url, "ls "+hidden)).Text; got != "ls: " && got != "ls error: open "+hidden+": permission denied" {
		t.Fatalf("a2a send ls %s = %q, want it empty or unreadable", hidden, got)
	}
}
