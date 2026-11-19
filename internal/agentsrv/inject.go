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
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentcfg"
	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/boundary"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

// secretEnv returns the secrets that a container with the grant g gets from
// stored under the inject rules, as NAME=value: in its environment and in
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
func secretEnv(rules []agentprofile.InjectRule, g access.Sandbox, stored map[string]string) (env []string, inject []boundary.Injection, missing []string, err error) {
	values := map[string]string{}
	for _, s := range g.Secrets {
		v, ok := stored[s.From]
		if !ok {
			missing = append(missing, s.From)
			continue
		}
		values[s.Name] = v
	}
	placeholders := map[string]string{}
	for _, r := range rules {
		h, err := r.HeaderRule()
		if err != nil {
			return nil, nil, nil, &agentcfg.InjectRuleError{Rule: r, Err: err}
		}
		secrets := r.Secrets()
		for _, n := range secrets {
			placeholders[n] = cmp.Or(placeholders[n], egressrules.Placeholder)
		}
		if slices.ContainsFunc(secrets, func(n string) bool { _, ok := values[n]; return !ok }) {
			continue
		}
		inj := boundary.Injection{Host: h.Host, Port: h.Port, Header: h.Name, Value: h.Expand(func(n string) string { return values[n] }), Secrets: secrets}
		if !httpguts.ValidHeaderFieldValue(inj.Value) {
			return nil, nil, nil, &agentcfg.InjectRuleError{Rule: r, Err: fmt.Errorf("the value of %s has a character that a header can't carry", strings.Join(secrets, " or "))}
		}
		if r.IsModel() {
			p, _ := egressrules.LookupProvider(r.Model)
			inj.Model, inj.QueryKey, inj.Endpoints = true, p.QueryKey, p.Endpoints
			placeholders[r.Key] = p.Placeholder(r.Key)
		}
		inject = append(inject, inj)
	}
	injected := agentprofile.InjectedSecrets(rules)
	for _, s := range g.Secrets {
		if _, ok := stored[s.From]; !ok {
			continue
		}
		switch {
		case placeholders[s.Name] != "":
			env = append(env, s.Name+"="+placeholders[s.Name])
		case slices.Contains(injected, s.From):
			env = append(env, s.Name+"="+egressrules.Placeholder)
		default:
			env = append(env, s.Name+"="+values[s.Name])
		}
	}
	return env, inject, missing, nil
}
