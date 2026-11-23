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
	"net"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

func resolveEgressMiddleware(cfg Config, targets map[string]string) ([]egress.MiddlewareEntry, []string, error) {
	if len(cfg.EgressMiddleware) == 0 {
		return nil, nil, nil
	}
	entries := make([]egress.MiddlewareEntry, 0, len(cfg.EgressMiddleware))
	for _, raw := range cfg.EgressMiddleware {
		host, rawURL, ok := strings.Cut(strings.TrimSpace(raw), "=")
		host, rawURL = strings.TrimSpace(host), strings.TrimSpace(rawURL)
		if !ok || host == "" || rawURL == "" {
			return nil, nil, usagef("invalid --egress-middleware: want host[:port]=URL, such as api.anthropic.com=http://127.0.0.1:8080/anthropic")
		}
		entries = append(entries, egress.MiddlewareEntry{Host: host, URL: rawURL})
	}
	if _, err := egress.NewMiddlewareRouter(egress.MiddlewareConfig{Entries: entries}); err != nil {
		return nil, nil, usagef("invalid --egress-middleware: %w", err)
	}
	var placeholders []string
	for _, e := range entries {
		h, port, _ := egressrules.ParseInjectTarget(e.Host)
		targetKey := net.JoinHostPort(h, strconv.Itoa(int(port)))
		shownTarget := h
		if port != egressrules.HTTPSPort {
			shownTarget = targetKey
		}
		if owner, exists := targets[targetKey]; exists {
			return nil, nil, usagef("--egress-middleware and %s both name %s; give each host one", owner, shownTarget)
		}
		if port == egressrules.HTTPSPort {
			for _, name := range egressrules.ProviderNames() {
				if p, _ := egressrules.LookupProvider(name); p.Host == h {
					placeholders = append(placeholders, p.KeyEnv+"="+p.Placeholder(p.KeyEnv))
				}
			}
		}
	}
	return entries, placeholders, nil
}
