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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

func lookupOwnerToken(t *testing.T, creds *credstore.Store, url string) string {
	t.Helper()
	c, ok, err := creds.Lookup(url)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if !ok {
		t.Fatalf("Lookup(%q) found no token, want the owner token of this machine", url)
	}
	if c.Principal != auth.OwnerID {
		t.Fatalf("Lookup(%q) principal = %q, want %q", url, c.Principal, auth.OwnerID)
	}
	return c.Token
}

// stopLeavingOwnerToken stops ts and saves its owner token again, as a
// server that exited without cleaning up leaves it.
func stopLeavingOwnerToken(t *testing.T, ts *testServer, creds *credstore.Store) {
	t.Helper()
	c, ok, err := creds.Lookup(ts.localURL())
	if err != nil || !ok {
		t.Fatalf("Lookup() = %v, %v, want the owner token", ok, err)
	}
	ts.stop()
	if err := creds.Put(c); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
}

func TestOwnerTokenSavedForThisMachine(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	ts := startServer(t, serverOpts{name: "assistant", credentials: creds})
	token := lookupOwnerToken(t, creds, ts.localURL())

	task := send(t, ts.client(token), userText("hello", "", ""))
	wantResult(t, task, a2a.TaskStateCompleted, "Hello from fakeacp.")

	dir := ts.dir.Root
	stopLeavingOwnerToken(t, ts, creds)
	ts = startServer(t, serverOpts{dir: dir, credentials: creds})
	if got := lookupOwnerToken(t, creds, ts.localURL()); got != token {
		t.Fatalf("owner token after a restart = %q, want the saved %q", got, token)
	}

	if _, err := controlapi.NewClient(ts.dir).Revoke(context.Background(), localOwnerName); err != nil {
		t.Fatalf("Revoke(%q) error = %v", localOwnerName, err)
	}
	stopLeavingOwnerToken(t, ts, creds)
	ts = startServer(t, serverOpts{dir: dir, credentials: creds})
	renewed := lookupOwnerToken(t, creds, ts.localURL())
	if renewed == token {
		t.Fatalf("owner token after a revoke = the revoked token, want a new one")
	}
	task = send(t, ts.client(renewed), userText("hello", "", ""))
	wantResult(t, task, a2a.TaskStateCompleted, "Hello from fakeacp.")
}

func TestOwnerTokenReplacedWhenLocalURLChanges(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	other := credstore.Credential{URL: "http://127.0.0.1:1/other", Token: "a2p_other", Agent: "other"}
	if err := creds.Put(other); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	ts := startServer(t, serverOpts{name: "assistant", credentials: creds})
	oldURL := ts.localURL()
	dir := ts.dir.Root
	stopLeavingOwnerToken(t, ts, creds)

	ts = startServer(t, serverOpts{dir: dir, credentials: creds, configure: func(c *agentdir.Config) { c.Port = freePort(t) }})

	if ts.localURL() == oldURL {
		t.Fatalf("local URL after changing the port = %q, want a new one", oldURL)
	}
	all, err := creds.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var urls []string
	for _, c := range all {
		urls = append(urls, c.URL)
	}
	want := []string{other.URL, credstore.Normalize(ts.localURL())}
	if diff := cmp.Diff(want, urls, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Fatalf("saved credential URLs after the local URL changed wrong result (-want +got) diff = %s", diff)
	}
	lookupOwnerToken(t, creds, ts.localURL())
}

func TestOwnerTokenKeepsAgentNameCurrent(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	ts := startServer(t, serverOpts{name: "assistant", credentials: creds})
	token := lookupOwnerToken(t, creds, ts.localURL())
	dir := ts.dir.Root
	stopLeavingOwnerToken(t, ts, creds)

	ts = startServer(t, serverOpts{dir: dir, credentials: creds, configure: func(c *agentdir.Config) { c.Name = "renamed" }})

	got, ok, err := creds.Lookup(ts.localURL())
	if err != nil || !ok {
		t.Fatalf("Lookup() = %v, %v, want the owner token", ok, err)
	}
	if got.Token != token || got.Agent != "renamed" {
		t.Fatalf("saved owner token after renaming the agent = %+v, want token %q for agent %q", got, token, "renamed")
	}
}

func TestOwnerTokenRemovedOnStop(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	ts := startServer(t, serverOpts{credentials: creds})
	local := ts.localURL()
	lookupOwnerToken(t, creds, local)

	ts.stop()

	if c, ok, err := creds.Lookup(local); err != nil || ok {
		t.Fatalf("Lookup(%q) after stop = %+v, %v, %v, want no credential", local, c, ok, err)
	}
}

func TestOwnerTokenOfOriginalKeptWhenRestoredCopyStarts(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	orig := startServer(t, serverOpts{name: "proj", credentials: creds})
	dir := orig.dir.Root
	orig.stop()
	copyDir := shortTempDir(t)
	if err := os.CopyFS(copyDir, os.DirFS(dir)); err != nil {
		t.Fatalf("CopyFS() error = %v", err)
	}
	orig = startServer(t, serverOpts{dir: dir, credentials: creds})
	origToken := lookupOwnerToken(t, creds, orig.localURL())

	restored := startServer(t, serverOpts{dir: copyDir, credentials: creds, configure: func(c *agentdir.Config) { c.Port = freePort(t) }})

	if got := lookupOwnerToken(t, creds, orig.localURL()); got != origToken {
		t.Fatalf("owner token of the original after the restored copy started = %q, want %q", got, origToken)
	}
	task := send(t, orig.client(origToken), userText("hello", "", ""))
	wantResult(t, task, a2a.TaskStateCompleted, "Hello from fakeacp.")
	if got := lookupOwnerToken(t, creds, restored.localURL()); got == origToken {
		t.Fatalf("owner token of the restored copy = the original's token, want its own")
	}
}

func TestOwnerTokenWorksOnlyOnLocalURL(t *testing.T) {
	t.Parallel()
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	net := &fakeProvider{net: &fakeNet{agents: map[string]string{}}, name: "coder", unidentified: true}
	ts := startServer(t, serverOpts{credentials: creds, private: net})
	header := http.Header{"Authorization": {"Bearer " + lookupOwnerToken(t, creds, ts.localURL())}, "Content-Type": {"application/json"}}
	_, private, _ := ts.srv.URLs()
	tests := []struct {
		name string
		url  string
		want int
	}{
		{name: "local", url: ts.localURL(), want: http.StatusOK},
		{name: "private", url: private, want: http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := statusOf(t, http.MethodPost, tc.url+"/", listTasksRPC, header); got != tc.want {
				t.Fatalf("POST %s with the owner token of this machine status = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestOwnerTokenSavedBeforeStatusReportsLocalURL(t *testing.T) {
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
	cfg := &agentdir.Config{Name: "assistant", Harness: "fake", Command: []string{fakeACP}, Sandbox: harness.SandboxLocal}
	if err := dir.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Dir: dir, Config: cfg, Secrets: agentdir.Secrets{}, Credentials: creds, Timings: testTimings})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() error = %v", err)
		}
	})

	deadline := time.Now().Add(30 * time.Second)
	for {
		status, err := controlapi.NewClient(dir).Status(ctx)
		if err == nil && status.Local != "" {
			lookupOwnerToken(t, creds, status.Local)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Status() = %v, %v, want the local URL", status, err)
		}
	}
}

func TestPairRejectsReservedName(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	if _, err := controlapi.NewClient(ts.dir).Pair(context.Background(), controlapi.PairCodeRequest{Name: localOwnerName}); err == nil {
		t.Fatalf("Pair(%q) error = nil, want error", localOwnerName)
	}
}

func TestPairCodeSetsKind(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	ctx := context.Background()
	ctl := controlapi.NewClient(ts.dir)
	tests := []struct {
		name    string
		kind    string
		want    string
		wantErr string
	}{
		{name: "anna", kind: pairapi.PairingPerson, want: pairapi.PairingPerson},
		{name: "reviewer", kind: pairapi.PairingAgent, want: pairapi.PairingAgent},
		{name: "laptop", want: pairapi.PairingPerson},
		{name: "fake", kind: pairapi.PairingWebhook, wantErr: "unknown kind webhook"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, err := ctl.Pair(ctx, controlapi.PairCodeRequest{Name: tc.name})
			if err != nil {
				t.Fatalf("Pair() error = %v", err)
			}
			_, err = pairapi.Pair(ctx, testHTTP, ts.localURL(), code.Code, tc.kind)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("pairapi.Pair() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("pairapi.Pair() error = %v", err)
			}
			p, err := ctl.Pairing(ctx, tc.name)
			if err != nil || p.Kind != tc.want {
				t.Fatalf("Pairing() = %+v, %v, want kind %s", p, err, tc.want)
			}
		})
	}
}

func TestPairRefusesOwnerLinkCode(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant"})
	code := ownerCode(t, ts)

	if _, err := pairapi.Pair(context.Background(), testHTTP, ts.localURL(), code, pairapi.PairingPerson); err == nil || !strings.Contains(err.Error(), "links your chat account") {
		t.Fatalf("pairapi.Pair() with the owner link code error = %v, want a hint to send it to the bot", err)
	}
	if _, err := ts.srv.store.TakeCode(context.Background(), auth.NormalizeCode(code), store.AnyCode); err != nil {
		t.Fatalf("TakeCode() of the owner link code error = %v, want it still pending", err)
	}
}
