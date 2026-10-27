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

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/a2aproject/a2a-serve/internal/relayapi"
)

func credentialGet(t *testing.T, creds *credstore.Store, stdin string) (credentialResponse, error) {
	t.Helper()
	var out bytes.Buffer
	cfg := &globalConfig{
		Printer:     output.NewPrinter(&out, output.ModeText),
		credentials: creds,
	}
	root := newRootCmd(cfg)
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&out)
	root.SetArgs([]string{pluginName, "credential", "get"})
	if err := root.Execute(); err != nil {
		return credentialResponse{}, err
	}
	var resp credentialResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out.String(), err)
	}
	return resp, nil
}

func TestCredentialGet(t *testing.T) {
	t.Parallel()
	nodeToken := relayapi.NewNodeToken(relayapi.APIBase("https://node-host.example.com"), "grok", "secret")
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	for _, c := range []credstore.Credential{
		{URL: "https://agent.example.com", Token: "a2p_agent"},
		{URL: "https://agent.example.com/n/grok", Token: "a2p_grok"},
		{URL: "https://node-host.example.com/n/grok", Kind: credstore.KindNode, Token: nodeToken},
	} {
		if err := creds.Put(c); err != nil {
			t.Fatalf("creds.Put() error = %v", err)
		}
	}
	tests := []struct {
		name    string
		stdin   string
		want    credentialResponse
		wantErr bool
	}{
		{
			name:  "saved token",
			stdin: `{"uri": "https://agent.example.com"}`,
			want:  credentialResponse{Headers: map[string][]string{"Authorization": {"Bearer a2p_agent"}}},
		},
		{
			name:  "longest matching URL",
			stdin: `{"uri": "https://agent.example.com/n/grok/.well-known/agent-card.json"}`,
			want:  credentialResponse{Headers: map[string][]string{"Authorization": {"Bearer a2p_grok"}}},
		},
		{
			name:  "saved node token for its agent",
			stdin: `{"uri": "https://node-host.example.com"}`,
			want:  credentialResponse{Headers: map[string][]string{"Authorization": {"Bearer " + nodeToken}}},
		},
		{name: "unknown agent", stdin: `{"uri": "https://other.example.com"}`, want: credentialResponse{}},
		{name: "no uri", stdin: `{}`, wantErr: true},
		{name: "not json", stdin: `https://agent.example.com`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := credentialGet(t, creds, tc.stdin)
			if (err != nil) != tc.wantErr {
				t.Fatalf("a2a serve credential get error = %v, want error %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("a2a serve credential get wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

// TestCredentialGetNodeTokenFromEnv can't run in parallel because of t.Setenv.
func TestCredentialGetNodeTokenFromEnv(t *testing.T) {
	nodeToken := relayapi.NewNodeToken(relayapi.APIBase("https://agent.example.com"), "grok", "secret")
	t.Setenv(relayapi.EnvNodeToken, nodeToken)
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	got, err := credentialGet(t, creds, `{"uri": "https://agent.example.com"}`)
	if err != nil {
		t.Fatalf("a2a serve credential get error = %v", err)
	}
	want := credentialResponse{Headers: map[string][]string{"Authorization": {"Bearer " + nodeToken}}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("a2a serve credential get wrong result (-want +got) diff = %s", diff)
	}
}
