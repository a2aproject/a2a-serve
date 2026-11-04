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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/auth"
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
			path := filepath.Join(t.TempDir(), "dir", "AGENTS.md")
			if tc.existing != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeSection(path, "new\n"); err != nil {
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

			_, got := ts.srv.peers.withAgentsLine(ctx, execCtx)

			if hasLine := strings.Contains(a2atext.Message(got.Message), agentsLinePrefix); hasLine != tc.wantLine {
				t.Fatalf("withAgentsLine() message = %q, want the agents line %v", a2atext.Message(got.Message), tc.wantLine)
			}
		})
	}
}
