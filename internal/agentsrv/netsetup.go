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
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/netinstall"
	"github.com/a2aproject/a2a-serve/internal/network"
)

// setupNetwork creates the private and public providers from the config.
// They start listening in listenNetwork.
func (s *Server) setupNetwork(ctx context.Context, closers *stack) error {
	cfg := s.rt.Config()
	var ts *network.Tailscale
	tailscale := func() *network.Tailscale {
		if ts == nil {
			ts = &network.Tailscale{
				Hostname:   cfg.Name,
				StateDir:   s.dir.TSNetDir(),
				ControlURL: cfg.Network.TailscaleControlURL,
				AuthKey:    s.rt.Secrets()[agentdir.SecretTailscaleKey],
				Logf:       s.logf,
			}
		}
		return ts
	}
	// One provider serves both URLs when --private and --public name the
	// network.
	var joined network.Provider
	networkAt := func(u string) (network.Provider, error) {
		if joined != nil {
			return joined, nil
		}
		spec := cfg.Network.ProviderSpec()
		p, err := s.resolveNetworkProvider(ctx, spec)
		if err != nil {
			return nil, err
		}
		newNetwork := s.newNetwork
		if newNetwork == nil {
			newNetwork = func(c network.ExternalConfig) (network.Provider, error) { return network.NewExternal(c), nil }
		}
		joined, err = newNetwork(network.ExternalConfig{
			Name: p.Name, Binary: p.Path,
			AgentName: cfg.Name, Description: cfg.Description, URL: strings.TrimSuffix(u, "/"),
			StateDir: s.dir.NetworkStateDir(p.Name), Token: networkToken(s.rt.Secrets()),
			Private: agentdir.IsNetworkURL(cfg.Network.Private), Public: agentdir.IsNetworkURL(cfg.Network.Public),
			Logf: s.logf,
		})
		return joined, err
	}
	var err error
	switch {
	case s.private != nil:
	case cfg.Network.Private == "":
	case agentdir.IsNetworkURL(cfg.Network.Private):
		if s.private, err = networkAt(cfg.Network.Private); err != nil {
			return err
		}
	case cfg.Network.Private == network.ProviderTailscale:
		s.private = tailscale()
	default:
		return fmt.Errorf("unknown private network %q: want tailscale or a network URL", cfg.Network.Private)
	}
	switch {
	case s.public != nil:
	case cfg.Network.Public == "":
	case agentdir.IsNetworkURL(cfg.Network.Public):
		if s.public, err = networkAt(cfg.Network.Public); err != nil {
			return err
		}
	case cfg.Network.Public == network.ProviderTailscale:
		t := tailscale()
		t.Funnel = true
		s.public = t
	default:
		return fmt.Errorf("unknown public network %q: want tailscale or a network URL", cfg.Network.Public)
	}
	if s.private != nil {
		closers.push(func(context.Context) error { return s.private.Close() })
	}
	if s.public != nil && s.public != s.private {
		closers.push(func(context.Context) error { return s.public.Close() })
	}
	return nil
}

// networkToken is the --network-token, or the --relay-token stored before
// network providers.
func networkToken(secrets agentdir.Secrets) string {
	if t := secrets[agentdir.SecretNetworkToken]; t != "" {
		return t
	}
	return secrets[agentdir.SecretRelayToken]
}

// resolveNetworkProvider finds the binary of the provider spec. A test
// that replaces how providers start without replacing how they are found
// gets the spec as the name.
func (s *Server) resolveNetworkProvider(ctx context.Context, spec string) (netinstall.Provider, error) {
	switch {
	case s.resolveNetwork != nil:
		return s.resolveNetwork(ctx, spec)
	case s.newNetwork != nil:
		return netinstall.Provider{Name: spec}, nil
	}
	inst, err := netinstall.New(s.logf)
	if err != nil {
		return netinstall.Provider{}, err
	}
	return inst.Resolve(ctx, spec)
}

// joinNetwork joins the networks of the providers which join before they
// listen.
func (s *Server) joinNetwork(ctx context.Context) error {
	for _, p := range []network.Provider{s.private, s.public} {
		if j, ok := p.(network.Joiner); ok {
			if err := j.Join(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// listenNetwork opens the private and public listeners.
func (s *Server) listenNetwork(ctx context.Context, closers *stack, errc chan<- error) error {
	if s.private != nil {
		ln, u, err := s.private.ListenPrivate(ctx)
		if err != nil {
			return fmt.Errorf("private network (%s): %w", s.private.Name(), err)
		}
		s.urlMu.Lock()
		s.privateURL = u.String()
		s.urlMu.Unlock()
		closers.push(s.serveHTTP(ctx, ln, s.privateListener(u), errc))
	}
	if s.public != nil {
		ln, u, err := s.public.ListenPublic(ctx)
		if err != nil {
			return fmt.Errorf("public network (%s): %w", s.public.Name(), err)
		}
		s.urlMu.Lock()
		s.publicURL = u.String()
		s.urlMu.Unlock()
		closers.push(s.serveHTTP(ctx, ln, listener{kind: network.ListenerPublic, baseURL: u.String(), tunneled: true}, errc))
	}
	return nil
}
