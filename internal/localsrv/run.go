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

package localsrv

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/a2aproject/a2a-serve/internal/flagparse"
)

// Options configure how a test server is served.
type Options struct {
	Host       string
	Port       int
	Protocol   string
	Transport  string
	CardCompat bool
	Quiet      bool
	Logger     LoggerFunc
	Card       CardParams
}

// ProxyOptions select the agent the proxy test server forwards to.
type ProxyOptions struct {
	// URL is the agent card URL of the agent to forward requests to.
	URL string
	// SvcParams and Tenant are forwarded to the proxied agent, and
	// InsecureGRPC allows reaching it over plaintext gRPC.
	SvcParams    *flagparse.ServiceParams
	Tenant       string
	InsecureGRPC bool
}

// ScriptOptions select the command the script test server runs per message.
type ScriptOptions struct {
	Command string
	// Chunk streams the output split at this delimiter.
	Chunk string
	// Stream streams the output line by line, keeping the line endings.
	Stream bool
}

// RunEcho serves the echo test server until ctx ends.
func RunEcho(ctx context.Context, o Options) error {
	sc, err := listen(o)
	if err != nil {
		return err
	}
	return ServeEcho(ctx, sc)
}

// RunProxy serves the proxy test server until ctx ends.
func RunProxy(ctx context.Context, o Options, p ProxyOptions) error {
	sc, err := listen(o)
	if err != nil {
		return err
	}
	var proxyOpts []a2aclient.FactoryOption
	if !o.Quiet {
		proxyOpts = append(proxyOpts, ProxyLogInterceptorOption())
	}
	client, err := UpstreamClient(ctx, p.URL, p.SvcParams, p.InsecureGRPC, proxyOpts...)
	if err != nil {
		if cerr := sc.Listener.Close(); cerr != nil {
			return errors.Join(fmt.Errorf("creating upstream client: %w", err), cerr)
		}
		return fmt.Errorf("creating upstream client: %w", err)
	}
	return ServeProxy(ctx, sc, p.SvcParams, client, p.Tenant)
}

// RunScript serves the script test server until ctx ends.
func RunScript(ctx context.Context, o Options, s ScriptOptions) error {
	sc, err := listen(o)
	if err != nil {
		return err
	}
	return ServeScript(ctx, sc, s)
}

func listen(o Options) (Config, error) {
	if o.Protocol != "latest" && o.Protocol != "0.3" {
		return Config{}, fmt.Errorf("--protocol must be %q or %q", "latest", "0.3")
	}
	transport, err := flagparse.SingleTransport([]string{o.Transport})
	if err != nil {
		return Config{}, err
	}
	if !isBuiltinTransport(transport) {
		return Config{}, fmt.Errorf("serve %q transport not supported", transport)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(o.Host, fmt.Sprint(o.Port)))
	if err != nil {
		return Config{}, fmt.Errorf("listen: %w", err)
	}
	sc := Config{
		Listener:        listener,
		Logger:          o.Logger,
		ProtocolVersion: a2a.ProtocolVersion(o.Protocol),
		CardCompat:      o.CardCompat,
		Quiet:           o.Quiet,
		CardParams:      o.Card,
	}
	sc.Transport = transport
	if sc.AdvertiseAddress == "" {
		sc.AdvertiseAddress = listener.Addr().String()
	}
	return sc, nil
}

func isBuiltinTransport(protocol a2a.TransportProtocol) bool {
	switch protocol {
	case a2a.TransportProtocolHTTPJSON, a2a.TransportProtocolJSONRPC, a2a.TransportProtocolGRPC:
		return true
	default:
		return false
	}
}
