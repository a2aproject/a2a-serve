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
	"log/slog"
	"net"
	"net/http"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	a2aslimrpc "github.com/agntcy/slim-a2a-go/a2aslimrpc/v1"
	slim_rpc "github.com/agntcy/slim-bindings-go/v3/slim_rpc"

	"github.com/a2aproject/a2a-serve/netprovider"
)

// maxInbound bounds the calls from the node that the agent serves at once.
const maxInbound = 128

// serverURL is where the server serves A2A on its private socket.
const serverURL = "http://" + Host + "/"

// inbound serves the SLIMRPC calls for the agent by sending them to the
// server over A2A JSON-RPC, each on a connection of its own that names the
// caller.
type inbound struct {
	forwarder
	in    *netprovider.Inbound
	self  name
	id    identity
	slots chan struct{}
}

func newInbound(in *netprovider.Inbound, self name, id identity) *inbound {
	h := &inbound{in: in, self: self, id: id, slots: make(chan struct{}, maxInbound)}
	h.open = h.openTransport
	// SLIM callers speak the A2A version of the SLIMRPC service.
	h.params = a2aclient.ServiceParams{a2a.SvcParamVersion: {string(a2a.Version)}}
	return h
}

// caller is who SLIM verified the call comes from.
func (h *inbound) caller(ctx context.Context) (netprovider.Caller, error) {
	c, ok := a2aslimrpc.CallerFromContext(ctx)
	if !ok || c.Subject == "" {
		return netprovider.Caller{}, slim_rpc.NewRpcErrorRpc(slim_rpc.RpcCodeUnauthenticated, "SLIM reported no identity for the caller", nil)
	}
	n := h.id.callerName(c.Subject)
	return netprovider.Caller{Node: n, Machine: n, ClientKey: "slim:" + n}, nil
}

// acquire takes a slot for a call and returns the caller.
func (h *inbound) acquire(ctx context.Context) (netprovider.Caller, func(), error) {
	select {
	case h.slots <- struct{}{}:
	default:
		return netprovider.Caller{}, nil, slim_rpc.NewRpcErrorRpc(slim_rpc.RpcCodeResourceExhausted, "the agent serves too many SLIM calls; try again later", nil)
	}
	release := func() { <-h.slots }
	c, err := h.caller(ctx)
	if err != nil {
		release()
		return netprovider.Caller{}, nil, err
	}
	return c, release, nil
}

// client returns an HTTP client to the server for c. Keep-alives are off,
// so that no connection carries the requests of two callers.
func (h *inbound) client(c netprovider.Caller) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return h.in.Dial(ctx, netprovider.Private, c)
		},
	}}
}

func (h *inbound) openTransport(ctx context.Context) (a2aclient.Transport, func(), error) {
	c, release, err := h.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	t := a2aclient.NewJSONRPCTransport(serverURL, h.client(c))
	return t, func() {
		destroy(t)
		release()
	}, nil
}

// GetExtendedAgentCard returns the server's public card with the agent's
// SLIMRPC interface, so that SLIM clients learn the binding.
func (h *inbound) GetExtendedAgentCard(ctx context.Context, _ *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	c, release, err := h.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	card, err := agentcard.NewResolver(h.client(c)).Resolve(ctx, serverURL)
	if err != nil {
		return nil, err
	}
	card.SupportedInterfaces = append(slices.Clone(card.SupportedInterfaces), a2a.NewAgentInterface(h.self.String(), a2aslimrpc.SLIMProtocol))
	// The added interface would fail the card's signatures.
	card.Signatures = nil
	return card, nil
}

func destroy(t a2aclient.Transport) {
	if err := t.Destroy(); err != nil {
		slog.Debug("closing an A2A transport", "error", err)
	}
}
