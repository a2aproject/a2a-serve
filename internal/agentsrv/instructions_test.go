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

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentproxy"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/cardinfo"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/network"
)

func TestWriteSection(t *testing.T) {
	t.Parallel()
	section := sectionBegin + "\nnew\n" + sectionEnd + "\n"
	tests := []struct {
		name     string
		existing string
		want     string
	}{
		{name: "new file", want: section},
		{name: "keeps the user's text", existing: "# Mine\n", want: "# Mine\n\n" + section},
		{name: "replaces the section", existing: "# Mine\n\n" + sectionBegin + "\nold\n" + sectionEnd + "\n\n# After\n", want: "# Mine\n\n" + section + "\n# After\n"},
		{name: "section without an end", existing: sectionBegin + "\nold\n", want: section},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			path := filepath.Join(home, "dir", "AGENTS.md")
			if tc.existing != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeSection(home, filepath.Join("dir", "AGENTS.md"), "new\n"); err != nil {
				t.Fatalf("writeSection() error = %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, string(got)); diff != "" {
				t.Fatalf("writeSection() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestHomeWritesRefuseSymlinksOutOfHome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		link      string
		targetDir bool
		write     func(home string) error
	}{
		{
			name:  "section through a linked file",
			link:  filepath.Join(".claude", "CLAUDE.md"),
			write: func(home string) error { return writeSection(home, filepath.Join(".claude", "CLAUDE.md"), "new\n") },
		},
		{
			name:      "section through a linked folder",
			link:      ".claude",
			targetDir: true,
			write:     func(home string) error { return writeSection(home, filepath.Join(".claude", "CLAUDE.md"), "new\n") },
		},
		{
			name:      "credential helper through a linked folder",
			link:      ".local",
			targetDir: true,
			write: func(home string) error {
				_, err := writeHarnessHelper(home, "host.docker.internal", "k3y")
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, outside := t.TempDir(), t.TempDir()
			target := filepath.Join(outside, "target")
			if err := os.MkdirAll(filepath.Join(home, filepath.Dir(tc.link)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(home, tc.link)); err != nil {
				t.Fatal(err)
			}
			if tc.targetDir {
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := tc.write(home); err == nil {
				t.Fatalf("write through %s error = nil, want an error", tc.link)
			}
			if diff := cmp.Diff([]string{}, filesUnder(t, outside)); diff != "" {
				t.Fatalf("write through %s wrote outside the home (-want +got) diff = %s", tc.link, diff)
			}
		})
	}
}

// filesUnder lists the regular files under dir.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	files := []string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestWithAgentsLine(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	ts.pair("anna", auth.RoleAuthenticated)
	tests := []struct {
		name     string
		caller   *auth.Principal
		wantLine bool
	}{
		{name: "owner", caller: &auth.Principal{ID: auth.OwnerID, Role: auth.RoleOwner}, wantLine: true},
		{name: "authenticated", caller: &auth.Principal{ID: "pair:anna", Role: auth.RoleAuthenticated}, wantLine: true},
		{name: "anonymous", caller: &auth.Principal{ID: "telegram:9", Role: auth.RoleAnonymous}},
		{name: "no caller"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tc.caller != nil {
				ctx = auth.WithPrincipal(ctx, *tc.caller)
			}
			execCtx := &a2asrv.ExecutorContext{ContextID: "c1", Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}

			_, got := ts.srv.claims.withAgentsLine(ctx, execCtx)

			if hasLine := strings.Contains(a2atext.Message(got.Message), agentsLinePrefix); hasLine != tc.wantLine {
				t.Fatalf("withAgentsLine() message = %q, want the agents line %v", a2atext.Message(got.Message), tc.wantLine)
			}
		})
	}
}

func TestWriteInstructionsSkipsHomeThatFailsTheSandboxCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		replace   bool
		wantWrite bool
	}{
		{name: "unchanged", wantWrite: true},
		{name: "agent directory replaced and its home a symlink", replace: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, err := agentdir.Open(filepath.Join(t.TempDir(), "proj"))
			if err != nil {
				t.Fatal(err)
			}
			if err := dir.Init(); err != nil {
				t.Fatal(err)
			}
			pins, err := harness.NewPins(dir.Root, dir.Path(), dir.StateDir(), dir.HomeDir())
			if err != nil {
				t.Fatalf("harness.NewPins() error = %v", err)
			}
			cfg := &agentdir.Config{Name: "coder", Harness: "fake", Command: []string{"fake-acp"}, Workspace: "..", Sandbox: harness.SandboxDocker}
			s := &Server{dir: dir, pins: pins, rt: agentdir.NewRuntime(dir, cfg, agentdir.Secrets{}), store: openTestStore(t)}
			home := dir.HomeDir()
			if tc.replace {
				home = t.TempDir()
				if err := os.Rename(dir.Root, dir.Root+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(dir.StateDir(), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(home, dir.HomeDir()); err != nil {
					t.Fatal(err)
				}
			}

			if err := s.writeInstructions(context.Background()); err != nil {
				t.Fatalf("Server.writeInstructions() error = %v", err)
			}

			_, err = os.Stat(filepath.Join(home, instructionFiles[0]))
			if gotWrite := err == nil; gotWrite != tc.wantWrite {
				t.Fatalf("Server.writeInstructions() wrote into %s = %v, want %v (stat error = %v)", home, gotWrite, tc.wantWrite, err)
			}
		})
	}
}

func TestWriteInstructionsMentionsPeers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		outgoing  string
		wantPeers bool
	}{
		{name: "everyone on the private network", outgoing: agentdir.AccessEveryone, wantPeers: true},
		{name: "contacts only", outgoing: agentdir.AccessContacts},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, err := agentdir.Open(filepath.Join(t.TempDir(), "proj"))
			if err != nil {
				t.Fatal(err)
			}
			if err := dir.Init(); err != nil {
				t.Fatal(err)
			}
			pins, err := harness.NewPins(dir.Root, dir.Path(), dir.StateDir(), dir.HomeDir())
			if err != nil {
				t.Fatalf("harness.NewPins() error = %v", err)
			}
			cfg := &agentdir.Config{
				Name: "coder", Harness: "fake", Command: []string{"fake-acp"}, Workspace: "..", Sandbox: harness.SandboxDocker,
				Policy: agentdir.PolicyConfig{PrivateAllowOutgoing: tc.outgoing},
			}
			st := openTestStore(t)
			rt := agentdir.NewRuntime(dir, cfg, agentdir.Secrets{})
			clients := agentproxy.NewContactClients(agentproxy.ContactClientsDeps{Store: st, Private: struct{ network.Provider }{}, Config: rt.Config})
			s := &Server{dir: dir, pins: pins, rt: rt, store: st, clients: clients, peerCards: newPeerCards()}
			s.peerCards.replace([]network.PeerInfo{{Name: "ben", Card: cardinfo.Summary{Description: "Reviews Go."}}})

			if err := s.writeInstructions(context.Background()); err != nil {
				t.Fatalf("Server.writeInstructions() error = %v", err)
			}

			data, err := os.ReadFile(filepath.Join(dir.HomeDir(), instructionFiles[0]))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range []string{"- other agents on the private network (by name)", `- ben (private network), says of itself: "Reviews Go."`} {
				if got := strings.Contains(string(data), line); got != tc.wantPeers {
					t.Fatalf("Server.writeInstructions() lists %q = %v, want %v:\n%s", line, got, tc.wantPeers, data)
				}
			}
		})
	}
}

func TestAgentRefString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ref  AgentRef
		want string
	}{
		{name: "name only", ref: AgentRef{Name: "ben", Kind: "contact"}, want: "ben (contact)"},
		{name: "description", ref: AgentRef{Name: "grok", Kind: "webhook node", Description: "answers questions"}, want: "grok (webhook node): answers questions"},
		{
			name: "card",
			ref: AgentRef{Name: "coder", Kind: "contact", Card: cardinfo.Summary{
				Description: "Writes Go.",
				Skills:      []cardinfo.Skill{{ID: "release-notes", Name: "Release notes", Description: "Writes release notes."}, {ID: "review"}},
			}},
			want: `coder (contact), says of itself: "Writes Go." Skills: release-notes (Release notes) "Writes release notes."; review`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.ref.String(); got != tc.want {
				t.Fatalf("AgentRef.String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteInstructionsSkipsFilesItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "FIFO",
			setup: func(t *testing.T, path string) {
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "over 1 MiB",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20+1), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, err := agentdir.Open(filepath.Join(t.TempDir(), "proj"))
			if err != nil {
				t.Fatal(err)
			}
			if err := dir.Init(); err != nil {
				t.Fatal(err)
			}
			pins, err := harness.NewPins(dir.Root, dir.Path(), dir.StateDir(), dir.HomeDir())
			if err != nil {
				t.Fatalf("harness.NewPins() error = %v", err)
			}
			cfg := &agentdir.Config{Name: "coder", Harness: "fake", Command: []string{"fake-acp"}, Workspace: "..", Sandbox: harness.SandboxDocker}
			s := &Server{dir: dir, pins: pins, rt: agentdir.NewRuntime(dir, cfg, agentdir.Secrets{}), store: openTestStore(t)}
			skipped := filepath.Join(dir.HomeDir(), instructionFiles[1])
			if err := os.MkdirAll(filepath.Dir(skipped), 0o700); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, skipped)
			before, err := os.Stat(skipped)
			if err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() { done <- s.writeInstructions(context.Background()) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Server.writeInstructions() error = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Server.writeInstructions() blocked")
			}

			after, err := os.Stat(skipped)
			if err != nil {
				t.Fatal(err)
			}
			if after.Mode() != before.Mode() || after.Size() != before.Size() {
				t.Fatalf("Server.writeInstructions() changed %s to %v of %d bytes, want it skipped", skipped, after.Mode(), after.Size())
			}
			if _, err := os.Stat(filepath.Join(dir.HomeDir(), instructionFiles[0])); err != nil {
				t.Fatalf("Server.writeInstructions() didn't write %s: %v", instructionFiles[0], err)
			}
		})
	}
}
