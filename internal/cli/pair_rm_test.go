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

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestPairRmEmail(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Port: 4100})
	st, err := store.Open(d.DBPath())
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	for tok, email := range map[string]string{"a2p_laptop": "anna@example.com", "a2p_phone": "anna@example.com", "a2p_bob": "bob@example.com"} {
		if err := st.PutToken(context.Background(), tok, store.Token{Email: email, URL: "https://coder.example.com"}, ""); err != nil {
			t.Fatalf("PutToken(%s) error = %v", tok, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	out, err := runCMD(t, "pair", "rm", "--email", "Anna@Example.com", "--dir", d.Root)
	if err != nil {
		t.Fatalf("a2a-serve pair rm --email error = %v", err)
	}
	if want := "✓ revoked 2 Google sign-in token(s) of anna@example.com\n"; !strings.HasPrefix(out, want) {
		t.Fatalf("a2a-serve pair rm --email = %q, want it to start with %q", out, want)
	}

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "email without sign-ins", args: []string{"pair", "rm", "--email", "anna@example.com"}, wantErr: "no Google sign-in of anna@example.com"},
		{name: "not an email", args: []string{"pair", "rm", "--email", "anna"}, wantErr: `"anna" is not an email address`},
		{name: "email with a name", args: []string{"pair", "rm", "anna", "--email", "anna@example.com"}, wantErr: "--email takes no pairing name or agent URL"},
		{name: "no name", args: []string{"pair", "rm"}, wantErr: "pair rm: missing <name|agent-url>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCMD(t, append(tc.args, "--dir", d.Root)...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("a2a-serve %s error = %v, want %q", strings.Join(tc.args, " "), err, tc.wantErr)
			}
			if got := clierr.Classify(err); got == nil || got.Code != clierr.CodeUsage {
				t.Fatalf("a2a-serve %s error class = %+v, want %s", strings.Join(tc.args, " "), got, clierr.CodeUsage)
			}
		})
	}
}

func TestPairRmDryRun(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Port: 4100})
	st, err := store.Open(d.DBPath())
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	ctx := context.Background()
	if _, err := st.EnsurePairing(ctx, store.Pairing{Name: "anna", Kind: "person", Role: "authenticated"}); err != nil {
		t.Fatalf("EnsurePairing() error = %v", err)
	}
	if err := st.PutToken(ctx, "a2p_laptop", store.Token{Pairing: "anna", URL: "https://coder.example.com"}, "person"); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	if err := st.PutLink(ctx, store.Link{Messenger: "telegram", Identity: "telegram:1", Pairing: "anna"}, "person"); err != nil {
		t.Fatalf("PutLink() error = %v", err)
	}
	if err := st.AddSchedule(ctx, store.Schedule{Name: "daily", Cron: "@daily", Prompt: "p", UserID: "pair:anna"}); err != nil {
		t.Fatalf("AddSchedule() error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	home := d.UserHomeDir("pair:anna")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	out, err := runCMD(t, "pair", "rm", "anna", "--dry-run", "--dir", d.Root)
	if err != nil {
		t.Fatalf("a2a-serve pair rm --dry-run error = %v", err)
	}

	want := "would remove the pairing anna:\n" +
		"  1 token\n" +
		"  1 linked chat account: telegram telegram:1\n" +
		"  1 schedule that runs as it\n" +
		"  the agent home directory " + home + "\n" +
		"(dry run: nothing removed)\n"
	if !strings.HasPrefix(out, want) {
		t.Fatalf("a2a-serve pair rm --dry-run = %q, want it to start with %q", out, want)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("Stat() of the agent home after pair rm --dry-run error = %v, want it kept", err)
	}
	if got, err := runCMD(t, "pair", "get", "anna", "--dir", d.Root); err != nil || !strings.Contains(got, "anna") {
		t.Fatalf("a2a-serve pair get anna after pair rm --dry-run = %q, %v, want the pairing kept", got, err)
	}
	if _, err := runCMD(t, "pair", "rm", "anna", "--dry-run", "--email", "anna@example.com", "--dir", d.Root); !isUsage(err) {
		t.Fatalf("a2a-serve pair rm --dry-run --email error = %v, want a usage error", err)
	}
}

func TestPairRmDryRunOfJoinedAgent(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := creds.Put(credstore.Credential{URL: "https://coder.example.com", Token: "a2p_x"}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	out, err := runCMDWithCreds(t, creds, "pair", "rm", "https://coder.example.com", "--dry-run")
	if err != nil {
		t.Fatalf("a2a-serve pair rm <url> --dry-run error = %v", err)
	}

	if want := "would delete the token you saved for https://coder.example.com\n(dry run: nothing removed)\n"; out != want {
		t.Fatalf("a2a-serve pair rm <url> --dry-run = %q, want %q", out, want)
	}
	if ok, err := creds.HasJoined("https://coder.example.com"); err != nil || !ok {
		t.Fatalf("creds.HasJoined() after pair rm --dry-run = %v, %v, want the token kept", ok, err)
	}
	if _, err := runCMDWithCreds(t, creds, "pair", "rm", "https://other.example.com", "--dry-run"); err == nil {
		t.Fatalf("a2a-serve pair rm <unknown url> --dry-run error = nil, want an error that it isn't joined")
	}
}

func TestPairRmOfLocalServerURLKeepsItsOwnerToken(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	local := credstore.Credential{URL: "http://127.0.0.1:4100", Token: "a2p_owner", Agent: "proj", Dir: t.TempDir()}
	if err := creds.Put(local); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	for _, args := range [][]string{{"pair", "rm", local.URL}, {"pair", "rm", local.URL, "--dry-run"}} {
		if _, err := runCMDWithCreds(t, creds, args...); err == nil || !isUsage(err) {
			t.Fatalf("a2a-serve %s error = %v, want a usage error that it isn't joined", strings.Join(args, " "), err)
		}
	}

	got, err := creds.List()
	if err != nil {
		t.Fatalf("creds.List() error = %v", err)
	}
	if diff := cmp.Diff([]credstore.Credential{local}, got, cmpopts.IgnoreFields(credstore.Credential{}, "Created")); diff != "" {
		t.Fatalf("creds.List() after pair rm wrong result (-want +got) diff = %s", diff)
	}
}
