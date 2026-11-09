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
	"cmp"
	"fmt"
	"net"
	"slices"
	"strconv"
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
//
// A secret that a rule adds stays in the environment as a placeholder,
// shaped like a key of its provider for a model key, so that clients that
// need the variable start, and their requests to the rule's host get the
// real header. A grant that passes an added secret under another name gets
// a placeholder too, whatever rules are pending, so that the value never
// enters the container. A rule adds nothing to a container that isn't
// granted all of its secrets.
func secretEnv(cfg *agentdir.Config, sb access.Sandbox, stored agentdir.Secrets) (env []string, inject []sandboxnet.Injection, missing []string, err error) {
	values := map[string]string{}
	for _, g := range sb.Secrets {
		v, ok := stored[g.From]
		if !ok {
			missing = append(missing, g.From)
			continue
		}
		values[g.Name] = v
	}
	placeholders := map[string]string{}
	for _, r := range cfg.Inject {
		h, err := r.HeaderRule()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("--%s %s: %w", r.Setting(), r.Entry(), err)
		}
		secrets := r.Secrets()
		for _, n := range secrets {
			placeholders[n] = cmp.Or(placeholders[n], egress.Placeholder)
		}
		if slices.ContainsFunc(secrets, func(n string) bool { _, ok := values[n]; return !ok }) {
			continue
		}
		inj := sandboxnet.Injection{Host: h.Host, Port: h.Port, Header: h.Name, Value: h.Expand(func(n string) string { return values[n] }), Secrets: secrets}
		if !httpguts.ValidHeaderFieldValue(inj.Value) {
			return nil, nil, nil, fmt.Errorf("--%s %s: the value of %s has a character that a header can't carry", r.Setting(), r.Entry(), strings.Join(secrets, " or "))
		}
		if r.IsModel() {
			p, _ := egress.LookupProvider(r.Model)
			inj.Model, inj.QueryKey, inj.Endpoints = true, p.QueryKey, p.Endpoints
			placeholders[r.Key] = p.Placeholder(r.Key)
		}
		inject = append(inject, inj)
	}
	injected := cfg.InjectedSecrets()
	for _, g := range sb.Secrets {
		if _, ok := stored[g.From]; !ok {
			continue
		}
		switch {
		case placeholders[g.Name] != "":
			env = append(env, g.Name+"="+placeholders[g.Name])
		case slices.Contains(injected, g.From):
			env = append(env, g.Name+"="+egress.Placeholder)
		default:
			env = append(env, g.Name+"="+values[g.Name])
		}
	}
	return env, inject, missing, nil
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

// injectAllow returns the egress rules that allow the hosts of inject, the
// headers a container's boundary adds, which a rule allows even with
// --egress allowlist. A container without the secrets of a rule doesn't
// get its host this way.
func injectAllow(inject []sandboxnet.Injection) []string {
	out := make([]string, 0, len(inject))
	for _, inj := range inject {
		out = append(out, net.JoinHostPort(inj.Host, strconv.Itoa(int(inj.Port))))
	}
	return out
}
