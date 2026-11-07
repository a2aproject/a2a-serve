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

package credstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
)

func TestLookup(t *testing.T) {
	t.Parallel()
	s := &Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	for _, c := range []Credential{
		{URL: "https://assistant.tail1234.ts.net/", Token: "a2p_root"},
		{URL: "https://assistant.tail1234.ts.net/n/grok", Token: "a2p_node"},
	} {
		if err := s.Put(c); err != nil {
			t.Fatalf("Put() error = %v", err)
		}
	}
	tests := []struct {
		target string
		want   string
	}{
		{target: "https://assistant.tail1234.ts.net", want: "a2p_root"},
		{target: "https://Assistant.tail1234.ts.net/.well-known/agent-card.json", want: "a2p_root"},
		{target: "https://assistant.tail1234.ts.net/n/grok/.well-known/agent-card.json", want: "a2p_node"},
		{target: "https://assistant.tail1234.ts.net/n/grokx", want: "a2p_root"},
		{target: "https://assistant.tail1234.ts.net/n/grok/../other", want: "a2p_root"},
		{target: "https://assistant.tail1234.ts.net/n/grok/%2e%2e/other", want: "a2p_root"},
		{target: "https://assistant.tail1234.ts.net/n/other/../grok/", want: "a2p_node"},
		{target: "https://assistant.tail1234.ts.net/n/./grok", want: "a2p_node"},
		{target: "https://other.example.com", want: ""},
	}
	for _, tc := range tests {
		got, _, err := s.Lookup(tc.target)
		if err != nil {
			t.Fatalf("Lookup() error = %v", err)
		}
		if got.Token != tc.want {
			t.Errorf("Lookup(%q) = %q, want %q", tc.target, got.Token, tc.want)
		}
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode = %v, want 0600", info.Mode().Perm())
	}
	if ok, err := s.Delete("https://assistant.tail1234.ts.net/n/grok/"); err != nil || !ok {
		t.Fatalf("Delete() = %v, %v, want true, nil", ok, err)
	}
}

func TestLookupNodeToken(t *testing.T) {
	t.Parallel()
	nodeToken := mailboxapi.NewNodeToken("https://assistant.example.com/mailbox", "grok", "secret")
	node := Credential{URL: "https://assistant.example.com/n/grok", Kind: KindNode, Token: nodeToken}
	caller := Credential{URL: "https://assistant.example.com", Token: "a2p_caller"}
	tests := []struct {
		name   string
		stored []Credential
		target string
		want   string
	}{
		{name: "agent hosting the node", stored: []Credential{node}, target: "https://assistant.example.com", want: nodeToken},
		{name: "card of the agent hosting the node", stored: []Credential{node}, target: "https://assistant.example.com/.well-known/agent-card.json", want: nodeToken},
		{name: "caller token wins", stored: []Credential{node, caller}, target: "https://assistant.example.com", want: "a2p_caller"},
		{name: "other agent", stored: []Credential{node}, target: "https://other.example.com", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
			for _, c := range tc.stored {
				if err := s.Put(c); err != nil {
					t.Fatalf("Put() error = %v", err)
				}
			}
			got, _, err := s.Lookup(tc.target)
			if err != nil {
				t.Fatalf("Lookup() error = %v", err)
			}
			if got.Token != tc.want {
				t.Fatalf("Lookup(%q) = %q, want %q", tc.target, got.Token, tc.want)
			}
		})
	}
}

func TestNodeCredential(t *testing.T) {
	t.Parallel()
	nodeToken := mailboxapi.NewNodeToken("https://assistant.example.com/mailbox", "grok", "secret")
	tests := []struct {
		name   string
		token  string
		target string
		want   Credential
		wantOK bool
	}{
		{
			name:   "agent hosting the node",
			token:  nodeToken,
			target: "https://assistant.example.com/",
			want:   Credential{URL: "https://assistant.example.com", Kind: KindNode, Token: nodeToken},
			wantOK: true,
		},
		{name: "other agent", token: nodeToken, target: "https://other.example.com"},
		{name: "not a node token", token: "a2p_caller", target: "https://assistant.example.com"},
		{name: "no token", target: "https://assistant.example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := NodeCredential(tc.token, tc.target)
			if ok != tc.wantOK {
				t.Fatalf("NodeCredential() ok = %v, want %v", ok, tc.wantOK)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("NodeCredential() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestPair(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || r.URL.Path != "/pair" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if req["code"] != "K7Q2-9XF4" {
			w.WriteHeader(http.StatusForbidden)
			if err := json.NewEncoder(w).Encode(map[string]string{"error": "invalid or expired pairing code"}); err != nil {
				t.Error(err)
			}
			return
		}
		if err := json.NewEncoder(w).Encode(pairapi.Response{Token: "a2p_x", Principal: "pair:grok", Agent: "assistant"}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c, err := Pair(ctx, srv.Client(), srv.URL+"/", "K7Q2-9XF4", pairapi.PairingPerson)
	if err != nil {
		t.Fatalf("Pair() error = %v", err)
	}
	if c.Token != "a2p_x" || c.Agent != "assistant" || c.URL != srv.URL {
		t.Fatalf("Pair() = %+v, want token a2p_x for %s", c, srv.URL)
	}
	if _, err := Pair(ctx, srv.Client(), srv.URL, "WRONG", pairapi.PairingPerson); err == nil {
		t.Fatalf("Pair() with a wrong code error = nil, want error")
	}
}

func TestPairNode(t *testing.T) {
	t.Parallel()
	var token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := pairapi.Response{Kind: KindNode, Token: token, Principal: "node:grok", Agent: "Grok", URL: "http://" + r.Host + "/n/grok"}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	token = mailboxapi.NewNodeToken(mailboxapi.APIBase(srv.URL), "grok", "secret")
	got, err := Pair(context.Background(), srv.Client(), srv.URL, "K7Q2-9XF4", pairapi.PairingPerson)
	if err != nil {
		t.Fatalf("Pair() error = %v", err)
	}
	want := Credential{URL: srv.URL + "/n/grok", Kind: KindNode, Token: token, Agent: "Grok", Principal: "node:grok", Created: got.Created}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Pair() wrong result (-want +got) diff = %s", diff)
	}
}

func TestPairRefusesTokensForOtherURLs(t *testing.T) {
	t.Parallel()
	nodeToken := func(agent, id string) string { return mailboxapi.NewNodeToken(mailboxapi.APIBase(agent), id, "secret") }
	tests := []struct {
		name string
		// resp builds the response of the agent at base.
		resp    func(base string) pairapi.Response
		local   bool
		wantErr bool
	}{
		{name: "caller token", resp: func(string) pairapi.Response {
			return pairapi.Response{Kind: pairapi.KindCaller, Token: "a2p_x", URL: "https://elsewhere.example.com"}
		}},
		{name: "node of the agent", resp: func(base string) pairapi.Response {
			return pairapi.Response{Kind: KindNode, Token: nodeToken(base, "grok"), URL: base + "/n/grok"}
		}},
		{name: "node of the agent at another loopback name", local: true, resp: func(base string) pairapi.Response {
			return pairapi.Response{Kind: KindNode, Token: nodeToken(base, "grok"), URL: base + "/n/grok"}
		}},
		{name: "unknown kind", wantErr: true, resp: func(string) pairapi.Response {
			return pairapi.Response{Kind: "admin", Token: "a2p_x"}
		}},
		{name: "node URL of another agent", wantErr: true, resp: func(base string) pairapi.Response {
			return pairapi.Response{Kind: KindNode, Token: nodeToken(base, "grok"), URL: "https://other.example.com/n/grok"}
		}},
		{name: "node token of another agent", wantErr: true, resp: func(string) pairapi.Response {
			return pairapi.Response{Kind: KindNode, Token: nodeToken("https://other.example.com", "grok"), URL: "https://other.example.com/n/grok"}
		}},
		{name: "node URL of another node", wantErr: true, resp: func(base string) pairapi.Response {
			return pairapi.Response{Kind: KindNode, Token: nodeToken(base, "grok"), URL: base + "/n/bot"}
		}},
		{name: "malformed node token", wantErr: true, resp: func(base string) pairapi.Response {
			return pairapi.Response{Kind: KindNode, Token: "a2rn.x", URL: base + "/n/grok"}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var base string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewEncoder(w).Encode(tc.resp(base)); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()
			base = srv.URL
			agent := srv.URL
			if tc.local {
				agent = strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
			}
			_, err := Pair(context.Background(), srv.Client(), agent, "K7Q2-9XF4", pairapi.PairingPerson)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("Pair() error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestNodeTokensAreKeptApart(t *testing.T) {
	t.Parallel()
	s := &Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if _, err := s.Node(); !errors.Is(err, ErrNoNode) {
		t.Fatalf("Node() error = %v, want %v", err, ErrNoNode)
	}
	for _, c := range []Credential{
		{URL: "https://assistant.example.com/n/grok", Token: "a2p_caller"},
		{URL: "https://assistant.example.com/n/grok", Kind: KindNode, Token: "a2rn.grok"},
	} {
		if err := s.Put(c); err != nil {
			t.Fatalf("Put() error = %v", err)
		}
	}
	caller, _, err := s.Lookup("https://assistant.example.com/n/grok")
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if caller.Token != "a2p_caller" {
		t.Fatalf("Lookup() = %q, want %q", caller.Token, "a2p_caller")
	}
	node, err := s.Node()
	if err != nil {
		t.Fatalf("Node() error = %v", err)
	}
	if node.Token != "a2rn.grok" {
		t.Fatalf("Node() = %q, want %q", node.Token, "a2rn.grok")
	}
	if err := s.Put(Credential{URL: "https://other.example.com/n/bot", Kind: KindNode, Token: "a2rn.bot"}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if _, err := s.Node(); err == nil {
		t.Fatalf("Node() with two node tokens error = nil, want error")
	}
}

func TestJoinedCredentialsExcludeOwnerTokens(t *testing.T) {
	t.Parallel()
	const (
		local  = "http://127.0.0.1:9001"
		caller = "https://assistant.example.com"
		node   = "https://assistant.example.com/n/grok"
	)
	s := &Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	for _, c := range []Credential{
		{URL: local, Token: "a2p_owner", Dir: t.TempDir()},
		{URL: caller, Token: "a2p_caller"},
		{URL: node, Kind: KindNode, Token: "a2rn.grok"},
	} {
		if err := s.Put(c); err != nil {
			t.Fatalf("Put() error = %v", err)
		}
	}
	tokens := func(creds []Credential) []string {
		var out []string
		for _, c := range creds {
			out = append(out, c.Token)
		}
		return out
	}

	listed, err := s.ListJoined()
	if err != nil {
		t.Fatalf("ListJoined() error = %v", err)
	}
	if diff := cmp.Diff([]string{"a2p_caller", "a2rn.grok"}, tokens(listed)); diff != "" {
		t.Fatalf("ListJoined() wrong result (-want +got) diff = %s", diff)
	}
	for url, want := range map[string]bool{local: false, caller + "/": true, node: true, "https://other.example.com": false} {
		if got, err := s.HasJoined(url); err != nil || got != want {
			t.Fatalf("HasJoined(%q) = %v, %v, want %v, nil", url, got, err, want)
		}
	}
	if deleted, err := s.DeleteJoined(local); err != nil || deleted {
		t.Fatalf("DeleteJoined(%q) = %v, %v, want false, nil", local, deleted, err)
	}
	if deleted, err := s.DeleteJoined(caller + "/"); err != nil || !deleted {
		t.Fatalf("DeleteJoined(%q) = %v, %v, want true, nil", caller+"/", deleted, err)
	}

	left, err := s.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if diff := cmp.Diff([]string{"a2p_owner", "a2rn.grok"}, tokens(left)); diff != "" {
		t.Fatalf("List() after DeleteJoined() wrong result (-want +got) diff = %s", diff)
	}
}

func TestDeleteDir(t *testing.T) {
	t.Parallel()
	s := &Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	a, b := t.TempDir(), t.TempDir()
	for _, c := range []Credential{
		{URL: "http://127.0.0.1:9001", Token: "a2p_a", Agent: "proj", Dir: a},
		{URL: "http://127.0.0.1:9002", Token: "a2p_b", Agent: "proj", Dir: b},
		{URL: "https://example.com", Token: "a2p_joined"},
	} {
		if err := s.Put(c); err != nil {
			t.Fatalf("Put() error = %v", err)
		}
	}

	if err := s.DeleteDir(a); err != nil {
		t.Fatalf("DeleteDir() error = %v", err)
	}

	got, err := s.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var tokens []string
	for _, c := range got {
		tokens = append(tokens, c.Token)
	}
	if diff := cmp.Diff([]string{"a2p_b", "a2p_joined"}, tokens); diff != "" {
		t.Fatalf("List() after DeleteDir() wrong result (-want +got) diff = %s", diff)
	}
}

func TestEntriesOfRemovedAgentsAreDropped(t *testing.T) {
	t.Parallel()
	s := &Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	live := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	for _, c := range []Credential{
		{URL: "http://127.0.0.1:9001", Token: "a2p_gone", Dir: gone},
		{URL: "http://127.0.0.1:9002", Token: "a2p_live", Dir: live},
	} {
		if err := s.Put(c); err != nil {
			t.Fatalf("Put() error = %v", err)
		}
	}

	if c, ok, err := s.Lookup("http://127.0.0.1:9001"); err != nil || ok {
		t.Fatalf("Lookup() of a removed agent's URL = %+v, %v, %v, want no credential", c, ok, err)
	}
	if err := s.Prune(); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	var saved []Credential
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	var tokens []string
	for _, c := range saved {
		tokens = append(tokens, c.Token)
	}
	if diff := cmp.Diff([]string{"a2p_live"}, tokens); diff != "" {
		t.Fatalf("saved tokens after Prune() wrong result (-want +got) diff = %s", diff)
	}
}
