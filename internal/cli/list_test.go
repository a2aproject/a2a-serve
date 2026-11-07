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
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/credstore"
)

func TestPairListJoinedEmpty(t *testing.T) {
	t.Parallel()

	out, err := runCMD(t, "pair", "list", "--joined")
	if err != nil {
		t.Fatalf("a2a-serve pair list --joined error = %v", err)
	}

	if want := "no joined agents; join one with `a2a-serve pair join <agent-url> <code>`\n"; out != want {
		t.Fatalf("a2a-serve pair list --joined = %q, want %q", out, want)
	}
}

func TestPairListJoined(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := creds.Put(credstore.Credential{URL: "https://coder.example.com", Token: "tok", Agent: "coder", Principal: "pair:anna"}); err != nil {
		t.Fatalf("creds.Put() error = %v", err)
	}

	out, err := runCMDWithCreds(t, creds, "-o", "json", "pair", "list", "--joined")
	if err != nil {
		t.Fatalf("a2a-serve pair list --joined error = %v", err)
	}
	var got []joinedPairing
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
	want := []joinedPairing{{Side: sideJoined, joinedAgent: joinedAgent{URL: "https://coder.example.com", Kind: "caller", Agent: "coder", Principal: "pair:anna"}}}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(joinedPairing{}), cmpopts.IgnoreFields(joinedAgent{}, "Joined")); diff != "" {
		t.Fatalf("a2a-serve pair list --joined wrong result (-want +got) diff = %s", diff)
	}
	for _, u := range []string{"HTTPS://Coder.example.com/", "https://coder.example.com/.well-known/agent-card.json"} {
		if out, err := runCMDWithCreds(t, creds, "pair", "get", u); err != nil || !strings.Contains(out, "agent        coder") {
			t.Fatalf("a2a-serve pair get %s = %q, %v, want the agent joined at the same URL", u, out, err)
		}
	}
	if _, err := runCMDWithCreds(t, creds, "pair", "get", "https://other.example.com"); err == nil || !strings.Contains(err.Error(), "you haven't joined") {
		t.Fatalf("a2a-serve pair get of an agent not joined error = %v, want a hint to join", err)
	}
}

func TestPairListJoinedSkipsTokensOfLocalServers(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	for _, c := range []credstore.Credential{
		{URL: "http://127.0.0.1:4100", Token: "a2p_owner", Agent: "proj", Dir: t.TempDir()},
		{URL: "https://coder.example.com", Token: "tok", Agent: "coder"},
	} {
		if err := creds.Put(c); err != nil {
			t.Fatalf("creds.Put() error = %v", err)
		}
	}

	out, err := runCMDWithCreds(t, creds, "-o", "json", "pair", "list", "--joined")
	if err != nil {
		t.Fatalf("a2a-serve pair list --joined error = %v", err)
	}
	var got []joinedPairing
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
	var urls []string
	for _, j := range got {
		urls = append(urls, j.URL)
	}
	if diff := cmp.Diff([]string{"https://coder.example.com"}, urls); diff != "" {
		t.Fatalf("a2a-serve pair list --joined URLs wrong result (-want +got) diff = %s", diff)
	}
	if _, err := runCMDWithCreds(t, creds, "pair", "get", "http://127.0.0.1:4100"); err == nil || !strings.Contains(err.Error(), "you haven't joined") {
		t.Fatalf("a2a-serve pair get of a local server's URL error = %v, want a hint to join", err)
	}
}

func TestPairGetJoinedNode(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	node := credstore.Credential{URL: "https://coder.example.com/n/grok", Kind: credstore.KindNode, Token: "a2rn.x.grok.s", Agent: "grok", Principal: "node:grok"}
	if err := creds.Put(node); err != nil {
		t.Fatalf("creds.Put() error = %v", err)
	}

	out, err := runCMDWithCreds(t, creds, "-o", "json", "pair", "get", node.URL)
	if err != nil {
		t.Fatalf("a2a-serve pair get %s error = %v", node.URL, err)
	}
	var got joinedAgent
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
	want := joinedAgent{URL: node.URL, Kind: credstore.KindNode, Agent: "grok", Principal: "node:grok"}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(joinedAgent{}, "Joined")); diff != "" {
		t.Fatalf("a2a-serve pair get of a joined node wrong result (-want +got) diff = %s", diff)
	}
}

func TestPairListSides(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := creds.Put(credstore.Credential{URL: "https://reviewer.example.com", Token: "tok", Agent: "reviewer", Principal: "pair:coder"}); err != nil {
		t.Fatalf("creds.Put() error = %v", err)
	}
	type entry struct {
		Side string `json:"side"`
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	tests := []struct {
		name     string
		args     []string
		want     []entry
		wantText []string
	}{
		{
			name:     "both sides",
			want:     []entry{{Side: sideInvited, Name: "local"}, {Side: sideJoined, URL: "https://reviewer.example.com"}},
			wantText: []string{"Invited to this agent:\nNAME", "local", "\n\nJoined by you:\nURL", "https://reviewer.example.com"},
		},
		{name: "invited", args: []string{"--invited"}, want: []entry{{Side: sideInvited, Name: "local"}}, wantText: []string{"NAME"}},
		{name: "joined", args: []string{"--joined"}, want: []entry{{Side: sideJoined, URL: "https://reviewer.example.com"}}, wantText: []string{"URL"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Port: 4100})
			args := append([]string{"pair", "list", "--dir", d.Root}, tc.args...)
			out, err := runCMDWithCreds(t, creds, append([]string{"-o", "json"}, args...)...)
			if err != nil {
				t.Fatalf("a2a-serve pair list error = %v", err)
			}
			var got []entry
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("a2a-serve pair list -o json wrong result (-want +got) diff = %s", diff)
			}
			text, err := runCMDWithCreds(t, creds, args...)
			if err != nil {
				t.Fatalf("a2a-serve pair list error = %v", err)
			}
			for _, want := range tc.wantText {
				if !strings.Contains(text, want) {
					t.Fatalf("a2a-serve pair list = %q, want %q", text, want)
				}
			}
			if (len(tc.args) == 0) != strings.Contains(text, "Joined by you:") {
				t.Fatalf("a2a-serve pair list = %q, want section headings only without a filter", text)
			}
		})
	}
}

func TestPairListOutsideAgentDir(t *testing.T) {
	t.Parallel()
	if _, err := runCMD(t, "pair", "list", "--dir", t.TempDir()); err == nil {
		t.Fatal("a2a-serve pair list outside an agent directory error = nil, want an error")
	}
}
