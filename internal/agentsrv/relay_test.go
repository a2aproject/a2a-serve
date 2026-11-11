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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/cardinfo"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/messenger/messengertest"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

func TestRelaySameOwnerCallsAndApprovesWithoutPairingCode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fabric := newTestRelayFabric(t, "Ben@example.com")

	startServer(t, serverOpts{
		name:     "reviewer",
		newRelay: fabric.newRelay,
		configure: func(c *agentdir.Config) {
			c.Network.Private = fabric.url()
			c.Network.Public = fabric.url()
		},
	})
	coder := startServer(t, serverOpts{
		name:      "coder",
		messenger: messengertest.NewMessenger("fake"),
		newRelay:  fabric.newRelay,
		configure: func(c *agentdir.Config) {
			c.Network.Private = fabric.url()
			c.Network.Public = fabric.url()
		},
	})

	coderCtl := controlapi.NewClient(coder.dir)
	st, err := coderCtl.Status(ctx)
	if err != nil {
		t.Fatalf("coderCtl.Status() error = %v, want nil", err)
	}
	if st.Private != "http://agentmesh.local/mesh/peer-coder/a2a/coder" || st.PrivateNote == "" {
		t.Fatalf("coderCtl.Status() private = (%q, %q), want agentmesh.local URL and note", st.Private, st.PrivateNote)
	}
	if st.Public != fabric.url()+"/ingress/peer-coder/a2a/coder" {
		t.Fatalf("coderCtl.Status() public = %q, want %q", st.Public, fabric.url()+"/ingress/peer-coder/a2a/coder")
	}

	peers, err := coderCtl.Peers(ctx)
	if err != nil {
		t.Fatalf("coderCtl.Peers() error = %v, want nil", err)
	}
	wantPeers := []controlapi.Peer{{
		Name:        "reviewer",
		URL:         "http://agentmesh.local/mesh/peer-reviewer/a2a/reviewer",
		Description: "reviewer is a fake coding agent reached through `a2a-serve`. Tool calls may pause a task in input-required until whoever started it answers.",
		Online:      true,
	}}
	if diff := cmp.Diff(wantPeers, peers); diff != "" {
		t.Fatalf("coderCtl.Peers() wrong result (-want +got) diff = %s", diff)
	}

	contact, err := coderCtl.AddContact(ctx, controlapi.ContactRequest{Name: "reviewer"})
	if err != nil {
		t.Fatalf("coderCtl.AddContact() error = %v, want nil", err)
	}
	wantContact := &controlapi.ContactInfo{
		Name:     "reviewer",
		URL:      "http://agentmesh.local/mesh/peer-reviewer/a2a/reviewer",
		Via:      contacts.ViaPrivate,
		HasToken: false,
		// The card text is plain: the backticks of the description are left out.
		Description: "reviewer is a fake coding agent reached through a2a-serve. Tool calls may pause a task in input-required until whoever started it answers.",
		Skills:      []cardinfo.Skill{{ID: "coding", Name: "Coding agent", Description: "Works in its project workspace with fake."}},
	}
	if diff := cmp.Diff(wantContact, contact); diff != "" {
		t.Fatalf("coderCtl.AddContact() wrong result (-want +got) diff = %s", diff)
	}

	ownerClient := coder.client(coder.pair("me", auth.RoleOwner))
	task := send(t, ownerClient, userText("call reviewer hello", "", ""))
	wantResult(t, task, a2a.TaskStateCompleted, "Hello from fakeacp.")

	linkOwner(t, coder, "telegram:1", "dm1")
	coder.ch.Write("telegram:1", "dm1", "call reviewer ask")
	ref, q := coder.ch.WaitFor(t, "dm1", messengertest.HasQuestion)
	if !strings.Contains(q.Text, "reviewer wants to run git push") {
		t.Fatalf("relayed question = %q, want reviewer and git push", q.Text)
	}
	coder.ch.Click("telegram:1", "dm1", ref, q.Question, 0)
	coder.ch.WaitFor(t, "dm1", messengertest.HasText("✓ Allow once by owner"))

	contextID, err := coder.srv.store.ContextOf(ctx, "fake", "dm1")
	if err != nil {
		t.Fatalf("store.ContextOf() error = %v, want nil", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		last, err := coder.srv.tasks.LatestInContext(ctx, contextID)
		if err != nil {
			t.Fatalf("tasks.LatestInContext() error = %v, want nil", err)
		}
		if len(last.History) > 0 && strings.Contains(a2atext.Message(last.History[0]), "Pushed (allow).") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("latest turn history = %v, want reviewer's result", last.History)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRelaySignInAdmitsEveryoneOnTheRelay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fabric := newTestRelayFabric(t, "Ben@example.com")
	fabric.setAccountForAgent("anna", "anna@example.com")

	startServer(t, serverOpts{
		name:     "open-reviewer",
		newRelay: fabric.newRelay,
		configure: func(c *agentdir.Config) {
			c.Network.Private = fabric.url()
		},
	})
	startServer(t, serverOpts{
		name:     "closed-reviewer",
		newRelay: fabric.newRelay,
		configure: func(c *agentdir.Config) {
			c.Network.Private = fabric.url()
			c.Policy.PrivateAllowIncoming = agentdir.AccessPaired
		},
	})
	anna := startServer(t, serverOpts{
		name:     "anna",
		newRelay: fabric.newRelay,
		configure: func(c *agentdir.Config) {
			c.Network.Private = fabric.url()
		},
	})

	annaCtl := controlapi.NewClient(anna.dir)
	for _, target := range []string{"closed-reviewer", "open-reviewer"} {
		if _, err := annaCtl.AddContact(ctx, controlapi.ContactRequest{Name: target}); err != nil {
			t.Fatalf("annaCtl.AddContact(%q) error = %v, want nil", target, err)
		}
	}

	annaClient := anna.client(anna.pair("anna-local", auth.RoleOwner))
	allowed := send(t, annaClient, userText("call open-reviewer hello", "", ""))
	wantResult(t, allowed, a2a.TaskStateCompleted, "Hello from fakeacp.")

	denied := send(t, annaClient, userText("call closed-reviewer hello", "", ""))
	wantResult(t, denied, a2a.TaskStateCompleted, "this agent doesn't accept anna, an agent of anna@example.com on the relay")
	for _, step := range []string{"`a2a-serve pair invite anna`", "`a2a-serve contact add closed-reviewer --url http://agentmesh.local/mesh/peer-closed-reviewer/a2a/closed-reviewer --code <code>`"} {
		if got := a2atext.Result(denied); !strings.Contains(got, step) {
			t.Fatalf("task result = %q, want it to contain %q", got, step)
		}
	}
	if got := a2atext.Result(denied); strings.Contains(got, "pair join") {
		t.Fatalf("task result = %q, want no pair join step for an agent", got)
	}
}

func TestRelayPeersDropStoppedAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fabric := newTestRelayFabric(t, "Ben@example.com")
	relayed := func(c *agentdir.Config) { c.Network.Private = fabric.url() }
	reviewer := startServer(t, serverOpts{name: "reviewer", newRelay: fabric.newRelay, configure: relayed})
	coder := startServer(t, serverOpts{name: "coder", newRelay: fabric.newRelay, configure: relayed})
	coderCtl := controlapi.NewClient(coder.dir)
	peers, err := coderCtl.Peers(ctx)
	if err != nil {
		t.Fatalf("coderCtl.Peers() error = %v, want nil", err)
	}
	if len(peers) != 1 || peers[0].Name != "reviewer" {
		t.Fatalf("coderCtl.Peers() = %v, want reviewer", peers)
	}

	reviewer.stop()

	waitFor(t, "peers to drop the stopped reviewer", func() bool {
		peers, err := coderCtl.Peers(ctx)
		if err != nil {
			t.Fatalf("coderCtl.Peers() error = %v, want nil", err)
		}
		return len(peers) == 0
	})
}

// joinFailingRelay is a relay that refuses the agent's invite.
type joinFailingRelay struct {
	network.Provider
	err error
}

func (r joinFailingRelay) Name() string               { return network.ProviderAgentMesh }
func (r joinFailingRelay) Join(context.Context) error { return r.err }
func (r joinFailingRelay) Close() error               { return nil }

func (r joinFailingRelay) ListenPrivate(context.Context) (net.Listener, *url.URL, error) {
	return nil, nil, r.err
}

func TestRelayRefusedInviteFailsBeforeAgentStarts(t *testing.T) {
	t.Parallel()
	dir, err := agentdir.Open(testutil.ShortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &agentdir.Config{Name: "coder", Harness: "fake", Command: []string{fakeACP}, Sandbox: harness.SandboxLocal, Port: freePort(t)}
	if err := dir.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("joining the relay: the invite was already used")
	out := &lockedBuffer{}

	err = Run(context.Background(), Options{Dir: dir, Config: cfg, Secrets: agentdir.Secrets{}, Private: joinFailingRelay{err: refused}, Out: out, HarnessLog: io.Discard, Timings: testTimings})

	if !errors.Is(err, refused) {
		t.Fatalf("Run() error = %v, want %v", err, refused)
	}
	if strings.Contains(out.String(), "is running") {
		t.Fatalf("Run() output = %q, want the agent not started", out.String())
	}
}

func TestRelayTokenJoinOpensPrivateNetworkAndServesPublicIngress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fabric := newTestRelayFabric(t, "")

	reviewer := startServer(t, serverOpts{
		name:     "reviewer",
		secrets:  agentdir.Secrets{agentdir.SecretNetworkToken: "join-secret"},
		newRelay: fabric.newRelay,
		configure: func(c *agentdir.Config) {
			c.Network.Private = fabric.url()
			c.Network.Public = fabric.url()
		},
	})
	coder := startServer(t, serverOpts{
		name:     "coder",
		secrets:  agentdir.Secrets{agentdir.SecretNetworkToken: "join-secret"},
		newRelay: fabric.newRelay,
		configure: func(c *agentdir.Config) {
			c.Network.Private = fabric.url()
			c.Network.Public = fabric.url()
		},
	})

	coderCtl := controlapi.NewClient(coder.dir)
	if _, err := coderCtl.AddContact(ctx, controlapi.ContactRequest{Name: "reviewer"}); err != nil {
		t.Fatalf("coderCtl.AddContact(unpaired) error = %v, want nil", err)
	}
	ownerClient := coder.client(coder.pair("me", auth.RoleOwner))
	unpaired := send(t, ownerClient, userText("call reviewer hello", "", ""))
	wantResult(t, unpaired, a2a.TaskStateCompleted, "Hello from fakeacp.")

	reviewerCtl := controlapi.NewClient(reviewer.dir)

	_, _, pubURL := reviewer.srv.URLs()
	cardResp, err := testHTTP.Get(pubURL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("testHTTP.Get(public card) error = %v, want nil", err)
	}
	if err := cardResp.Body.Close(); err != nil {
		t.Fatalf("cardResp.Body.Close() error = %v, want nil", err)
	}
	if cardResp.StatusCode != http.StatusOK {
		t.Fatalf("testHTTP.Get(public card) status = %d, want %d", cardResp.StatusCode, http.StatusOK)
	}

	unauthResp, err := testHTTP.Post(pubURL+"/", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("testHTTP.Post(public unauthenticated) error = %v, want nil", err)
	}
	if err := unauthResp.Body.Close(); err != nil {
		t.Fatalf("unauthResp.Body.Close() error = %v, want nil", err)
	}
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("testHTTP.Post(public unauthenticated) status = %d, want %d", unauthResp.StatusCode, http.StatusUnauthorized)
	}

	extCode, err := reviewerCtl.Pair(ctx, controlapi.PairCodeRequest{Name: "external"})
	if err != nil {
		t.Fatalf("reviewerCtl.Pair(external) error = %v, want nil", err)
	}
	extCred, err := credstore.Pair(ctx, testHTTP, pubURL, extCode.Code, pairapi.PairingPerson)
	if err != nil {
		t.Fatalf("credstore.Pair() error = %v, want nil", err)
	}
	extHTTP := contacts.TokenClient(testHTTP, store.Contact{URL: pubURL, Token: extCred.Token})
	card, err := agentcard.NewResolver(extHTTP).Resolve(ctx, pubURL)
	if err != nil {
		t.Fatalf("agentcard.Resolve(public) error = %v, want nil", err)
	}
	extClient, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(extHTTP), a2aclient.WithRESTTransport(extHTTP))
	if err != nil {
		t.Fatalf("a2aclient.NewFromCard(public) error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := extClient.Destroy(); err != nil {
			t.Errorf("extClient.Destroy() error = %v", err)
		}
	})
	extTask := send(t, extClient, userText("hello", "", ""))
	wantResult(t, extTask, a2a.TaskStateCompleted, "Hello from fakeacp.")
}

func TestRelayStatusAndPeersShowTheConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fabric := newTestRelayFabric(t, "")
	coder := startServer(t, serverOpts{
		name:     "coder",
		secrets:  agentdir.Secrets{agentdir.SecretNetworkToken: "join-secret"},
		newRelay: fabric.newRelay,
		configure: func(c *agentdir.Config) {
			c.Network.Private = fabric.url()
			c.Network.Public = fabric.url()
		},
	})
	node := fabric.node("peer-coder")
	ctl := controlapi.NewClient(coder.dir)
	_, privateURL, publicURL := coder.srv.URLs()

	disconnected := network.Health{State: network.HealthDisconnected, Problem: "not connected to the relay", Fix: "check the relay"}
	removed := network.Health{State: network.HealthRemoved, Problem: "the relay removed this agent", Fix: "ask the relay's operator"}
	tests := []struct {
		name    string
		health  network.Health
		want    controlapi.Status
		wantErr string
	}{
		{
			name:    "disconnected",
			health:  disconnected,
			want:    controlapi.Status{Private: privateURL, Public: publicURL, PrivateProblem: networkProblem(disconnected), PublicProblem: networkProblem(disconnected)},
			wantErr: "not connected to the relay",
		},
		{
			name:    "removed",
			health:  removed,
			want:    controlapi.Status{PrivateProblem: networkProblem(removed), PublicProblem: networkProblem(removed)},
			wantErr: "the relay removed this agent",
		},
		{
			name: "connected",
			want: controlapi.Status{Private: privateURL, Public: publicURL},
		},
	}
	for _, tc := range tests {
		node.setHealth(tc.health)
		st, err := ctl.Status(ctx)
		if err != nil {
			t.Fatalf("%s: ctl.Status() error = %v, want nil", tc.name, err)
		}
		got := controlapi.Status{Private: st.Private, Public: st.Public, PrivateProblem: st.PrivateProblem, PublicProblem: st.PublicProblem}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Fatalf("%s: ctl.Status() wrong result (-want +got) diff = %s", tc.name, diff)
		}
		_, err = ctl.Peers(ctx)
		if tc.wantErr == "" && err != nil {
			t.Fatalf("%s: ctl.Peers() error = %v, want nil", tc.name, err)
		}
		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Fatalf("%s: ctl.Peers() error = %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

// testRelayHost is the host of private URLs on the fake relay.
const testRelayHost = "agentmesh.local"

type testRelayFabric struct {
	t            *testing.T
	srv          *httptest.Server
	defaultEmail string

	mu            sync.RWMutex
	accountByName map[string]string
	nodes         map[string]*testRelayNode
	seq           atomic.Int64
}

func newTestRelayFabric(t *testing.T, defaultEmail string) *testRelayFabric {
	t.Helper()
	f := &testRelayFabric{
		t:             t,
		defaultEmail:  defaultEmail,
		accountByName: map[string]string{},
		nodes:         map[string]*testRelayNode{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ingress/", f.handleIngress)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *testRelayFabric) url() string { return f.srv.URL }

func (f *testRelayFabric) node(peerID string) *testRelayNode {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.nodes[peerID]
}

func (f *testRelayFabric) setAccountForAgent(agentName, email string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accountByName[agentName] = email
}

func (f *testRelayFabric) handleIngress(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "ingress" || parts[2] != "a2a" {
		http.NotFound(w, r)
		return
	}
	peerID := parts[1]
	rest := "/"
	if len(parts) > 4 {
		rest = "/" + strings.Join(parts[4:], "/")
	}
	f.mu.RLock()
	target := f.nodes[peerID]
	f.mu.RUnlock()
	if target == nil || target.pubLn == nil {
		http.NotFound(w, r)
		return
	}
	target.servePipe(w, r, rest, target.pubLn, nil)
}

func (f *testRelayFabric) newRelay(cfg network.ExternalConfig) (network.Provider, error) {
	// Tests start and stop agents faster than the cards would expire.
	cards := network.NewCardCache(0, 0, time.Now)
	return &testRelayNode{fabric: f, cfg: cfg, peerID: "peer-" + cfg.AgentName, cards: cards}, nil
}

type testRelayNode struct {
	fabric    *testRelayFabric
	cfg       network.ExternalConfig
	peerID    string
	principal string
	owner     string

	startOnce sync.Once
	startErr  error
	privLn    *relayPipeListener
	pubLn     *relayPipeListener
	conns     sync.Map

	healthMu sync.Mutex
	health   network.Health
	cards    *network.CardCache
}

func (n *testRelayNode) setHealth(h network.Health) {
	n.healthMu.Lock()
	defer n.healthMu.Unlock()
	n.health = h
}

func (n *testRelayNode) Health() network.Health {
	n.healthMu.Lock()
	defer n.healthMu.Unlock()
	return n.health
}

func (n *testRelayNode) Name() string { return network.ProviderAgentMesh }

func (n *testRelayNode) start(ctx context.Context) error {
	n.startOnce.Do(func() {
		// Without a token, the agent signs in to the relay with the account
		// of its person.
		if n.cfg.Token == "" {
			n.fabric.mu.RLock()
			email := n.fabric.accountByName[n.cfg.AgentName]
			if email == "" {
				email = n.fabric.defaultEmail
			}
			n.fabric.mu.RUnlock()
			n.principal, n.owner = email, strings.ToLower(email)
			if n.cfg.Logf != nil {
				n.cfg.Logf("✓ signed in to the relay as %s", n.owner)
			}
		} else {
			n.principal = "node:" + n.peerID
		}
		if n.cfg.Private {
			n.privLn = newRelayPipeListener("mesh-private:" + n.peerID)
		}
		if n.cfg.Public {
			n.pubLn = newRelayPipeListener("mesh-public:" + n.peerID)
		}
		n.fabric.mu.Lock()
		n.fabric.nodes[n.peerID] = n
		n.fabric.mu.Unlock()
	})
	return n.startErr
}

func (n *testRelayNode) ListenPrivate(ctx context.Context) (net.Listener, *url.URL, error) {
	if err := n.start(ctx); err != nil {
		return nil, nil, err
	}
	if n.privLn == nil {
		return nil, nil, network.ErrUnsupported
	}
	u, err := url.Parse(fmt.Sprintf("http://%s/mesh/%s/a2a/%s", testRelayHost, n.peerID, n.cfg.AgentName))
	return n.privLn, u, err
}

func (n *testRelayNode) ListenPublic(ctx context.Context) (net.Listener, *url.URL, error) {
	if err := n.start(ctx); err != nil {
		return nil, nil, err
	}
	if n.pubLn == nil {
		return nil, nil, errors.New("no public listener")
	}
	u, err := url.Parse(fmt.Sprintf("%s/ingress/%s/a2a/%s", strings.TrimSuffix(n.cfg.URL, "/"), n.peerID, n.cfg.AgentName))
	return n.pubLn, u, err
}

func (n *testRelayNode) WhoIs(_ context.Context, remoteAddr string) (*network.Peer, error) {
	v, ok := n.conns.Load(remoteAddr)
	if !ok {
		return nil, network.ErrNoIdentity
	}
	caller := v.(*testRelayNode)
	p := &network.Peer{Node: caller.peerID, Machine: caller.peerID}
	if network.IsEmail(caller.principal) {
		p.Email = caller.principal
		p.Self = n.owner != "" && strings.EqualFold(caller.principal, n.owner)
	}
	return p, nil
}

func (n *testRelayNode) ClientKey(remoteAddr string, _ bool) string {
	v, ok := n.conns.Load(remoteAddr)
	if !ok {
		return ""
	}
	return "peer:" + v.(*testRelayNode).peerID
}

func (n *testRelayNode) PrivateNote() string {
	return "works only from other a2a-serve agents on the relay; give people the public URL"
}

func (n *testRelayNode) Member(context.Context, string) (*network.Peer, error) {
	return nil, network.ErrUnsupported
}

func (n *testRelayNode) Peers(ctx context.Context) ([]network.PeerInfo, error) {
	n.fabric.mu.RLock()
	var candidates []network.PeerInfo
	for _, other := range n.fabric.nodes {
		if other.peerID == n.peerID || other.privLn == nil {
			continue
		}
		candidates = append(candidates, network.PeerInfo{
			Name:   other.cfg.AgentName,
			URL:    fmt.Sprintf("http://%s/mesh/%s/a2a/%s", testRelayHost, other.peerID, other.cfg.AgentName),
			Online: true,
			Node:   other.peerID,
		})
	}
	n.fabric.mu.RUnlock()
	return n.cards.ProbeAgents(ctx, n.HTTPClient(), candidates, []string{n.cfg.AgentName}), nil
}

func (n *testRelayNode) HTTPClient() *http.Client {
	return &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL == nil || !strings.EqualFold(r.URL.Host, testRelayHost) {
			return testHTTP.Transport.RoundTrip(r)
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) < 4 || parts[0] != "mesh" || parts[2] != "a2a" {
			return nil, fmt.Errorf("unexpected mesh path %q", r.URL.Path)
		}
		peerID := parts[1]
		rest := "/"
		if len(parts) > 4 {
			rest = "/" + strings.Join(parts[4:], "/")
		}
		n.fabric.mu.RLock()
		target := n.fabric.nodes[peerID]
		n.fabric.mu.RUnlock()
		if target == nil || target.privLn == nil {
			return nil, fmt.Errorf("unknown peer %q", peerID)
		}
		return target.roundTripPipe(r, rest, target.privLn, n)
	})}
}

func (n *testRelayNode) Close() error {
	var errs []error
	if n.privLn != nil {
		errs = append(errs, n.privLn.Close())
	}
	if n.pubLn != nil {
		errs = append(errs, n.pubLn.Close())
	}
	return errors.Join(errs...)
}

func (n *testRelayNode) servePipe(w http.ResponseWriter, r *http.Request, restPath string, ln *relayPipeListener, caller *testRelayNode) {
	remoteKey := fmt.Sprintf("relay-conn-%d", n.fabric.seq.Add(1))
	if caller != nil {
		n.conns.Store(remoteKey, caller)
	}
	c1, c2 := net.Pipe()
	srvConn := &relayAddrConn{
		Conn:   c2,
		local:  ln.Addr(),
		remote: relayPipeAddr(remoteKey),
		onClose: func() {
			if caller != nil {
				n.conns.Delete(remoteKey)
			}
		},
	}
	var dialOnce sync.Once
	dialUsed := false
	tr := &http.Transport{
		DisableCompression: true,
		DialContext: func(dCtx context.Context, _, _ string) (net.Conn, error) {
			var out net.Conn
			var dErr error
			dialOnce.Do(func() {
				dialUsed = true
				if err := ln.deliver(dCtx, srvConn); err != nil {
					dErr = errors.Join(err, c1.Close())
					if caller != nil {
						n.conns.Delete(remoteKey)
					}
					return
				}
				out = c1
			})
			if dErr != nil {
				return nil, dErr
			}
			if out == nil {
				return nil, errors.New("relay pipe connection already consumed")
			}
			return out, nil
		},
	}
	defer func() {
		tr.CloseIdleConnections()
		if !dialUsed {
			if err := errors.Join(c1.Close(), srvConn.Close()); err != nil {
				n.fabric.t.Logf("closing unused pipe: %v", err)
			}
			if caller != nil {
				n.conns.Delete(remoteKey)
			}
		}
	}()
	targetURL := &url.URL{Scheme: "http", Host: "relay.internal"}
	proxy := &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(targetURL)
			pr.Out.Host = pr.In.Host
			pr.Out.URL.Path = restPath
			pr.Out.URL.RawPath = ""
		},
	}
	proxy.ServeHTTP(w, r)
}

func (n *testRelayNode) roundTripPipe(r *http.Request, restPath string, ln *relayPipeListener, caller *testRelayNode) (*http.Response, error) {
	remoteKey := fmt.Sprintf("relay-conn-%d", n.fabric.seq.Add(1))
	if caller != nil {
		n.conns.Store(remoteKey, caller)
	}
	c1, c2 := net.Pipe()
	srvConn := &relayAddrConn{
		Conn:   c2,
		local:  ln.Addr(),
		remote: relayPipeAddr(remoteKey),
		onClose: func() {
			if caller != nil {
				n.conns.Delete(remoteKey)
			}
		},
	}
	if err := ln.deliver(r.Context(), srvConn); err != nil {
		if caller != nil {
			n.conns.Delete(remoteKey)
		}
		return nil, errors.Join(err, c1.Close())
	}
	var dialOnce sync.Once
	tr := &http.Transport{
		DisableCompression: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			var out net.Conn
			dialOnce.Do(func() { out = c1 })
			if out == nil {
				return nil, errors.New("relay pipe connection already consumed")
			}
			return out, nil
		},
	}
	defer tr.CloseIdleConnections()
	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = "http"
	outReq.URL.Host = r.URL.Host
	outReq.URL.Path = restPath
	outReq.URL.RawPath = ""
	return tr.RoundTrip(outReq)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type relayPipeAddr string

func (a relayPipeAddr) Network() string { return "pipe" }
func (a relayPipeAddr) String() string  { return string(a) }

type relayPipeListener struct {
	addr   relayPipeAddr
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newRelayPipeListener(label string) *relayPipeListener {
	return &relayPipeListener{
		addr:   relayPipeAddr(label),
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (l *relayPipeListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	case c := <-l.conns:
		return c, nil
	}
}

func (l *relayPipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *relayPipeListener) Addr() net.Addr { return l.addr }

func (l *relayPipeListener) deliver(ctx context.Context, c net.Conn) error {
	select {
	case <-l.closed:
		return errors.Join(net.ErrClosed, c.Close())
	case <-ctx.Done():
		return errors.Join(ctx.Err(), c.Close())
	case l.conns <- c:
		return nil
	}
}

type relayAddrConn struct {
	net.Conn
	local   net.Addr
	remote  net.Addr
	once    sync.Once
	onClose func()
}

func (c *relayAddrConn) LocalAddr() net.Addr  { return c.local }
func (c *relayAddrConn) RemoteAddr() net.Addr { return c.remote }

func (c *relayAddrConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
	})
	return err
}
