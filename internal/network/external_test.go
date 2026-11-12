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

package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/testutil"
	"github.com/a2aproject/a2a-serve/netprovider"
)

var loopnet string

func TestMain(m *testing.M) {
	dir, err := testutil.MkdirShortTemp()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	loopnet = filepath.Join(dir, "loopnet")
	if out, err := exec.Command("go", "build", "-o", loopnet, "github.com/a2aproject/a2a-serve/netprovider/internal/loopnet").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building loopnet: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func startLoopnet(t *testing.T, url, agent string, public bool) *external {
	t.Helper()
	e := NewExternal(ExternalConfig{
		Name: "loopnet", Binary: loopnet, AgentName: agent, URL: url, StateDir: t.TempDir(), Private: true, Public: public, Logf: t.Logf,
	}).(*external)
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("external.Close() error = %v", err)
		}
	})
	if err := e.Join(t.Context()); err != nil {
		t.Fatalf("external.Join() error = %v", err)
	}
	return e
}

type whoAmI struct {
	Peer      *Peer
	ClientKey string
}

// serveWhoAmI serves an agent card, and answers other requests with who
// the provider says calls.
func serveWhoAmI(t *testing.T, e *external) {
	t.Helper()
	serve := func(public bool) {
		listen := e.ListenPrivate
		if public {
			listen = e.ListenPublic
		}
		ln, _, err := listen(t.Context())
		if err != nil {
			t.Fatalf("listen(public=%v) error = %v", public, err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/agent-card.json" {
				if _, err := fmt.Fprintf(w, `{"name":%q,"description":"test agent"}`, e.cfg.AgentName); err != nil {
					t.Errorf("writing the card: %v", err)
				}
				return
			}
			var res whoAmI
			if p, err := e.WhoIs(r.Context(), r.RemoteAddr); err == nil {
				res.Peer = p
			}
			res.ClientKey = e.ClientKey(r.RemoteAddr, public)
			if err := json.NewEncoder(w).Encode(res); err != nil {
				t.Errorf("encoding: %v", err)
			}
		})}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("Serve() error = %v", err)
			}
		}()
		t.Cleanup(func() {
			if err := srv.Close(); err != nil {
				t.Errorf("Server.Close() error = %v", err)
			}
		})
	}
	serve(false)
	if e.cfg.Public {
		serve(true)
	}
}

func getWhoAmI(t *testing.T, c *http.Client, u string) whoAmI {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", u, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	var got whoAmI
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Get(%s) = %s: %v", u, body, err)
	}
	return got
}

func TestExternalIdentifiesCallers(t *testing.T) {
	t.Parallel()
	dir := testutil.ShortTempDir(t)
	coder := startLoopnet(t, "loopnet://"+dir+"?owner=Ben@example.com", "coder", true)
	reviewer := startLoopnet(t, "loopnet://"+dir+"?owner=ben@example.com", "reviewer", false)
	stranger := startLoopnet(t, "loopnet://"+dir+"?owner=not-an-email", "stranger", false)
	serveWhoAmI(t, coder)
	serveWhoAmI(t, reviewer)
	serveWhoAmI(t, stranger)

	tests := []struct {
		name string
		from *external
		url  string
		want whoAmI
	}{
		{
			name: "same owner",
			from: reviewer,
			url:  "http://loopnet.local/coder/whoami",
			want: whoAmI{Peer: &Peer{Node: "loop:reviewer", Machine: "reviewer", Email: "ben@example.com", Self: true}, ClientKey: "net:loop:reviewer"},
		},
		{
			name: "email that isn't an address",
			from: stranger,
			url:  "http://loopnet.local/coder/whoami",
			want: whoAmI{Peer: &Peer{Node: "loop:stranger", Machine: "stranger"}, ClientKey: "net:loop:stranger"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getWhoAmI(t, &http.Client{Transport: tc.from.HTTPClient().Transport, Timeout: 10 * time.Second}, tc.url)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("WhoIs wrong result (-want +got) diff = %s", diff)
			}
		})
	}

	_, publicURL, err := coder.ListenPublic(t.Context())
	if err != nil {
		t.Fatalf("ListenPublic() error = %v", err)
	}
	got := getWhoAmI(t, &http.Client{Timeout: 10 * time.Second}, publicURL.String()+"/whoami")
	if diff := cmp.Diff(whoAmI{ClientKey: "net:ip:127.0.0.1"}, got); diff != "" {
		t.Fatalf("public WhoIs wrong result (-want +got) diff = %s", diff)
	}

	peers, err := reviewer.Peers(t.Context())
	if err != nil {
		t.Fatalf("Peers() error = %v", err)
	}
	wantPeers := []PeerInfo{
		{Name: "coder", URL: "http://loopnet.local/coder", Description: "test agent", Online: true, Node: "loop:coder", CardName: "coder"},
		{Name: "stranger", URL: "http://loopnet.local/stranger", Description: "test agent", Online: true, Node: "loop:stranger", CardName: "stranger"},
	}
	if diff := cmp.Diff(wantPeers, peers, cmpopts.IgnoreFields(PeerInfo{}, "Card")); diff != "" {
		t.Fatalf("Peers() wrong result (-want +got) diff = %s", diff)
	}

	if _, err := reviewer.Member(t.Context(), "loop:nobody"); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Member(loop:nobody) error = %v, want %v", err, ErrNoIdentity)
	}
	if p, err := reviewer.Member(t.Context(), "loop:coder"); err != nil || p.Node != "loop:coder" {
		t.Fatalf("Member(loop:coder) = %+v, %v, want coder", p, err)
	}
	if got := coder.PrivateNote(); got != "works only from other agents on loopnet" {
		t.Fatalf("PrivateNote() = %q, want the provider's note", got)
	}
}

func TestExternalIgnoresOwnerOfATokenJoin(t *testing.T) {
	t.Parallel()
	e := NewExternal(ExternalConfig{
		Name: "loopnet", Binary: loopnet, AgentName: "coder", URL: "loopnet://" + testutil.ShortTempDir(t) + "?owner=ben@example.com",
		Token: "invite", StateDir: t.TempDir(), Private: true,
	}).(*external)
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("external.Close() error = %v", err)
		}
	})
	if err := e.Join(t.Context()); err != nil {
		t.Fatalf("external.Join() error = %v", err)
	}
	if p := e.peer(netprovider.Caller{Node: "n", Email: "ben@example.com"}); p.Self {
		t.Fatalf("peer() = %+v, want no owner for an agent that joined with a token", p)
	}
}

func TestExternalReportsExit(t *testing.T) {
	t.Parallel()
	dir := testutil.ShortTempDir(t)
	e := startLoopnet(t, "loopnet://"+dir, "coder", false)
	if !e.Health().OK() {
		t.Fatalf("Health() = %+v, want OK", e.Health())
	}
	e.mu.Lock()
	proc := e.proc
	e.mu.Unlock()
	if err := proc.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.Health().OK() {
		if time.Now().After(deadline) {
			t.Fatalf("Health() still OK after the provider exited")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h := e.Health(); h.State != HealthDisconnected || h.Fix != "run `a2a-serve restart`" {
		t.Fatalf("Health() = %+v, want disconnected with the restart fix", h)
	}
}

func TestExternalTakesHealthWhileJoining(t *testing.T) {
	t.Parallel()
	e := startLoopnet(t, "loopnet://"+testutil.ShortTempDir(t)+"?health=disconnected", "coder", false)
	want := Health{State: HealthDisconnected, Problem: "loopnet reports disconnected"}
	deadline := time.Now().Add(10 * time.Second)
	for e.Health() != want {
		if time.Now().After(deadline) {
			t.Fatalf("Health() = %+v, want %+v, what the provider reported while joining", e.Health(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSanitize(t *testing.T) {
	t.Parallel()
	if got := Sanitize("a\x1b[2Jb\x1b[0m\a\n", 300); got != "ab" {
		t.Fatalf("Sanitize() = %q, want escapes and control characters dropped", got)
	}
	if got := Sanitize("abcdef", 3); got != "abc…" {
		t.Fatalf("Sanitize() = %q, want abc…", got)
	}
}
