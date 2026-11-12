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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/googleauth/googletest"
	"github.com/a2aproject/a2a-serve/internal/relay"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/meshbackend"
	"github.com/a2aproject/a2a-serve/internal/relay/signin"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
	"github.com/a2aproject/a2a-serve/netprovider"
	"github.com/a2aproject/a2a-serve/netprovider/conformance"
	"github.com/a2aproject/a2a-serve/netprovider/host"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// providerBinary builds netprovider-agentmesh the way releases are built,
// once per test run.
func providerBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "netprovider-agentmesh")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "netprovider-agentmesh")
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("building the provider: %v", buildErr)
	}
	return binPath
}

func TestMain(m *testing.M) {
	code := m.Run()
	if binPath != "" {
		if err := os.RemoveAll(filepath.Dir(binPath)); err != nil {
			fmt.Fprintf(os.Stderr, "removing the provider binary: %v\n", err)
		}
	}
	os.Exit(code)
}

// agentmeshOne returns the pinned agentmesh-one: the one A2A_SERVE_AGENTMESH_ONE
// names, or the release, downloaded once into the user's cache.
func agentmeshOne(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs an Agent Mesh relay")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	f := meshbackend.NewFetcher(filepath.Join(cache, "a2a-serve"), os.Getenv(meshbackend.BinaryEnv), http.DefaultClient, runtime.GOOS, runtime.GOARCH, t.Logf)
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

func openRelayDir(t *testing.T, s relaydir.Settings) relaydir.Dir {
	t.Helper()
	port := freePort(t)
	d, err := relaydir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.URL, s.Listen = fmt.Sprintf("http://127.0.0.1:%d", port), fmt.Sprintf("127.0.0.1:%d", port)
	if _, err := d.SaveSettings(s); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	return d
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
			Dir: d, Backend: meshbackend.New(meshbackend.Options{Binary: bin, Log: io.Discard, Now: time.Now}),
			A2AServe: "test", Google: google, Out: logWriter{t}, TunnelLog: io.Discard, Now: time.Now,
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

func (r *runningRelay) invite(t *testing.T, name string) *control.Invite {
	t.Helper()
	inv, err := r.client.Invite(t.Context(), control.InviteRequest{Name: name})
	if err != nil {
		t.Fatalf("Client.Invite() error = %v", err)
	}
	return inv
}

// runDir returns a directory for the provider's sockets, whose paths have
// to stay short.
func runDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "am")
	if err != nil {
		t.Fatalf("os.MkdirTemp() error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("os.RemoveAll() error = %v", err)
		}
	})
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("os.Chmod() error = %v", err)
	}
	return dir
}

// meshAgent is an agent served through the provider binary, as the server
// runs it.
type meshAgent struct {
	name   string
	proc   *host.Process
	joined netprovider.JoinResult
	ep     *host.Endpoints
	// client sends requests through the provider's CONNECT tunnel.
	client *http.Client

	mu     sync.Mutex
	health []netprovider.Health
}

func (a *meshAgent) healthStates() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, h := range a.health {
		out = append(out, h.State)
	}
	return out
}

func (a *meshAgent) close(t *testing.T) {
	t.Helper()
	if err := a.proc.Close(); err != nil {
		t.Fatalf("closing %s: %v", a.name, err)
	}
}

// startProvider starts the provider binary, with notices going to
// onNotice.
func startProvider(t *testing.T, onNotice func(string), onHealth func(netprovider.Health)) *host.Process {
	t.Helper()
	p, err := host.Start(host.Config{
		Path: providerBinary(t), RunDir: runDir(t), Env: host.Environ(os.Environ()), Stderr: logWriter{t},
		OnNotice: func(n netprovider.Notice) {
			t.Logf("notice: %s", n.Message)
			if onNotice != nil {
				onNotice(n.Message)
			}
		},
		OnHealth: onHealth,
	})
	if err != nil {
		t.Fatalf("host.Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Logf("closing the provider: %v", err)
		}
	})
	return p
}

// tryJoin joins the relay with req in a new provider, leaves it again, and
// returns the join error.
func tryJoin(t *testing.T, req netprovider.JoinRequest, onNotice func(string)) error {
	t.Helper()
	p := startProvider(t, onNotice, nil)
	defer func() {
		if err := p.Close(); err != nil {
			t.Fatalf("closing the provider: %v", err)
		}
	}()
	_, err := p.Join(t.Context(), req)
	return err
}

func joinRequest(name, relayURL, token string) netprovider.JoinRequest {
	return netprovider.JoinRequest{Agent: name, URL: relayURL, Token: token, Private: true, Public: true}
}

// whoCalled is what an agent answers: its name and the caller the
// provider's header named.
type whoCalled struct {
	Name   string             `json:"name"`
	Caller netprovider.Caller `json:"caller"`
}

type callerKey struct{}

// serveAgent joins the agent of req to its relay through the provider and
// serves it there.
func serveAgent(t *testing.T, req netprovider.JoinRequest, onNotice func(string)) *meshAgent {
	t.Helper()
	if req.StateDir == "" {
		req.StateDir = t.TempDir()
	}
	a := &meshAgent{name: req.Agent}
	a.proc = startProvider(t, onNotice, func(h netprovider.Health) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.health = append(a.health, h)
	})
	var err error
	if a.joined, err = a.proc.Join(t.Context(), req); err != nil {
		t.Fatalf("joining the relay as %s: %v", req.Agent, err)
	}
	if a.ep, err = a.proc.Listen(t.Context(), req.Private, req.Public); err != nil {
		t.Fatalf("listening on the relay as %s: %v", req.Agent, err)
	}
	a.client = &http.Client{Transport: a.proc.Transport(), Timeout: 30 * time.Second}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == a2asrv.WellKnownAgentCardPath {
			// The relay passes on only cards with an interface it can carry.
			if _, err := fmt.Fprintf(w, `{"name":%q,"supportedInterfaces":[{"url":"http://%s","protocolBinding":"JSONRPC","protocolVersion":"1.0"}]}`, req.Agent, r.Host); err != nil {
				t.Errorf("writing: %v", err)
			}
			return
		}
		caller, _ := r.Context().Value(callerKey{}).(netprovider.Caller)
		if err := json.NewEncoder(w).Encode(whoCalled{Name: req.Agent, Caller: caller}); err != nil {
			t.Errorf("writing: %v", err)
		}
	})
	for _, ln := range []*host.Listener{a.ep.Private, a.ep.Public} {
		if ln == nil {
			continue
		}
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if hc, ok := c.(*host.Conn); ok {
				return context.WithValue(ctx, callerKey{}, hc.Caller())
			}
			return ctx
		}}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				t.Logf("serving %s: %v", req.Agent, err)
			}
		}()
		t.Cleanup(func() {
			if err := srv.Close(); err != nil {
				t.Logf("closing: %v", err)
			}
		})
	}
	return a
}

func joinAgent(t *testing.T, r *runningRelay, name string) *meshAgent {
	t.Helper()
	return serveAgent(t, joinRequest(name, r.url, r.invite(t, name).Token), nil)
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
	defer closeResp(resp)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, body)
	}
	return string(body), nil
}

// call gets rawURL with hc and decodes who answered.
func call(ctx context.Context, hc *http.Client, rawURL string) (whoCalled, error) {
	body, err := get(ctx, hc, rawURL)
	if err != nil {
		return whoCalled{}, err
	}
	var got whoCalled
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		return whoCalled{}, fmt.Errorf("decoding %q: %w", body, err)
	}
	return got, nil
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

func wantKind(t *testing.T, err error, kind string) *netprovider.Error {
	t.Helper()
	var perr *netprovider.Error
	if !errors.As(err, &perr) || perr.Kind != kind {
		t.Fatalf("join error = %v, want a %s error", err, kind)
	}
	return perr
}

func TestRelayRunsAgentMesh(t *testing.T) {
	bin := agentmeshOne(t)
	d := openRelayDir(t, relaydir.Settings{})
	r := startRelay(t, d, bin)

	coderInvite := r.invite(t, "coder")
	coder := serveAgent(t, joinRequest("coder", r.url, coderInvite.Token), nil)
	reviewer := joinAgent(t, r, "reviewer")

	if want := "works only from other a2a-serve agents on the relay; give people the public URL"; reviewer.ep.PrivateNote != want {
		t.Fatalf("listen private note = %q, want %q", reviewer.ep.PrivateNote, want)
	}
	if coder.joined.Owner != "" {
		t.Fatalf("join owner of an invited agent = %q, want none", coder.joined.Owner)
	}
	eventually(t, "coder calls reviewer on the relay", func() error {
		got, err := call(t.Context(), coder.client, reviewer.ep.PrivateURL+"/hello")
		if err != nil {
			return err
		}
		want := whoCalled{Name: "reviewer", Caller: netprovider.Caller{Node: coder.joined.Node, Machine: coder.joined.Node, ClientKey: "peer:" + coder.joined.Node}}
		if diff := cmp.Diff(want, got); diff != "" {
			return fmt.Errorf("call wrong result (-want +got) diff = %s", diff)
		}
		return nil
	})
	eventually(t, "coder finds reviewer on the relay", func() error {
		peers, err := coder.proc.Peers(t.Context())
		if err != nil {
			return err
		}
		want := []netprovider.PeerInfo{{Name: "reviewer", URL: reviewer.ep.PrivateURL, Node: reviewer.joined.Node}}
		if diff := cmp.Diff(want, peers); diff != "" {
			return fmt.Errorf("peers wrong result (-want +got) diff = %s", diff)
		}
		return nil
	})
	if !strings.HasPrefix(reviewer.ep.PublicURL, r.url+"/ingress/") {
		t.Fatalf("reviewer's public URL = %s, want one under %s/ingress/", reviewer.ep.PublicURL, r.url)
	}
	eventually(t, "a caller outside the relay calls reviewer's public URL", func() error {
		got, err := call(t.Context(), http.DefaultClient, reviewer.ep.PublicURL+"/hello")
		if err != nil {
			return err
		}
		if got.Name != "reviewer" || got.Caller.Identified() {
			return fmt.Errorf("public call = %+v, want reviewer and no identity", got)
		}
		return nil
	})
	used := wantKind(t, tryJoin(t, netprovider.JoinRequest{Agent: "coder", URL: r.url, Token: coderInvite.Token, StateDir: t.TempDir(), Private: true}, nil), netprovider.KindToken)
	wantUsed := netprovider.Error{Kind: netprovider.KindToken, Problem: "the invite was already used", Fix: "ask the relay's admin for a new invite (`a2a-serve relay invite coder`)"}
	if diff := cmp.Diff(wantUsed, *used); diff != "" {
		t.Fatalf("join with a used invite error wrong result (-want +got) diff = %s", diff)
	}
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
	eventually(t, "reviewer reports that the relay removed it", func() error {
		if states := reviewer.healthStates(); !slices.Contains(states, netprovider.HealthRemoved) {
			return fmt.Errorf("health states = %q, want %q", states, netprovider.HealthRemoved)
		}
		return nil
	})
}

func TestRelayConformance(t *testing.T) {
	bin := agentmeshOne(t)
	r := startRelay(t, openRelayDir(t, relaydir.Settings{}), bin)
	conformance.Run(t, conformance.Config{Binary: providerBinary(t), URL: r.url, Token: r.invite(t, "conformance").Token})
}

func TestRelayLetsBackARemovedAgent(t *testing.T) {
	bin := agentmeshOne(t)
	r := startRelay(t, openRelayDir(t, relaydir.Settings{}), bin)
	coder := joinAgent(t, r, "coder")
	req := joinRequest("reviewer", r.url, r.invite(t, "reviewer").Token)
	req.StateDir = t.TempDir()
	reviewer := serveAgent(t, req, nil)
	eventually(t, "members lists reviewer", func() error {
		_, err := r.client.Remove(t.Context(), control.RemoveRequest{Target: "reviewer"})
		return err
	})
	reviewer.close(t)

	perr := wantKind(t, tryJoin(t, req, nil), netprovider.KindRemoved)
	want := fmt.Sprintf("the relay %s removed this agent (peer %s); ask its operator to let it back with `a2a-serve relay invite reviewer`, then run `a2a-serve restart`", r.url, reviewer.joined.Node)
	if perr.Problem != want {
		t.Fatalf("join of a removed agent problem = %q, want %q", perr.Problem, want)
	}

	inv := r.invite(t, "reviewer")
	if inv.LetBack == nil {
		t.Fatal("Client.Invite() let back no agent, want reviewer")
	}
	req.Token = inv.Token
	back := serveAgent(t, req, nil)
	if back.ep.PrivateURL != reviewer.ep.PrivateURL {
		t.Fatalf("reviewer's private URL after it came back = %s, want %s", back.ep.PrivateURL, reviewer.ep.PrivateURL)
	}
	// Agent Mesh announces no unban. Coder's node lifts its ban of reviewer at
	// its next sync with the relay, which the let-back's policy update starts
	// after a random delay of up to a tenth of meshnet's 15-minute interval.
	const meshSyncJitter = 90 * time.Second
	eventuallyWithin(t, meshSyncJitter+10*time.Second, "coder calls reviewer after it came back", func() error {
		_, err := call(t.Context(), coder.client, reviewer.ep.PrivateURL+"/hello")
		return err
	})
}

func TestRelayLetsBackAnAgentRemovedBeforeARestart(t *testing.T) {
	bin := agentmeshOne(t)
	d := openRelayDir(t, relaydir.Settings{})
	r := startRelay(t, d, bin)
	req := joinRequest("reviewer", r.url, r.invite(t, "reviewer").Token)
	req.StateDir = t.TempDir()
	reviewer := serveAgent(t, req, nil)
	eventually(t, "members lists reviewer", func() error {
		_, err := r.client.Remove(t.Context(), control.RemoveRequest{Target: "reviewer"})
		return err
	})
	reviewer.close(t)
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
		var perr *netprovider.Error
		if err := tryJoin(t, req, nil); !errors.As(err, &perr) || perr.Kind != netprovider.KindRemoved {
			return fmt.Errorf("joining with the removed credential: error = %v, want the agent removed", err)
		}
		return nil
	})

	inv := r.invite(t, "reviewer")
	if inv.LetBack == nil {
		t.Fatal("Client.Invite() let back no agent, want reviewer")
	}
	mistyped := req
	mistyped.Token = inv.Token[1:]
	for range 2 {
		if e := wantKind(t, tryJoin(t, mistyped, nil), netprovider.KindToken); e.Problem == "" {
			t.Fatalf("join with a mistyped invite error = %v, want a problem", e)
		}
	}
	req.Token = inv.Token
	back := serveAgent(t, req, nil)
	if back.ep.PrivateURL != reviewer.ep.PrivateURL {
		t.Fatalf("reviewer's private URL after it came back = %s, want %s", back.ep.PrivateURL, reviewer.ep.PrivateURL)
	}
}

func TestRelayKeepsAnAgentWhoseInviteIsRefused(t *testing.T) {
	bin := agentmeshOne(t)
	r := startRelay(t, openRelayDir(t, relaydir.Settings{}), bin)
	inv := r.invite(t, "reviewer")
	req := joinRequest("reviewer", r.url, inv.Token)
	req.StateDir = t.TempDir()
	reviewer := serveAgent(t, req, nil)
	reviewer.close(t)
	// An agent that joined before a2a-serve recorded its invite.
	if err := os.Remove(filepath.Join(req.StateDir, inviteHashFile)); err != nil {
		t.Fatal(err)
	}

	const fellBack = "! relay: the relay doesn't know this invite; check that it was copied whole; the agent joined with the credential of its last invite"
	tests := []struct {
		token       string
		wantNotices []string
	}{
		{token: inv.Token},
		{token: "not-an-invite", wantNotices: []string{fellBack}},
		{token: "not-an-invite", wantNotices: []string{fellBack}},
		{token: inv.Token},
	}
	for _, tc := range tests {
		req.Token = tc.token
		var mu sync.Mutex
		var notices []string
		back := serveAgent(t, req, func(m string) {
			mu.Lock()
			defer mu.Unlock()
			notices = append(notices, m)
		})
		if back.ep.PrivateURL != reviewer.ep.PrivateURL {
			t.Fatalf("reviewer's private URL after a start with token %q = %s, want %s", tc.token, back.ep.PrivateURL, reviewer.ep.PrivateURL)
		}
		back.close(t)
		mu.Lock()
		got := notices
		mu.Unlock()
		if diff := cmp.Diff(tc.wantNotices, got); diff != "" {
			t.Fatalf("notices of a start with token %q wrong result (-want +got) diff = %s", tc.token, diff)
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

// notice opens the sign-in links an agent prints.
func (b *browser) notice(line string) {
	_, rest, ok := strings.Cut(line, "→ sign in to the relay "+b.relay+": ")
	if !ok {
		return
	}
	link, _, _ := strings.Cut(rest, " (code ")
	b.links.Add(1)
	go b.approve(link)
}

func (b *browser) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	b.t.Log(line)
	b.notice(line)
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

func signInRelayDir(t *testing.T, g *googletest.Google) relaydir.Dir {
	t.Helper()
	return openRelayDir(t, relaydir.Settings{AllowEmails: []string{"*@example.com"}, GoogleClientID: "relay-app", GoogleClientSecret: g.Secret})
}

func TestRelaySignIn(t *testing.T) {
	bin := agentmeshOne(t)
	g := googletest.New(t, googletest.Account{Subject: "anna-id", Email: "anna@example.com", Verified: true, HostedDomain: "example.com"})
	d := signInRelayDir(t, g)
	r := startRelayWith(t, d, bin, g.Endpoints())
	b := &browser{t: t, relay: r.url, hc: &http.Client{Timeout: 30 * time.Second}}
	stateDir := t.TempDir()
	login := &relayLogin{RelayURL: r.url, StateDir: stateDir, HTTP: b.hc, Logf: b.logf, Poll: time.Second}

	idToken, err := login.Token(t.Context())
	if err != nil {
		t.Fatalf("relayLogin.Token() error = %v", err)
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

	coderReq := joinRequest("coder", r.url, "")
	coderReq.StateDir = stateDir
	coder := serveAgent(t, coderReq, b.notice)
	reviewer := joinAgent(t, r, "reviewer")

	if got := b.links.Load(); got != 1 {
		t.Fatalf("sign-in links printed after the agent joined = %d, want 1: it joins with its refresh token", got)
	}
	if coder.joined.Owner != "anna@example.com" {
		t.Fatalf("join owner of the signed-in coder = %q, want anna@example.com", coder.joined.Owner)
	}
	eventually(t, "the invited reviewer calls the signed-in coder", func() error {
		got, err := call(t.Context(), reviewer.client, coder.ep.PrivateURL+"/hello")
		if err != nil {
			return err
		}
		want := whoCalled{Name: "coder", Caller: netprovider.Caller{Node: reviewer.joined.Node, Machine: reviewer.joined.Node, ClientKey: "peer:" + reviewer.joined.Node}}
		if diff := cmp.Diff(want, got); diff != "" {
			return fmt.Errorf("call wrong result (-want +got) diff = %s", diff)
		}
		return nil
	})
	eventually(t, "the signed-in coder calls the reviewer as anna", func() error {
		got, err := call(t.Context(), coder.client, reviewer.ep.PrivateURL+"/hello")
		if err != nil {
			return err
		}
		want := whoCalled{Name: "reviewer", Caller: netprovider.Caller{Node: coder.joined.Node, Machine: coder.joined.Node, Email: "anna@example.com", ClientKey: "peer:" + coder.joined.Node}}
		if diff := cmp.Diff(want, got); diff != "" {
			return fmt.Errorf("call wrong result (-want +got) diff = %s", diff)
		}
		return nil
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
	// While the relay refuses anna, a running coder would reconnect and print
	// a sign-in link, which the browser fails to approve.
	coder.close(t)

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

func TestRelayLetsBackASignedInAgentRemovedBeforeARestart(t *testing.T) {
	bin := agentmeshOne(t)
	g := googletest.New(t, googletest.Account{Subject: "anna-id", Email: "anna@example.com", Verified: true, HostedDomain: "example.com"})
	d := signInRelayDir(t, g)
	r := startRelayWith(t, d, bin, g.Endpoints())
	b := &browser{t: t, relay: r.url, hc: &http.Client{Timeout: 30 * time.Second}}
	req := joinRequest("coder", r.url, "")
	req.StateDir = t.TempDir()
	var mu sync.Mutex
	var notices []string
	coder := serveAgent(t, req, func(m string) {
		mu.Lock()
		notices = append(notices, m)
		mu.Unlock()
		b.notice(m)
	})
	eventually(t, "members lists coder", func() error {
		_, err := r.client.Remove(t.Context(), control.RemoveRequest{Target: "coder"})
		return err
	})
	coder.close(t)
	if err := r.stop(); err != nil {
		t.Fatalf("relay.Run() error = %v", err)
	}
	mu.Lock()
	first := slices.Clone(notices)
	mu.Unlock()
	wantFirst := []string{
		"… joining the relay " + r.url + "; this needs a sign-in on the first run (or --relay-token)",
		"✓ signed in to the relay as anna@example.com",
	}
	if len(first) != 3 || first[0] != wantFirst[0] || !strings.HasPrefix(first[1], "→ sign in to the relay "+r.url+": ") || first[2] != wantFirst[1] {
		t.Fatalf("notices of the first join = %q, want %q around the sign-in link", first, wantFirst)
	}

	// The restarted router refuses the coder's credential for a day when it
	// syncs before the relay lets the coder back, which is likely under load.
	r = startRelayWith(t, d, bin, g.Endpoints())
	back := serveAgent(t, req, b.notice)
	if back.ep.PrivateURL != coder.ep.PrivateURL {
		t.Fatalf("coder's private URL after it came back = %s, want %s", back.ep.PrivateURL, coder.ep.PrivateURL)
	}
	if got := b.links.Load(); got != 1 {
		t.Fatalf("sign-in links printed after coder came back = %d, want 1: it signs in again with its refresh token", got)
	}
}
