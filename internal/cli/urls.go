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

package cli

import (
	"cmp"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// URL scopes of an agent.
const (
	scopeLocal   = "local"
	scopePrivate = "private"
	scopePublic  = "public"
)

// statusURL returns the agent's URL in scope, or with an empty scope the
// one which reaches furthest: public, then private, then local.
func statusURL(s *controlapi.Status, scope string) (string, error) {
	switch scope {
	case "":
		return cmp.Or(s.Public, s.Private, s.Local), nil
	case scopeLocal:
		return s.Local, nil
	case scopePrivate:
		if s.Private == "" && s.PrivateProblem != nil {
			return "", fmt.Errorf("%s has no private URL now: %s", s.Name, s.PrivateProblem.Line())
		}
		if s.Private == "" {
			return "", fmt.Errorf("%s has no private URL; add one with `a2a-serve config set --private tailscale` (or a relay URL), then `a2a-serve restart`", s.Name)
		}
		return s.Private, nil
	case scopePublic:
		if s.Public == "" && s.PublicProblem != nil {
			return "", fmt.Errorf("%s has no public URL now: %s", s.Name, s.PublicProblem.Line())
		}
		if s.Public == "" {
			return "", fmt.Errorf("%s has no public URL; add one with `a2a-serve config set --public tailscale` (or a relay URL), then `a2a-serve restart`", s.Name)
		}
		return s.Public, nil
	}
	return "", unknownScope(scope)
}

func unknownScope(scope string) error {
	return fmt.Errorf("unknown URL scope %q", scope)
}

// checkScope reports whether scope is empty, local, private or public, the
// values [statusURL] takes.
func checkScope(scope string) error {
	switch scope {
	case "", scopeLocal, scopePrivate, scopePublic:
		return nil
	}
	return unknownScope(scope)
}

// checkInspectorScope reports whether scope is local or private, the
// values [inspectorURL] takes.
func checkInspectorScope(scope string) error {
	switch scope {
	case scopeLocal, scopePrivate:
		return nil
	}
	return fmt.Errorf("the inspector isn't served on the %s URL; want local or private", scope)
}

// inspectorURL returns the URL to open the web inspector at in scope: the
// local URL, or the private URL, where it is served only over HTTPS.
func inspectorURL(s *controlapi.Status, scope string) (string, error) {
	if err := checkInspectorScope(scope); err != nil {
		return "", err
	}
	if scope == scopeLocal {
		return s.Local, nil
	}
	u, err := statusURL(s, scopePrivate)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf("the inspector isn't served on %s, because its login link and session would cross the network unencrypted; run `a2a-serve inspect --url local` on the agent's machine", u)
	}
	return u, nil
}

// localOnly reports whether rawURL only works on this machine.
func localOnly(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Hostname() == "localhost" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}
