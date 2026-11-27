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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
)

func TestMailboxPeers(t *testing.T) {
	t.Parallel()
	peers := mailboxapi.Peers{
		Peers: []mailboxapi.Peer{
			{
				Name:        "me",
				URL:         "https://relay.example/ingress/p1/a2a/me",
				Description: "Personal agent",
				Skills: []mailboxapi.PeerSkill{
					{ID: "mail-cousin", Name: "Mail cousin", Description: "Reaches Cousin Sam by email."},
				},
			},
			{
				Name:        "instinct",
				URL:         "https://relay.example/ingress/p1/a2a/me/n/instinct",
				Description: "Sister's Instinct",
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != mailboxapi.Prefix+"/api/peers" {
			t.Errorf("request = %s %s, want GET %s/api/peers", r.Method, r.URL.Path, mailboxapi.Prefix)
		}
		if err := json.NewEncoder(w).Encode(peers); err != nil {
			t.Errorf("encoding peers error = %v", err)
		}
	}))
	defer srv.Close()
	token := mailboxapi.NewNodeToken(srv.URL+mailboxapi.Prefix, "muse", "secret")

	out, err := runCMD(t, "mailbox", "peers", "--token", token)
	if err != nil {
		t.Fatalf("a2a-serve mailbox peers error = %v", err)
	}
	for _, want := range []string{"NAME", "URL", "DESCRIPTION", "SKILLS", "me", "instinct", "mail-cousin: Reaches Cousin Sam by email."} {
		if !strings.Contains(out, want) {
			t.Fatalf("a2a-serve mailbox peers output missing %q:\n%s", want, out)
		}
	}

	jsonOut, err := runCMD(t, "mailbox", "peers", "--token", token, "-o", "json")
	if err != nil {
		t.Fatalf("a2a-serve mailbox peers -o json error = %v", err)
	}
	var got []mailboxapi.Peer
	if err := json.Unmarshal([]byte(jsonOut), &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if diff := cmp.Diff(peers.Peers, got); diff != "" {
		t.Fatalf("a2a-serve mailbox peers -o json wrong result (-want +got) diff = %s", diff)
	}
}
