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

	"github.com/a2aproject/a2a-serve/internal/credstore"
)

func TestPairListJoinedEmpty(t *testing.T) {
	t.Parallel()

	out, err := runCMD(t, "pair", "list", "--joined")
	if err != nil {
		t.Fatalf("a2a serve pair list --joined error = %v", err)
	}

	if want := "no joined agents; join one with `a2a serve pair join <agent-url> <code>`\n"; out != want {
		t.Fatalf("a2a serve pair list --joined = %q, want %q", out, want)
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
		t.Fatalf("a2a serve pair list --joined error = %v", err)
	}
	var got []joinedAgent
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
	want := []joinedAgent{{URL: "https://coder.example.com", Kind: "caller", Agent: "coder", Principal: "pair:anna"}}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(joinedAgent{}, "Joined")); diff != "" {
		t.Fatalf("a2a serve pair list --joined wrong result (-want +got) diff = %s", diff)
	}
	if _, err := runCMDWithCreds(t, creds, "pair", "get", "https://other.example.com"); err == nil || !strings.Contains(err.Error(), "you haven't joined") {
		t.Fatalf("a2a serve pair get of an agent not joined error = %v, want a hint to join", err)
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
		t.Fatalf("a2a serve pair get %s error = %v", node.URL, err)
	}
	var got joinedAgent
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
	want := joinedAgent{URL: node.URL, Kind: credstore.KindNode, Agent: "grok", Principal: "node:grok"}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(joinedAgent{}, "Joined")); diff != "" {
		t.Fatalf("a2a serve pair get of a joined node wrong result (-want +got) diff = %s", diff)
	}
}
