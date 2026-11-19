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

package serve

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/a2arun/hosttools"
)

func TestToolsRouterKeepsTheToolsHostFromTheRouterItWraps(t *testing.T) {
	t.Parallel()
	ep, err := hosttools.Parse("unix:///run/host/tools.sock")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := egress.NewProxyRouter(egress.ProxyConfig{URL: "unix:///run/host/egress.sock"})
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	next := egress.RouterFunc(func(_ context.Context, f egress.Flow) (egress.Route, error) {
		seen = append(seen, f.Host)
		return egress.Direct(), nil
	})
	r := (&hostTools{ep: ep}).router(next)
	for _, f := range []egress.Flow{{Key: "c1", Host: ToolsHost, Port: 80}, {Key: "c1", Host: ToolsHost, Port: 443}, {Key: "c1", Host: "api.anthropic.com", Port: 443}} {
		if _, err := r.Route(t.Context(), f); err != nil {
			t.Fatalf("Router.Route(%s:%d) error = %v", f.Host, f.Port, err)
		}
	}
	if diff := cmp.Diff([]string{"api.anthropic.com"}, seen); diff != "" {
		t.Fatalf("hosts that reached the wrapped router wrong result (-want +got) diff = %s", diff)
	}
	if _, ok := (&hostTools{ep: ep}).router(proxy).(egress.InterceptHosts); ok {
		t.Fatal("tools router of the egress proxy implements InterceptHosts, want the proxy's unconstrained CA kept")
	}
	policy, err := egress.Policy{}.Router(egress.Header{Host: "api.example.com", Name: "Authorization", Value: "Bearer k"})
	if err != nil {
		t.Fatal(err)
	}
	ih, ok := (&hostTools{ep: ep}).router(policy).(egress.InterceptHosts)
	if !ok {
		t.Fatal("tools router of the policy doesn't implement InterceptHosts, want the policy's hosts")
	}
	if diff := cmp.Diff(policy.(egress.InterceptHosts).Hosts(), ih.Hosts()); diff != "" {
		t.Fatalf("Hosts() wrong result (-want +got) diff = %s", diff)
	}
}
