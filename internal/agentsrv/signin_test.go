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
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func TestSignOutRevokesGoogleSignIns(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{configure: func(c *agentdir.Config) { c.Policy.AllowEmails = []string{"anna@example.com"} }})
	ctx := context.Background()
	if err := ts.srv.store.PutToken(ctx, "a2p_google", store.Token{Email: "anna@example.com", URL: ts.localURL()}, ""); err != nil {
		t.Fatalf("PutToken() error = %v", err)
	}
	header := http.Header{"Authorization": {"Bearer a2p_google"}, "Content-Type": {"application/json"}}
	if got := statusOf(t, http.MethodPost, ts.localURL()+"/", listTasksRPC, header); got != http.StatusOK {
		t.Fatalf("POST with a Google sign-in token status = %d, want %d", got, http.StatusOK)
	}

	resp, err := controlapi.NewClient(ts.dir).SignOut(ctx, "anna@example.com")
	if err != nil {
		t.Fatalf("SignOut() error = %v", err)
	}

	if resp.Email != "anna@example.com" || resp.Tokens != 1 {
		t.Fatalf("SignOut() = %+v, want one token of anna@example.com", resp)
	}
	if got := statusOf(t, http.MethodPost, ts.localURL()+"/", listTasksRPC, header); got != http.StatusUnauthorized {
		t.Fatalf("POST with a revoked Google sign-in token status = %d, want %d", got, http.StatusUnauthorized)
	}
}

func TestServerPrunesSignInsOfRemovedEmails(t *testing.T) {
	t.Parallel()
	d, err := os.MkdirTemp("", "a2a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(d); err != nil {
			t.Errorf("removing %s: %v", d, err)
		}
	})
	dir, err := agentdir.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	seedDB(t, dir.DBPath())
	st, err := store.Open(dir.DBPath())
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	for tok, email := range map[string]string{"a2p_anna": "anna@example.com", "a2p_bob": "bob@example.com"} {
		if err := st.PutToken(context.Background(), tok, store.Token{Email: email, URL: "https://coder.example.com"}, ""); err != nil {
			t.Fatalf("PutToken(%s) error = %v", tok, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	ts := startServer(t, serverOpts{dir: d, configure: func(c *agentdir.Config) { c.Policy.AllowEmails = []string{"bob@example.com"} }})

	want := []string{"bob@example.com"}
	var got []string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		toks, err := ts.srv.store.Tokens(context.Background())
		if err != nil {
			t.Fatalf("Tokens() error = %v", err)
		}
		got = nil
		for _, tk := range toks {
			if tk.Email != "" {
				got = append(got, tk.Email)
			}
		}
		slices.Sort(got)
		if slices.Equal(got, want) {
			return
		}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Google sign-ins after the server pruned them wrong result (-want +got) diff = %s", diff)
	}
}
