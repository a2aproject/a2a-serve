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
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/a2aproject/a2a-serve/core/egress"
	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

func resolveInjectHeaders(cfg Config, targets map[string]string) ([]egress.Credential, []string, error) {
	var creds []egress.Credential
	var placeholders []string
	for _, raw := range cfg.InjectHeaders {
		dest, header, ok := strings.Cut(strings.TrimSpace(raw), "=")
		if !ok {
			return nil, nil, usagef("invalid --egress-inject-header: want host[:port]=Name: value, such as api.github.com=\"Authorization:Bearer env:GH_TOKEN\"")
		}
		host, port, err := egressrules.ParseInjectTarget(dest)
		if err != nil {
			return nil, nil, usagef("invalid --egress-inject-header: the host before = %w; want host[:port]=Name: value", err)
		}
		targetKey := net.JoinHostPort(host, strconv.Itoa(int(port)))
		shownTarget := host
		if port != egressrules.HTTPSPort {
			shownTarget = targetKey
		}
		if owner, exists := targets[targetKey]; exists {
			if owner == "--egress-inject-header" {
				return nil, nil, usagef("--egress-inject-header names %s twice; give each host one header", shownTarget)
			}
			return nil, nil, usagef("--egress-inject-header and %s both name %s; give each host one", owner, shownTarget)
		}
		name, valExpr, ok := strings.Cut(header, ":")
		name, valExpr = strings.TrimSpace(name), strings.TrimSpace(valExpr)
		canonName := http.CanonicalHeaderKey(name)
		switch {
		case !ok || !httpguts.ValidHeaderFieldName(name):
			return nil, nil, usagef("invalid --egress-inject-header for %s: want a header Name: value after =", shownTarget)
		case egressrules.ReservedHeader(canonName):
			return nil, nil, usagef("invalid --egress-inject-header for %s: a2a-serve-sidecar can't set the header %s", shownTarget, canonName)
		case valExpr == "":
			return nil, nil, usagef("invalid --egress-inject-header for %s: empty header value", shownTarget)
		}
		val, vars, err := expandInjectHeaderValue(cfg, raw, shownTarget, valExpr)
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(val) == "" {
			return nil, nil, usagef("invalid --egress-inject-header for %s: empty header value", shownTarget)
		}
		if !httpguts.ValidHeaderFieldValue(val) {
			return nil, nil, usagef("invalid --egress-inject-header for %s: the value of %s has a character that a header can't carry", shownTarget, canonName)
		}
		creds = append(creds, egress.Header{Host: shownTarget, Name: canonName, Value: val})
		targets[targetKey] = "--egress-inject-header"
		for _, v := range vars {
			placeholders = append(placeholders, v+"="+egressrules.Placeholder)
		}
	}
	return creds, placeholders, nil
}

func expandInjectHeaderValue(cfg Config, raw, shownTarget, valExpr string) (string, []string, error) {
	head, last := "", valExpr
	if i := strings.LastIndexByte(valExpr, ' '); i >= 0 {
		head, last = valExpr[:i+1], valExpr[i+1:]
	}
	if strings.HasPrefix(last, "env:") {
		v := strings.TrimPrefix(last, "env:")
		if !envName.MatchString(v) {
			return "", nil, usagef("invalid --egress-inject-header for %s: env: needs the name of a variable, such as env:GH_TOKEN", shownTarget)
		}
		secret := cfg.getenv(v)
		if secret == "" {
			return "", nil, usagef("--egress-inject-header %s: the variable %s is unset or empty; export it before you start a2a-serve-sidecar", shownTarget, v)
		}
		return head + secret, []string{v}, nil
	}
	if strings.Contains(valExpr, "$") {
		r, err := egressrules.ParseHeaderRule(raw)
		if err != nil {
			return "", nil, usagef("invalid --egress-inject-header: %w", err)
		}
		vars := r.Secrets()
		for _, v := range vars {
			if cfg.getenv(v) == "" {
				return "", nil, usagef("--egress-inject-header %s: the variable %s is unset or empty; export it before you start a2a-serve-sidecar", shownTarget, v)
			}
		}
		return r.Expand(cfg.getenv), vars, nil
	}
	return valExpr, nil, nil
}
