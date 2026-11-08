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
	"net/http"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentkey"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/modelproxy"
)

// modelKeyPlaceholder is the API key that an agent process reaching the
// server through a socket sends; the socket tells the process.
const modelKeyPlaceholder = "a2a-serve-adds-the-key"

// modelSecrets returns the secrets of cfg with model API keys, which stay
// out of the sandbox. An agent without a sandbox gets them.
func modelSecrets(cfg *agentdir.Config) []string {
	if cfg.Sandbox == harness.SandboxLocal {
		return nil
	}
	var out []string
	for _, name := range cfg.Secrets {
		if _, ok := modelproxy.ProviderOfKey(name); ok {
			out = append(out, name)
		}
	}
	return out
}

// modelRoute returns how the agent process of ep reaches the model routes
// with the API keys in the secrets names.
func (s *Server) modelRoute(ep *harnessEndpoint, names []string) *harness.ModelRoute {
	if ep.key == "" {
		return &harness.ModelRoute{Keys: names, Socket: ep.socket, Key: modelKeyPlaceholder}
	}
	return &harness.ModelRoute{Keys: names, URL: s.harness.url, Key: ep.key}
}

// harnessRoutes serves agent processes the model routes and the agents
// routes on hl. The process of anonymous callers gets only the model routes.
func (s *Server) harnessRoutes(hl *harnessListeners) http.Handler {
	agents := s.proxy.Routes(hl.processOf)
	models := modelproxy.New(modelproxy.Options{
		Key:       func(agentKey string, p modelproxy.Provider) (string, bool) { return modelKey(hl, agentKey, p) },
		Process:   hl.modelProcessOf,
		Transport: s.modelTransport,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, modelproxy.PathPrefix) {
			models.ServeHTTP(w, r)
			return
		}
		if key, ok := hl.processOf(r); ok && key == agentkey.Anonymous {
			jsonhttp.Error(w, http.StatusNotFound, "not found")
			return
		}
		agents.ServeHTTP(w, r)
	})
}

// modelKey returns the API key of provider that hl granted the agent
// process of agentKey.
func modelKey(hl *harnessListeners, agentKey string, p modelproxy.Provider) (string, bool) {
	for _, name := range p.Keys {
		if v, ok := hl.modelKey(agentKey, name); ok {
			return v, true
		}
	}
	return "", false
}

// modelGrantOf returns the values in env of the variables names, which the
// agent process gets through the model routes in place of its environment.
func modelGrantOf(env, names []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if slices.Contains(names, name) {
			out[name] = value
		}
	}
	return out
}
