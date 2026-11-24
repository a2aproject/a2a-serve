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
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-serve/core/a2aserve"
	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/sidecar/hosttools"
)

const (
	// ToolsHost is the name at which the Docker sandbox reaches the
	// host's tools endpoint, on port 80. The boundary sends its requests
	// to the endpoint, so no port of this machine is open to the sandbox.
	ToolsHost = "tools.sidecar.internal"
	// ToolsServer names the MCP server of the tools endpoint in the
	// agent's sessions.
	ToolsServer = "host"
	// toolsDialTimeout bounds the start of the session with the tools
	// endpoint, and toolsCloseTimeout its end when a2a-serve-sidecar fails.
	toolsDialTimeout  = 10 * time.Second
	toolsCloseTimeout = 5 * time.Second
)

// parseTools parses --tools, or returns nil without it.
func parseTools(v, sandbox string) (*hosttools.Endpoint, error) {
	if v == "" {
		return nil, nil
	}
	ep, err := hosttools.Parse(v)
	if err != nil {
		return nil, usagef("invalid --tools: %w", err)
	}
	if ep.Unix() && sandbox == SandboxLocal {
		return nil, usagef("--tools unix:// needs --sandbox docker, gvisor or bwrap: with --sandbox local the agent connects to the tools endpoint itself, so pass an http:// URL")
	}
	return &ep, nil
}

// hostTools is the session of a2a-serve-sidecar with the host's tools endpoint.
type hostTools struct {
	ep     hosttools.Endpoint
	client *hosttools.Client
	hooks  *hosttools.Hooks
}

// startTools starts a session with the tools endpoint ep, and reads which
// hooks it answers.
func startTools(ctx context.Context, ep hosttools.Endpoint, version string, log *slog.Logger) (*hostTools, error) {
	ctx, cancel := context.WithTimeout(ctx, toolsDialTimeout)
	defer cancel()
	c, err := hosttools.Dial(ctx, ep, version)
	if err != nil {
		return nil, fmt.Errorf("connecting to --tools: %w", err)
	}
	hooks := hosttools.NewHooks(c, hosttools.DefaultHookTimeouts, log)
	s := hooks.Support()
	log.Info("connected to the tools endpoint", "endpoint", ep.String(), "beforeTool", s.BeforeTool, "afterTool", s.AfterTool)
	return &hostTools{ep: ep, client: c, hooks: hooks}, nil
}

// server returns the MCP server of the tools endpoint as the agent
// reaches it: at ToolsHost in a Docker sandbox, without the query of the
// URL, which the boundary adds, and at the endpoint's own URL on this
// machine, with the endpoint's token, which the boundary adds otherwise.
func (t *hostTools) server(sandbox string) a2aserve.MCPServer {
	if sandbox == SandboxLocal {
		srv := a2aserve.MCPServer{Name: ToolsServer, URL: t.ep.URL()}
		if tok := t.ep.Token(); tok != "" {
			srv.Headers = map[string]string{hosttools.HeaderToken: tok}
		}
		return srv
	}
	return a2aserve.MCPServer{Name: ToolsServer, URL: "http://" + ToolsHost + t.ep.Path()}
}

// wrapRouter returns the wrapper that routes ToolsHost of a router to the
// tools endpoint, or nil without t.
func (t *hostTools) wrapRouter() func(egress.Router) egress.Router {
	if t == nil {
		return nil
	}
	return t.router
}

// router returns next, which also routes ToolsHost to the tools endpoint.
// It intercepts the hosts that next does.
func (t *hostTools) router(next egress.Router) egress.Router {
	r := toolsRouter{next: next, proxy: t.ep.Proxy()}
	if ih, ok := next.(egress.InterceptHosts); ok {
		return interceptingToolsRouter{toolsRouter: r, hosts: ih}
	}
	return r
}

// deciders returns the tool deciders of the hooks, or none without t.
func (t *hostTools) deciders() []a2aserve.ToolDecider {
	if t == nil || !t.hooks.Support().BeforeTool {
		return nil
	}
	return []a2aserve.ToolDecider{t.hooks}
}

// toolDone returns the ToolDone hook of the hooks, or nil without t.
func (t *hostTools) toolDone() func(context.Context, a2aserve.ToolResult) {
	if t == nil || !t.hooks.Support().AfterTool {
		return nil
	}
	return t.hooks.ToolDone
}

// closeTools sends the afterTool hooks that wait until ctx ends, and ends
// the session with the tools endpoint.
func (s *Server) closeTools(ctx context.Context) error {
	if s.tools == nil {
		return nil
	}
	t := s.tools
	s.tools = nil
	t.hooks.Close(ctx)
	return t.client.Close(ctx)
}

// toolsRouter routes plain HTTP to ToolsHost:80 to the tools endpoint
// through proxy, and other connections by next.
type toolsRouter struct {
	next  egress.Router
	proxy http.RoundTripper
}

func (r toolsRouter) Route(ctx context.Context, f egress.Flow) (egress.Route, error) {
	if f.Host != ToolsHost {
		return r.next.Route(ctx, f)
	}
	if f.Port != 80 {
		return egress.Refuse(http.StatusForbidden, "the tools endpoint is at port 80"), nil
	}
	return egress.HTTP(func(http.RoundTripper) http.RoundTripper { return r.proxy }), nil
}

// interceptingToolsRouter is a toolsRouter whose next router names the
// hosts it intercepts. The tools endpoint takes plain HTTP only, so it adds
// none.
type interceptingToolsRouter struct {
	toolsRouter
	hosts egress.InterceptHosts
}

func (r interceptingToolsRouter) Hosts() []string { return r.hosts.Hosts() }
