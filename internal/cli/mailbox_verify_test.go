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
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func TestMailboxVerify(t *testing.T) {
	t.Parallel()
	token := mailboxapi.NewNodeToken(mailboxapi.APIBase("https://agent.example.com"), "grok", "secret")
	other := mailboxapi.NewNodeToken(mailboxapi.APIBase("https://agent.example.com"), "grok", "other")
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := creds.Put(credstore.Credential{URL: "https://agent.example.com/n/grok", Kind: credstore.KindNode, Token: token}); err != nil {
		t.Fatalf("creds.Put() error = %v", err)
	}
	body := `{"id":"wake_1","kind":"task","node":"grok","taskId":"t1"}`
	sign := func(token string, at time.Time) string {
		return mailboxapi.Sign(mailboxapi.WakeKey(token), at, []byte(body))
	}
	tests := []struct {
		name      string
		args      []string
		body      string
		want      map[string]any
		wantError bool
	}{
		{
			name: "signed for the saved token",
			args: []string{"--signature", sign(token, time.Now())},
			body: body,
			want: map[string]any{"valid": true, "id": "wake_1", "kind": "task", "taskId": "t1"},
		},
		{
			name: "signed for the token flag",
			args: []string{"--signature", sign(other, time.Now()), "--token", other},
			body: body,
			want: map[string]any{"valid": true, "id": "wake_1", "kind": "task", "taskId": "t1"},
		},
		{name: "signed for another token", args: []string{"--signature", sign(other, time.Now())}, body: body, wantError: true},
		{name: "changed body", args: []string{"--signature", sign(token, time.Now())}, body: `{"id":"wake_2"}`, wantError: true},
		{name: "signed too long ago", args: []string{"--signature", sign(token, time.Now().Add(-time.Hour))}, body: body, wantError: true},
		{name: "no signature", body: body, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			cfg := &globalConfig{Printer: output.NewPrinter(&out, output.ModeText), credentials: creds}
			root := newRootCmd(cfg)
			root.SetIn(strings.NewReader(tc.body))
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(append([]string{"mailbox", "verify", "-o", "json"}, tc.args...))
			err := root.Execute()
			if tc.wantError {
				if err == nil {
					t.Fatalf("mailbox verify error = nil, want an error; output %s", out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("mailbox verify error = %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("json.Unmarshal(%q) error = %v", out.String(), err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("mailbox verify wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
