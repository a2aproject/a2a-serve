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
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/network"
)

// fakeNet is a private network of agents on loopback, where every caller is
// the machine "peer".
type fakeNet struct {
	mu     sync.Mutex
	agents map[string]string
}

type fakeProvider struct {
	net  *fakeNet
	name string
	// kind is the provider name it reports; empty is "fake".
	kind string
	// unidentified makes WhoIs identify nobody.
	unidentified bool
	// signIn makes the network one which admits people by sign-in.
	signIn bool
}

func (p *fakeProvider) AdmitsBySignIn() bool { return p.signIn }

func (n *fakeNet) provider(name string) *fakeProvider { return &fakeProvider{net: n, name: name} }

func (p *fakeProvider) Name() string {
	if p.kind == "" {
		return "fake"
	}
	return p.kind
}

func (p *fakeProvider) ListenPrivate(context.Context) (net.Listener, *url.URL, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	u := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	p.net.mu.Lock()
	p.net.agents[p.name] = u.String()
	p.net.mu.Unlock()
	return ln, u, nil
}

func (p *fakeProvider) ListenPublic(context.Context) (net.Listener, *url.URL, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	return ln, &url.URL{Scheme: "http", Host: ln.Addr().String()}, nil
}

func (p *fakeProvider) WhoIs(context.Context, string) (*network.Peer, error) {
	if p.unidentified {
		return nil, network.ErrNoIdentity
	}
	return &network.Peer{Node: "peer"}, nil
}

func (p *fakeProvider) Member(_ context.Context, node string) (*network.Peer, error) {
	if p.unidentified {
		return nil, network.ErrNoIdentity
	}
	return &network.Peer{Node: node}, nil
}

func (p *fakeProvider) Peers(context.Context) ([]network.PeerInfo, error) {
	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	var out []network.PeerInfo
	for name, u := range p.net.agents {
		if name != p.name {
			out = append(out, network.PeerInfo{Name: name, URL: u, Online: true})
		}
	}
	return out, nil
}

func (p *fakeProvider) HTTPClient() *http.Client { return http.DefaultClient }

func (p *fakeProvider) Close() error { return nil }

func TestAdvertisedPairing(t *testing.T) {
	t.Parallel()
	fnet := &fakeNet{agents: map[string]string{}}
	ben := startServer(t, serverOpts{name: "ben", private: fnet.provider("ben")})
	anna := startServer(t, serverOpts{name: "anna", private: fnet.provider("anna")})
	ctx := context.Background()
	addNode(t, ben, "grok", 0)
	advertise := func(on bool) {
		t.Helper()
		resp, err := controlapi.NewClient(ben.dir).UpdatePairing(ctx, "grok", controlapi.PairingUpdate{Advertised: &on})
		if err != nil || resp.Pairing.Node.Advertised != on {
			t.Fatalf("UpdatePairing(advertised=%v) = %+v, %v", on, resp, err)
		}
	}
	advertise(true)

	anna.srv.gossipRound(ctx)
	ads, err := controlapi.NewClient(anna.dir).Advertisements(ctx)
	if err != nil {
		t.Fatalf("Advertisements() error = %v", err)
	}
	keys := make([]string, len(ads))
	for i, a := range ads {
		keys[i] = a.Key
	}
	if diff := cmp.Diff([]string{"grok@ben"}, keys); diff != "" {
		t.Fatalf("Advertisements() on anna wrong result (-want +got) diff = %s", diff)
	}

	contact, err := controlapi.NewClient(anna.dir).AddContact(ctx, controlapi.ContactRequest{Name: "grok@ben"})
	if err != nil {
		t.Fatalf("AddContact(grok@ben) error = %v", err)
	}
	if contact.URL != ben.srv.privateURL+"/n/grok" {
		t.Fatalf("AddContact(grok@ben).URL = %q, want ben's node on the private network", contact.URL)
	}
	c := anna.client(anna.pair("me", auth.RoleOwner))
	task := send(t, c, userText("call grok@ben what's new?", "", ""))
	wantResult(t, task, a2a.TaskStateCompleted, "grok says: what's new?")

	advertise(false)
	anna.srv.gossipRound(ctx)
	if ads, err := controlapi.NewClient(anna.dir).Advertisements(ctx); err != nil || len(ads) != 0 {
		t.Fatalf("Advertisements() after the withdrawal = %v, %v, want none", ads, err)
	}
	if _, err := controlapi.NewClient(anna.dir).AddContact(ctx, controlapi.ContactRequest{Name: "other@ben"}); err == nil {
		t.Fatalf("AddContact() of a pairing nobody advertises error = nil, want error")
	}
}

func TestAdvertisedNeedsWebhookNode(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	on := true
	if _, err := controlapi.NewClient(ts.dir).Pair(context.Background(), controlapi.PairCodeRequest{Name: "anna", Advertised: &on}); err == nil {
		t.Fatalf("Pair() of a caller with advertised error = nil, want error")
	}
}
