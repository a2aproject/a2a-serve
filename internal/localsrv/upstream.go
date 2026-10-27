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
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	a2agrpcv0 "github.com/a2aproject/a2a-go/v2/a2agrpc/v0"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v1"

	"github.com/a2aproject/a2a-serve/internal/flagparse"
)

// UpstreamClient resolves the agent card at cardURL, sending svcParams as
// request headers, and returns a client for the agent over any transport
// of A2A v1.0 or v0.3 it declares. insecureGRPC allows plaintext gRPC.
func UpstreamClient(ctx context.Context, cardURL string, svcParams *flagparse.ServiceParams, insecureGRPC bool, opts ...a2aclient.FactoryOption) (*a2aclient.Client, error) {
	resolver := agentcard.NewResolver(&http.Client{Timeout: 30 * time.Second})
	resolver.CardParser = a2av0.NewAgentCardParser()
	var resolveOpts []agentcard.ResolveOption
	for k, vals := range svcParams.Params() {
		resolveOpts = append(resolveOpts, agentcard.WithRequestHeader(k, strings.Join(vals, ", ")))
	}
	card, err := resolver.Resolve(ctx, cardURL, resolveOpts...)
	if err != nil {
		return nil, fmt.Errorf("resolving the agent card of %s: %w", cardURL, err)
	}

	var grpcOpts []grpc.DialOption
	if insecureGRPC {
		grpcOpts = append(grpcOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	// The a2a-go default HTTP client caps every request at 3 minutes; proxied
	// requests are bounded by the caller instead.
	httpClient := &http.Client{}
	factoryOpts := []a2aclient.FactoryOption{
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithRESTTransport(httpClient),
		a2aclient.WithJSONRPCTransport(httpClient),
		a2agrpc.WithGRPCTransport(grpcOpts...),
		a2av0.WithRESTTransport(a2av0.RESTTransportConfig{Client: httpClient}),
		a2av0.WithJSONRPCTransport(a2av0.JSONRPCTransportConfig{Client: httpClient}),
		a2agrpcv0.WithGRPCTransport(grpcOpts...),
	}
	client, err := a2aclient.NewFromCard(ctx, card, append(factoryOpts, opts...)...)
	if err != nil && strings.Contains(err.Error(), "no transport security set") {
		return nil, fmt.Errorf("%w\n\nhint: pass --insecure to allow plaintext gRPC connections", err)
	}
	return client, err
}
