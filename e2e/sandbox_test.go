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
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/harness"
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

func TestDockerSandboxMasksAgentConfig(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	owner := newUser(t)
	coder := owner.startAgent("coder", "--deny-tools", "execute")

	for _, name := range []string{"opencode.json", filepath.Join(".claude", "settings.json")} {
		path := filepath.Join(coder.dir, name)
		if got := resultOf(owner.mustSend(coder.url, "write "+path)).Text; !strings.HasPrefix(got, "write error: ") {
			t.Fatalf("a2a send write %s = %q, want a write error", name, got)
		}
	}
	scratch := filepath.Join(coder.dir, ".opencode", "opencode.json")
	owner.mustSend(coder.url, "write "+scratch)
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat(%s) after the agent wrote it error = %v, want it to stay in the container", scratch, err)
	}
	writeFile(t, filepath.Join(coder.dir, "opencode.json"), `{"permission": {"bash": "allow"}}`)
	coder.serve("restart")

	if got, want := resultOf(owner.mustSend(coder.url, "whoami")).Text, "whoami denied."; got != want {
		t.Fatalf("a2a send whoami with opencode.json allowing bash = %q, want %q", got, want)
	}
}

func TestDockerSandboxLocksMemberHomeConfig(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	owner, anna := newUser(t), newUser(t)
	coder := owner.startAgent("coder", "--deny-tools", "execute")
	anna.pairJoin(coder.url, coder.invite("anna"))
	home := harness.ContainerHome

	for _, name := range []string{
		filepath.Join(".claude", "settings.json"),
		filepath.Join(".config", "opencode", "opencode.json"),
		filepath.Join(".local", "share", "opencode", "auth.json"),
	} {
		path := filepath.Join(home, name)
		if got := resultOf(anna.mustSend(coder.url, "write "+path)).Text; !strings.HasPrefix(got, "write error: ") {
			t.Fatalf("a2a send write %s as a member = %q, want a write error", name, got)
		}
	}
	if got := resultOf(anna.mustSend(coder.url, "rename "+filepath.Join(home, ".config")+" "+filepath.Join(home, ".config-old"))).Text; !strings.HasPrefix(got, "rename error: ") {
		t.Fatalf("a2a send rename .config as a member = %q, want a rename error", got)
	}
	if got, want := resultOf(anna.mustSend(coder.url, "write "+filepath.Join(home, "notes.txt"))).Text, "write ok"; got != want {
		t.Fatalf("a2a send write notes.txt as a member = %q, want %q", got, want)
	}
	if got, want := resultOf(owner.mustSend(coder.url, "write "+filepath.Join(home, ".claude", "settings.json"))).Text, "write ok"; got != want {
		t.Fatalf("a2a send write .claude/settings.json as the owner = %q, want %q", got, want)
	}
}

// TestStartWhenDockerDoesNotAnswer covers start --detach with a docker
// endpoint that accepts connections and never answers: start fails within
// seconds and names DOCKER_HOST.
func TestStartWhenDockerDoesNotAnswer(t *testing.T) {
	t.Parallel()
	dependsOnSandbox(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("needs the docker CLI")
	}
	owner := newUser(t)
	sock := filepath.Join(owner.home, "hang.sock")
	hangingListener(t, sock)
	owner.env = append(owner.env, "DOCKER_HOST=unix://"+sock)
	coder := owner.newAgent("coder")
	t.Cleanup(coder.cleanup)

	start := time.Now()
	out, err := owner.exec(coder.dir, "a2a", "serve", "start", "acp", "--name", "coder", "--detach",
		"--sandbox", "docker", "--image", "example.invalid/fakeacp", "--command", "/usr/local/bin/fakeacp")
	coder.stopped = err != nil
	want := "docker does not answer within 10s at unix://" + sock + " from DOCKER_HOST"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("a2a-serve start --detach = %q, %v, want an error containing %q", out, err, want)
	}
	if elapsed := time.Since(start); elapsed > 45*time.Second {
		t.Fatalf("a2a-serve start --detach failed after %v, want it to fail within seconds", elapsed)
	}
}

// hangingListener listens on the Unix socket path, and accepts connections
// without ever answering them.
func hangingListener(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("closing the listener: %v", err)
		}
		<-done
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			if err := c.Close(); err != nil {
				t.Errorf("closing a connection: %v", err)
			}
		}
	})
}
