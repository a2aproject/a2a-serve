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
	"fmt"
	"os"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-serve/internal/a2aproxy"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
)

// ProxyLogInterceptorOption returns a client factory option that logs proxied
// calls to stderr. Callers should apply it when building the upstream client so
// that proxied requests are traced unless running in quiet mode.
func ProxyLogInterceptorOption() a2aclient.FactoryOption {
	return a2aclient.WithCallInterceptors(&proxyLogInterceptor{})
}

// ServeProxy runs a local A2A server that forwards every request to the given
// upstream client, exposing the resolved (or supplied) agent card.
func ServeProxy(ctx context.Context, cfg Config, svcParams *flagparse.ServiceParams, client *a2aclient.Client, tenant string) error {
	card, err := resolveProxyCard(ctx, cfg, client, tenant)
	if err != nil {
		return err
	}

	handler := &a2aproxy.ClientHandler{Client: client, Context: func(ctx context.Context) context.Context {
		if params := svcParams.Params(); len(params) > 0 {
			return a2aclient.AttachServiceParams(ctx, params)
		}
		return ctx
	}}

	if !cfg.Quiet {
		fmt.Fprintf(os.Stderr, "Proxying to %q agent\n", card.Name)
	}

	cfg.Logger("proxy mode, transport=%s protocol=%s", cfg.Transport, cfg.ProtocolVersion)

	return serve(ctx, cfg, handler, card)
}

func resolveProxyCard(ctx context.Context, cfg Config, client *a2aclient.Client, tenant string) (*a2a.AgentCard, error) {
	if cfg.CardPath != "" {
		return createAgentCard(cfg.CardParams)
	}

	upstream := client.Card()
	if upstream == nil {
		fetched, err := client.GetExtendedAgentCard(ctx, &a2a.GetExtendedAgentCardRequest{Tenant: tenant})
		if err != nil {
			return nil, fmt.Errorf("resolving upstream agent card: %w", err)
		}
		upstream = fetched
	}

	cardCopy := *upstream
	cardCopy.SupportedInterfaces = []*a2a.AgentInterface{getAgentInterface(cfg.CardParams)}
	return &cardCopy, nil
}

type proxyLogInterceptor struct {
	a2aclient.PassthroughInterceptor
}

func (i *proxyLogInterceptor) After(_ context.Context, resp *a2aclient.Response) error {
	if resp.Err != nil {
		fmt.Fprintf(os.Stderr, "proxy %s → error: %v\n", resp.Method, resp.Err)
	} else {
		fmt.Fprintf(os.Stderr, "proxy %s → ok\n", resp.Method)
	}
	return nil
}
