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

//go:build agentmesh

package relay_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/googleauth/googletest"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/relay"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/signin"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// agentmeshOne returns the pinned agentmesh-one: the one A2A_SERVE_AGENTMESH_ONE
// names, or the release, downloaded once into the user's cache.
func agentmeshOne(t *testing.T) string {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	f := relay.NewFetcher(filepath.Join(cache, "a2a-serve"), os.Getenv(relay.BinaryEnv), http.DefaultClient, runtime.GOOS, runtime.GOARCH, t.Logf)
	bin, err := f.Get(t.Context())
	if err != nil {
		t.Fatalf("Fetcher.Get() error = %v", err)
	}
	return bin
}

type logWriter struct{ t *testing.T }

func (w logWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

type runningRelay struct {
	url    string
	client *control.Client
	stop   func() error
}

func startRelay(t *testing.T, d relaydir.Dir, bin string) *runningRelay {
	t.Helper()
	return startRelayWith(t, d, bin, googleauth.Endpoints{})
}

func startRelayWith(t *testing.T, d relaydir.Dir, bin string, google googleauth.Endpoints) *runningRelay {
	t.Helper()
	release, err := d.Lock()
	if err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		err := relay.New(relay.Options{
			Dir: d, Binary: bin, A2AServe: "test", Google: google, Out: logWriter{t}, MeshLog: io.Discard, Now: time.Now,
		}).Run(ctx)
		done <- errors.Join(err, release())
	}()
	r := &runningRelay{client: control.NewSocketClient(d.SocketPath(), 10*time.Second)}
	var once bool
	r.stop = func() error {
		if once {
			return nil
		}
		once = true
		cancel()
		return <-done
	}
	t.Cleanup(func() {
		if err := r.stop(); err != nil {
			t.Errorf("relay.Run() error = %v", err)
		}
	})
	deadline := time.Now().Add(time.Minute)
	for {
		st, err := r.client.Status(ctx)
		if err == nil {
			_, err = get(ctx, http.DefaultClient, st.URL+"/readyz")
		}
		if err == nil {
			r.url = st.URL
			return r
		}
		select {
		case err := <-done:
			t.Fatalf("relay.Run() error = %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relay didn't answer on its control socket: %v", err)
		}
	}
}

type joinedAgent struct {
	name       string
	provider   network.Provider
	privateURL string
	publicURL  string
}

func joinAgent(t *testing.T, r *runningRelay, name string) *joinedAgent {
	t.Helper()
	inv, err := r.client.Invite(t.Context(), control.InviteRequest{Name: name})
	if err != nil {
		t.Fatalf("Client.Invite() error = %v", err)
	}
	return serveAgent(t, network.RelayConfig{
		AgentName: name, URL: r.url, StateDir: t.TempDir(), Token: inv.Token, Private: true, Public: true, Logf: t.Logf,
	})
}

// serveAgent joins the agent of cfg to its relay and serves it there.
func serveAgent(t *testing.T, cfg network.RelayConfig) *joinedAgent {
	t.Helper()
	name := cfg.AgentName
	p, err := network.NewRelay(cfg)
	if err != nil {
		t.Fatalf("network.NewRelay() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Logf("closing %s: %v", name, err)
		}
	})
	a := &joinedAgent{name: name, provider: p}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := fmt.Sprintf(`{"name":%q}`, name)
		if r.URL.Path == a2asrv.WellKnownAgentCardPath {
			// The relay passes on only cards with an interface it can carry.
			body = fmt.Sprintf(`{"name":%q,"supportedInterfaces":[{"url":"http://%s","protocolBinding":"JSONRPC","protocolVersion":"1.0"}]}`, name, r.Host)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("writing: %v", err)
		}
	})
	serve := func(ln net.Listener, u *url.URL, err error) string {
		if err != nil {
			t.Fatalf("listening on the relay as %s: %v", name, err)
		}
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				t.Logf("serving %s: %v", name, err)
			}
		}()
		t.Cleanup(func() {
			if err := srv.Close(); err != nil {
				t.Logf("closing: %v", err)
			}
		})
		return u.String()
	}
	a.privateURL = serve(p.ListenPrivate(t.Context()))
	a.publicURL = serve(p.ListenPublic(t.Context()))
	return a
}

func get(ctx context.Context, hc *http.Client, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			return
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, body)
	}
	return string(body), nil
}

// eventually retries fn until it succeeds or a minute passes.
func eventually(t *testing.T, what string, fn func() error) {
	t.Helper()
	eventuallyWithin(t, time.Minute, what, fn)
}

// eventuallyWithin retries fn until it succeeds or d passes.
func eventuallyWithin(t *testing.T, d time.Duration, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		err := fn()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func agentNames(ms []control.Member) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Agent)
	}
	slices.Sort(out)
	return out
}

func TestRelayRunsAgentMesh(t *testing.T) {
	bin := agentmeshOne(t)
	port := freePort(t)
	d, err := relaydir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	relayURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	if _, err := d.SaveSettings(relaydir.Settings{URL: relayURL, Listen: fmt.Sprintf("127.0.0.1:%d", port)}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	r := startRelay(t, d, bin)

	coder := joinAgent(t, r, "coder")
	reviewer := joinAgent(t, r, "reviewer")

	eventually(t, "coder calls reviewer on the relay", func() error {
		got, err := get(t.Context(), coder.provider.HTTPClient(), reviewer.privateURL+"/hello")
		if err == nil && got != `{"name":"reviewer"}` {
			return fmt.Errorf("got %q", got)
		}
		return err
	})
	eventually(t, "coder finds reviewer on the relay", func() error {
		peers, err := coder.provider.Peers(t.Context())
		if err != nil {
			return err
		}
		var names []string
		for _, p := range peers {
			names = append(names, p.Name)
		}
		if diff := cmp.Diff([]string{"reviewer"}, names); diff != "" {
			return fmt.Errorf("Peers() wrong names (-want +got) diff = %s", diff)
		}
		return nil
	})
	if !strings.HasPrefix(reviewer.publicURL, relayURL+"/ingress/") {
		t.Fatalf("reviewer's public URL = %s, want one under %s/ingress/", reviewer.publicURL, relayURL)
	}
	eventually(t, "a caller outside the relay calls reviewer's public URL", func() error {
		got, err := get(t.Context(), http.DefaultClient, reviewer.publicURL+"/hello")
		if err == nil && got != `{"name":"reviewer"}` {
			return fmt.Errorf("got %q", got)
		}
		return err
	})
	eventually(t, "members lists both agents", func() error {
		ms, err := r.client.Members(t.Context(), false)
		if err != nil {
			return err
		}
		if diff := cmp.Diff([]string{"coder", "reviewer"}, agentNames(ms)); diff != "" {
			return fmt.Errorf("Members() wrong agents (-want +got) diff = %s", diff)
		}
		return nil
	})

	res, err := r.client.Remove(t.Context(), control.RemoveRequest{Target: "reviewer"})
	if err != nil {
		t.Fatalf("Client.Remove() error = %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0].Agent != "reviewer" {
		t.Fatalf("Client.Remove() = %+v, want reviewer", res.Removed)
	}
	ms, err := r.client.Members(t.Context(), false)
	if err != nil {
		t.Fatalf("Client.Members() error = %v", err)
	}
	if diff := cmp.Diff([]string{"coder"}, agentNames(ms)); diff != "" {
		t.Fatalf("Client.Members() after removal wrong agents (-want +got) diff = %s", diff)
	}

	if err := r.stop(); err != nil {
		t.Fatalf("relay.Run() error = %v", err)
	}
	r = startRelay(t, d, bin)
	ms, err = r.client.Members(t.Context(), true)
	if err != nil {
		t.Fatalf("Client.Members() after a restart error = %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("Client.Members() after a restart = %+v, want coder and the removed reviewer", ms)
	}
}

func TestRelayLetsBackARemovedAgent(t *testing.T) {
	bin := agentmeshOne(t)
	port := freePort(t)
	d, err := relaydir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SaveSettings(relaydir.Settings{URL: fmt.Sprintf("http://127.0.0.1:%d", port), Listen: fmt.Sprintf("127.0.0.1:%d", port)}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	r := startRelay(t, d, bin)
	coder := joinAgent(t, r, "coder")
	inv, err := r.client.Invite(t.Context(), control.InviteRequest{Name: "reviewer"})
	if err != nil {
		t.Fatalf("Client.Invite() error = %v", err)
	}
	cfg := network.RelayConfig{AgentName: "reviewer", URL: r.url, StateDir: t.TempDir(), Token: inv.Token, Private: true, Public: true, Logf: t.Logf}
	reviewer := serveAgent(t, cfg)
	eventually(t, "members lists reviewer", func() error {
		_, err := r.client.Remove(t.Context(), control.RemoveRequest{Target: "reviewer"})
		return err
	})
	if err := reviewer.provider.Close(); err != nil {
		t.Fatalf("closing reviewer: %v", err)
	}

	p, err := network.NewRelay(cfg)
	if err != nil {
		t.Fatalf("network.NewRelay() error = %v", err)
	}
	if _, _, err := p.ListenPrivate(t.Context()); err == nil || !strings.Contains(err.Error(), "removed this agent") {
		t.Fatalf("joining a relay that removed the agent: error = %v, want one that says the relay removed it", err)
	}

	inv, err = r.client.Invite(t.Context(), control.InviteRequest{Name: "reviewer"})
	if err != nil {
		t.Fatalf("Client.Invite() error = %v", err)
	}
	if inv.LetBack == nil {
		t.Fatal("Client.Invite() let back no agent, want reviewer")
	}
	cfg.Token = inv.Token
	back := serveAgent(t, cfg)
	if back.privateURL != reviewer.privateURL {
		t.Fatalf("reviewer's private URL after it came back = %s, want %s", back.privateURL, reviewer.privateURL)
	}
	// Agent Mesh announces no unban. Coder's node lifts its ban of reviewer at
	// its next sync with the relay, which the let-back's policy update starts
	// after a random delay of up to a tenth of meshnet's 15-minute interval.
	const meshSyncJitter = 90 * time.Second
	eventuallyWithin(t, meshSyncJitter+10*time.Second, "coder calls reviewer after it came back", func() error {
		_, err := get(t.Context(), coder.provider.HTTPClient(), reviewer.privateURL+"/hello")
		return err
	})
}

func TestRelayLetsBackAnAgentRemovedBeforeARestart(t *testing.T) {
	bin := agentmeshOne(t)
	port := freePort(t)
	d, err := relaydir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SaveSettings(relaydir.Settings{URL: fmt.Sprintf("http://127.0.0.1:%d", port), Listen: fmt.Sprintf("127.0.0.1:%d", port)}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	r := startRelay(t, d, bin)
	inv, err := r.client.Invite(t.Context(), control.InviteRequest{Name: "reviewer"})
	if err != nil {
		t.Fatalf("Client.Invite() error = %v", err)
	}
	cfg := network.RelayConfig{AgentName: "reviewer", URL: r.url, StateDir: t.TempDir(), Token: inv.Token, Private: true, Public: true, Logf: t.Logf}
	reviewer := serveAgent(t, cfg)
	eventually(t, "members lists reviewer", func() error {
		_, err := r.client.Remove(t.Context(), control.RemoveRequest{Target: "reviewer"})
		return err
	})
	if err := reviewer.provider.Close(); err != nil {
		t.Fatalf("closing reviewer: %v", err)
	}
	if err := r.stop(); err != nil {
		t.Fatalf("relay.Run() error = %v", err)
	}
	r = startRelay(t, d, bin)
	// Letting the agent back lifts the revocation of its credential. The
	// router keeps refusing that credential only once it synced with the
	// control plane while the agent was removed, which the restarted router
	// does after it turns ready. Its sync loads the bans a step before the
	// revocations, and a join reports a refusal only after retrying for
	// 14 seconds.
	eventually(t, "the restarted relay refuses the removed reviewer", func() error {
		if err := tryJoin(t, cfg); err == nil || !strings.Contains(err.Error(), "removed this agent") {
			return fmt.Errorf("joining with the removed credential: error = %v, want the agent removed", err)
		}
		return nil
	})

	inv, err = r.client.Invite(t.Context(), control.InviteRequest{Name: "reviewer"})
	if err != nil {
		t.Fatalf("Client.Invite() error = %v", err)
	}
	if inv.LetBack == nil {
		t.Fatal("Client.Invite() let back no agent, want reviewer")
	}
	mistyped := cfg
	mistyped.Token = inv.Token[1:]
	for range 2 {
		var invite *network.InviteError
		if err := tryJoin(t, mistyped); !errors.As(err, &invite) {
			t.Fatalf("joining with a mistyped invite: error = %v, want an invite error", err)
		}
	}
	cfg.Token = inv.Token
	back := serveAgent(t, cfg)
	if back.privateURL != reviewer.privateURL {
		t.Fatalf("reviewer's private URL after it came back = %s, want %s", back.privateURL, reviewer.privateURL)
	}
}

// tryJoin joins the relay with cfg, leaves it again, and returns the join
// error.
func tryJoin(t *testing.T, cfg network.RelayConfig) error {
	t.Helper()
	p, err := network.NewRelay(cfg)
	if err != nil {
		t.Fatalf("network.NewRelay() error = %v", err)
	}
	if _, _, err := p.ListenPrivate(t.Context()); err != nil {
		return err
	}
	if err := p.Close(); err != nil {
		t.Fatalf("closing the joined agent: %v", err)
	}
	return nil
}

func TestRelayKeepsAnAgentWhoseInviteIsRefused(t *testing.T) {
	bin := agentmeshOne(t)
	port := freePort(t)
	d, err := relaydir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SaveSettings(relaydir.Settings{URL: fmt.Sprintf("http://127.0.0.1:%d", port), Listen: fmt.Sprintf("127.0.0.1:%d", port)}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	r := startRelay(t, d, bin)
	inv, err := r.client.Invite(t.Context(), control.InviteRequest{Name: "reviewer"})
	if err != nil {
		t.Fatalf("Client.Invite() error = %v", err)
	}
	cfg := network.RelayConfig{AgentName: "reviewer", URL: r.url, StateDir: t.TempDir(), Token: inv.Token, Private: true, Public: true, Logf: t.Logf}
	reviewer := serveAgent(t, cfg)
	if err := reviewer.provider.Close(); err != nil {
		t.Fatalf("closing reviewer: %v", err)
	}
	// An agent that joined before a2a-serve recorded its invite.
	if err := os.Remove(filepath.Join(cfg.StateDir, "invite-hash")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		token       string
		wantProblem bool
	}{
		{token: inv.Token},
		{token: "not-an-invite", wantProblem: true},
		{token: "not-an-invite", wantProblem: true},
		{token: inv.Token},
	}
	for _, tc := range tests {
		cfg.Token = tc.token
		back := serveAgent(t, cfg)
		if back.privateURL != reviewer.privateURL {
			t.Fatalf("reviewer's private URL after a start with token %q = %s, want %s", tc.token, back.privateURL, reviewer.privateURL)
		}
		if err := back.provider.Close(); err != nil {
			t.Fatalf("closing reviewer: %v", err)
		}
		problem, err := network.InviteProblem(cfg.StateDir)
		if err != nil {
			t.Fatalf("network.InviteProblem() error = %v", err)
		}
		if got := problem != ""; got != tc.wantProblem {
			t.Fatalf("network.InviteProblem() after a start with token %q = %q, want a problem: %v", tc.token, problem, tc.wantProblem)
		}
	}
}

// browser is the person behind signed-in agents: it approves every sign-in
// link an agent prints, signing in at the fake Google at once.
type browser struct {
	t     *testing.T
	relay string
	hc    *http.Client
	links atomic.Int32
}

var approvalToken = regexp.MustCompile(`name="token" value="([^"]+)"`)

// logf is the Logf of an agent: it opens the sign-in links the agent prints.
func (b *browser) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	b.t.Log(line)
	_, rest, ok := strings.Cut(line, "→ sign in to the relay "+b.relay+": ")
	if !ok {
		return
	}
	link, _, _ := strings.Cut(rest, " (code ")
	b.links.Add(1)
	go b.approve(link)
}

func (b *browser) approve(link string) {
	page, err := get(context.Background(), b.hc, link)
	if err != nil {
		b.t.Errorf("opening the sign-in link: %v", err)
		return
	}
	m := approvalToken.FindStringSubmatch(page)
	if m == nil {
		b.t.Errorf("no approval form on the sign-in page:\n%s", page)
		return
	}
	resp, err := b.hc.PostForm(signin.URL(b.relay)+"/approve", url.Values{"token": {m[1]}, "answer": {"approve"}})
	if err != nil {
		b.t.Errorf("approving the sign-in: %v", err)
		return
	}
	if err := resp.Body.Close(); err != nil {
		b.t.Errorf("closing: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b.t.Errorf("approving the sign-in: %s", resp.Status)
	}
}

func TestRelaySignIn(t *testing.T) {
	bin := agentmeshOne(t)
	g := googletest.New(t, googletest.Account{Subject: "anna-id", Email: "anna@example.com", Verified: true, HostedDomain: "example.com"})
	port := freePort(t)
	d, err := relaydir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	relayURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	if _, err := d.SaveSettings(relaydir.Settings{
		URL: relayURL, Listen: fmt.Sprintf("127.0.0.1:%d", port),
		AllowEmails: []string{"*@example.com"}, GoogleClientID: "relay-app", GoogleClientSecret: g.Secret,
	}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	r := startRelayWith(t, d, bin, g.Endpoints())
	b := &browser{t: t, relay: r.url, hc: &http.Client{Timeout: 30 * time.Second}}
	stateDir := t.TempDir()
	login := &network.RelayLogin{RelayURL: r.url, StateDir: stateDir, HTTP: b.hc, Logf: b.logf, Poll: time.Second}

	idToken, err := login.Token(t.Context())
	if err != nil {
		t.Fatalf("RelayLogin.Token() error = %v", err)
	}
	if got := b.links.Load(); got != 1 {
		t.Fatalf("sign-in links printed = %d, want 1", got)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.url+"/user/bootstrap-tokens", strings.NewReader(`{"role":"mesh:role:node"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+idToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /user/bootstrap-tokens with a signed-in identity status = %d, want 403", resp.StatusCode)
	}

	coder := serveAgent(t, network.RelayConfig{AgentName: "coder", URL: r.url, StateDir: stateDir, Private: true, Public: true, Logf: b.logf})
	reviewer := joinAgent(t, r, "reviewer")

	if got := b.links.Load(); got != 1 {
		t.Fatalf("sign-in links printed after the agent joined = %d, want 1: it joins with its refresh token", got)
	}
	eventually(t, "the invited reviewer calls the signed-in coder", func() error {
		got, err := get(t.Context(), reviewer.provider.HTTPClient(), coder.privateURL+"/hello")
		if err == nil && got != `{"name":"coder"}` {
			return fmt.Errorf("got %q", got)
		}
		return err
	})
	eventually(t, "members shows who signed in", func() error {
		ms, err := r.client.Members(t.Context(), false)
		if err != nil {
			return err
		}
		var who []string
		for _, m := range ms {
			who = append(who, m.Agent+" "+m.Who())
		}
		slices.Sort(who)
		if diff := cmp.Diff([]string{"coder anna@example.com", "reviewer invite reviewer"}, who); diff != "" {
			return fmt.Errorf("Members() wrong result (-want +got) diff = %s", diff)
		}
		return nil
	})
	st, err := r.client.Status(t.Context())
	if err != nil {
		t.Fatalf("Client.Status() error = %v", err)
	}
	if want := "sign-in (*@example.com) and invites"; st.Join != want {
		t.Fatalf("Client.Status() join = %q, want %q", st.Join, want)
	}

	restartAllowing := func(allow string) {
		t.Helper()
		if err := r.stop(); err != nil {
			t.Fatalf("relay.Run() error = %v", err)
		}
		if _, err := d.SaveSettings(relaydir.Settings{AllowEmails: []string{allow}}); err != nil {
			t.Fatalf("SaveSettings() error = %v", err)
		}
		r = startRelayWith(t, d, bin, g.Endpoints())
	}
	banned := func() []string {
		t.Helper()
		ms, err := r.client.Members(t.Context(), true)
		if err != nil {
			t.Fatalf("Client.Members() error = %v", err)
		}
		var out []string
		for _, m := range ms {
			if m.Banned {
				out = append(out, m.Who())
			}
		}
		return out
	}

	restartAllowing("ben@example.com")
	if diff := cmp.Diff([]string{"anna@example.com"}, banned()); diff != "" {
		t.Fatalf("banned members after dropping anna@example.com wrong result (-want +got) diff = %s", diff)
	}

	restartAllowing("anna@example.com")
	if got := banned(); len(got) != 0 {
		t.Fatalf("banned members after allowing anna@example.com again = %v, want none", got)
	}
}
