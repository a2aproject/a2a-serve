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

package meshbackend

import (
	"net/http"
	"strings"
)

// Routes describes agentmesh-one's routes to the relay's front.
type Routes struct{}

// Blocked refuses Agent Mesh's admin, console and metrics routes, and
// changes to its policies. The relay is managed through the control API and
// its policy comes from its settings; the console and metrics answer without
// authentication.
func (Routes) Blocked(p, method string) bool {
	for _, prefix := range []string{"/admin", "/console", "/metrics"} {
		if under(p, prefix) {
			return true
		}
	}
	return p == "/policies" && method != http.MethodGet && method != http.MethodOptions
}

// Enrollment reports whether agents join the relay or sign in to it at p.
func (Routes) Enrollment(p string) bool {
	return p == "/register" || p == "/enroll" || p == "/enroll/status"
}

// Ingress splits rest, which is <peer>/<service type>/<service name>/<route>,
// into the peer ID and the route.
func (Routes) Ingress(rest string) (agent, route string) {
	parts := strings.SplitN(rest, "/", 4)
	if peer, ok := canonicalPeer(parts[0]); ok {
		agent = peer
	}
	if len(parts) == 4 {
		route = parts[3]
	}
	return agent, route
}

func under(p, prefix string) bool {
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}
