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
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
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
		{name: "no name", args: []string{"pair", "rm"}, wantErr: "accepts 1 arg(s), received 0"},
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
