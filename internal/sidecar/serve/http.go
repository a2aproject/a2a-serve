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
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// keepAlive is the interval of the keep-alive comments of A2A streams.
const keepAlive = 15 * time.Second

// listen listens on a. A Unix socket that a crashed run left behind is
// replaced, and one that answers is refused.
func listen(a address) (net.Listener, error) {
	if a.network != "unix" {
		ln, err := net.Listen(a.network, a.addr)
		if err != nil {
			return nil, &UsageError{Err: fmt.Errorf("listening on %s: %w", a.addr, err)}
		}
		return ln, nil
	}
	if err := removeStaleSocket(a.addr); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", a.addr)
	if err != nil {
		return nil, &UsageError{Err: fmt.Errorf("listening on unix://%s: %w", a.addr, err)}
	}
	if err := os.Chmod(a.addr, 0o600); err != nil {
		return nil, errors.Join(err, ln.Close())
	}
	return ln, nil
}

// removeStaleSocket removes the socket at path if nothing answers on it.
func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSocket == 0 {
		return &UsageError{Err: fmt.Errorf("invalid --listen unix://%s: the path exists and isn't a socket", path)}
	}
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		return &UsageError{Err: errors.Join(fmt.Errorf("invalid --listen unix://%s: another process listens on it", path), conn.Close())}
	}
	return os.Remove(path)
}

// url returns the URL of the A2A endpoint on ln, as the ready line shows
// it.
func url(ln net.Listener) string {
	if ln.Addr().Network() == "unix" {
		return "unix://" + ln.Addr().String()
	}
	return "http://" + ln.Addr().String()
}

// routes serves the agent card, and the A2A handler over JSON-RPC and REST.
// Over TCP, it serves only requests for loopback hosts, so that a web page
// can't rebind its own name to the endpoint.
func routes(h a2asrv.RequestHandler, card func(baseURL string) *a2a.AgentCard, tcp bool) http.Handler {
	mux := http.NewServeMux()
	cardHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a2asrv.NewStaticAgentCardHandler(card("http://"+r.Host)).ServeHTTP(w, r)
	})
	mux.Handle("GET "+a2asrv.WellKnownAgentCardPath, cardHandler)
	limit := jsonhttp.Limit{Max: jsonhttp.MaxBody, Idle: jsonhttp.ReadIdleTimeout}
	a2aRoute := func(next http.Handler) http.Handler { return jsonhttp.RequireJSON(limit.Handler(next)) }
	mux.Handle("POST /{$}", a2aRoute(a2asrv.NewJSONRPCHandler(h, a2asrv.WithTransportKeepAlive(keepAlive))))
	mux.Handle("/", a2aRoute(a2asrv.NewRESTHandler(h, a2asrv.WithTransportKeepAlive(keepAlive))))
	out := http.NewCrossOriginProtection().Handler(mux)
	if !tcp {
		return out
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if !loopback(host) {
			jsonhttp.Error(w, http.StatusMisdirectedRequest, "this server serves only localhost")
			return
		}
		out.ServeHTTP(w, r)
	})
}

// agentCard returns the card of agent served at baseURL.
func agentCard(agent, version, baseURL string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:        agent,
		Description: agent + " in a sandbox, working in a shared workspace",
		Version:     version,
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(baseURL, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(baseURL, a2a.TransportProtocolHTTPJSON),
		},
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{{
			ID:          "code",
			Name:        "Coding",
			Description: "Reads, changes and runs the code in the workspace.",
			Tags:        []string{"coding"},
		}},
	}
}
