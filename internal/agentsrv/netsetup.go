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
	"github.com/a2aproject/a2a-serve/internal/network"
)

// Network provider names.
const (
	ProviderTailscale = network.ProviderTailscale
	ProviderLAN       = network.ProviderLAN
)

// Secrets read by network providers.
const (
	SecretTailscaleAuthKey = "TS_AUTHKEY"
	SecretRelayToken       = "RELAY_TOKEN"
)

// setupNetwork creates the private and public providers from the config.
// They start listening in listenNetwork.
func (s *Server) setupNetwork(closers *stack) error {
	cfg := s.config()
	var ts *network.Tailscale
	tailscale := func() *network.Tailscale {
		if ts == nil {
			ts = &network.Tailscale{
				Hostname:   cfg.Name,
				StateDir:   s.dir.TSNetDir(),
				ControlURL: cfg.Network.TailscaleControlURL,
				AuthKey:    s.secrets()[SecretTailscaleAuthKey],
				Logf:       s.logf,
			}
		}
		return ts
	}
	// One relay serves both URLs when --private and --public name it.
	var relay network.Provider
	relayAt := func(u string) (network.Provider, error) {
		if relay != nil {
			return relay, nil
		}
		newRelay := s.newRelay
		if newRelay == nil {
			newRelay = network.NewRelay
		}
		var err error
		relay, err = newRelay(network.RelayConfig{
			AgentName: cfg.Name, Description: cfg.Description, URL: strings.TrimSuffix(u, "/"),
			StateDir: s.dir.AgentMeshDir(), Token: s.secrets()[SecretRelayToken],
			Private: network.IsRelayURL(cfg.Network.Private), Public: network.IsRelayURL(cfg.Network.Public),
			Logf: s.logf,
		})
		return relay, err
	}
	var err error
	switch {
	case s.private != nil:
	case cfg.Network.Private == "":
	case network.IsRelayURL(cfg.Network.Private):
		if s.private, err = relayAt(cfg.Network.Private); err != nil {
			return err
		}
	case cfg.Network.Private == ProviderTailscale:
		s.private = tailscale()
	case cfg.Network.Private == ProviderLAN:
		s.private = &network.LAN{AgentName: cfg.Name, Port: cfg.Network.LANPort}
	default:
		return fmt.Errorf("unknown private network %q: want tailscale, lan or a relay URL", cfg.Network.Private)
	}
	switch {
	case s.public != nil:
	case cfg.Network.Public == "":
	case network.IsRelayURL(cfg.Network.Public):
		if s.public, err = relayAt(cfg.Network.Public); err != nil {
			return err
		}
	case cfg.Network.Public == ProviderTailscale:
		t := tailscale()
		t.Funnel = true
		s.public = t
	default:
		return fmt.Errorf("unknown public network %q: want tailscale or a relay URL", cfg.Network.Public)
	}
	if s.private != nil {
		closers.push(func(context.Context) error { return s.private.Close() })
	}
	if s.public != nil && s.public != s.private {
		closers.push(func(context.Context) error { return s.public.Close() })
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
		if lan, ok := s.private.(*network.LAN); ok && s.config().Network.LANPort == 0 {
			if err := s.updateConfig(func(c *agentdir.Config) error { c.Network.LANPort = lan.BoundPort(); return nil }); err != nil {
				return err
			}
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
