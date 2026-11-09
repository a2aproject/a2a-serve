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
	"fmt"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

// secretEnv returns the secrets that a container with sb gets from stored
// under the inject rules of cfg, as NAME=value: in its environment and in
// the variables of .mcp.json. It also returns the headers its boundary
// adds, and the names of the granted secrets that have no stored value.
func secretEnv(cfg *agentdir.Config, sb access.Sandbox, stored agentdir.Secrets) (env []string, inject []sandboxnet.Injection, missing []string, err error) {
	granted, missing := sb.Values(stored)
	env, inject, err = injections(cfg, granted)
	return env, inject, missing, err
}

// injections returns the environment of a container whose granted secrets
// env holds, as NAME=value, and the headers its boundary adds under the
// inject rules of cfg. A secret that a rule adds stays in the environment
// as a placeholder, shaped like a key of its provider for a model key, so
// that clients that need the variable start, and their requests to the
// rule's host get the real header. A rule adds nothing to a container that
// isn't granted all of its secrets.
func injections(cfg *agentdir.Config, env []string) ([]string, []sandboxnet.Injection, error) {
	if len(cfg.Inject) == 0 {
		return env, nil, nil
	}
	values := map[string]string{}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		values[name] = value
	}
	placeholders := map[string]string{}
	var out []sandboxnet.Injection
	for _, r := range cfg.Inject {
		h, err := r.HeaderRule()
		if err != nil {
			return nil, nil, fmt.Errorf("--%s %s: %w", r.Setting(), r.Entry(), err)
		}
		names := r.Secrets()
		if slices.ContainsFunc(names, func(n string) bool { _, ok := values[n]; return !ok }) {
			continue
		}
		inj := sandboxnet.Injection{Host: h.Host, Port: h.Port, Header: h.Name, Value: h.Expand(func(n string) string { return values[n] }), Secrets: names}
		if !httpguts.ValidHeaderFieldValue(inj.Value) {
			return nil, nil, fmt.Errorf("--%s %s: the value of %s has a character that a header can't carry", r.Setting(), r.Entry(), strings.Join(names, " or "))
		}
		if r.IsModel() {
			p, _ := egress.LookupProvider(r.Model)
			inj.Placeholder, inj.Key, inj.QueryKey, inj.Endpoints = p.Placeholder(r.Key), values[r.Key], p.QueryKey, p.Endpoints
			placeholders[r.Key] = inj.Placeholder
		}
		out = append(out, inj)
	}
	injected := cfg.InjectedSecrets()
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case placeholders[name] != "":
			kept = append(kept, name+"="+placeholders[name])
		case slices.Contains(injected, name):
			kept = append(kept, name+"="+egress.Placeholder)
		default:
			kept = append(kept, kv)
		}
	}
	return kept, out, nil
}

// injectHosts returns the hosts of the inject rules of cfg, which the
// egress CA covers.
func injectHosts(cfg *agentdir.Config) []string {
	var out []string
	for _, r := range cfg.Inject {
		h, err := r.HeaderRule()
		if err != nil || slices.Contains(out, h.Host) {
			continue
		}
		out = append(out, h.Host)
	}
	return out
}

// injectAllow returns the egress rules that allow the hosts of the inject
// rules of cfg, which a rule allows even with --egress allowlist.
func injectAllow(cfg *agentdir.Config) []string {
	var out []string
	for _, r := range cfg.Inject {
		h, err := r.HeaderRule()
		if err != nil {
			continue
		}
		out = append(out, h.Host+":"+fmt.Sprint(h.Port))
	}
	return out
}
