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

package main

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Host is the host of the agents' private URLs.
const Host = "slim.local"

const (
	defaultOrg       = "a2a"
	defaultNamespace = "default"
)

var (
	// namePart is a component of a SLIM name.
	namePart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	// agentName is the name of an a2a-serve agent: a DNS label.
	agentName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// settings is what the network URL says: the SLIM node and where on it the
// agents are.
type settings struct {
	// endpoint is the node, scheme://host[:port].
	endpoint string
	tls      bool
	org      string
	ns       string
	// peers are the agents the user expects in the namespace.
	peers []string
}

func parseSettings(raw string) (settings, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return settings{}, fmt.Errorf("the network URL isn't a URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return settings{}, fmt.Errorf("want the SLIM node as http://host:port or https://host:port, got scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return settings{}, errors.New("the network URL names no SLIM node host")
	}
	if u.User != nil {
		return settings{}, errors.New("the network URL can't carry credentials; pass the shared secret with --network-token")
	}
	if u.Path != "" && u.Path != "/" {
		return settings{}, fmt.Errorf("the network URL has a path %q; put settings in the query, such as ?namespace=demo", u.Path)
	}
	if u.Fragment != "" {
		return settings{}, errors.New("the network URL can't have a fragment")
	}
	s := settings{endpoint: u.Scheme + "://" + u.Host, tls: u.Scheme == "https", org: defaultOrg, ns: defaultNamespace}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return settings{}, fmt.Errorf("the network URL's query: %w", err)
	}
	for key, values := range q {
		if len(values) != 1 {
			return settings{}, fmt.Errorf("the network URL sets %s more than once", key)
		}
		v := values[0]
		switch key {
		case "org":
			s.org = v
		case "namespace":
			s.ns = v
		case "peers":
			if s.peers, err = parsePeers(v); err != nil {
				return settings{}, err
			}
		default:
			return settings{}, fmt.Errorf("unknown setting %q in the network URL; want org, namespace or peers", key)
		}
	}
	if !namePart.MatchString(s.org) {
		return settings{}, fmt.Errorf("org %q isn't a SLIM name part: use letters, digits, '.', '_' and '-'", s.org)
	}
	if !namePart.MatchString(s.ns) {
		return settings{}, fmt.Errorf("namespace %q isn't a SLIM name part: use letters, digits, '.', '_' and '-'", s.ns)
	}
	return s, nil
}

func parsePeers(v string) ([]string, error) {
	var peers []string
	seen := map[string]bool{}
	for p := range strings.SplitSeq(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !agentName.MatchString(p) {
			return nil, fmt.Errorf("peer %q isn't an agent name: use lower-case letters, digits and dashes", p)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		peers = append(peers, p)
	}
	return peers, nil
}

// name is the SLIM name of an agent, org/namespace/agent.
type name struct {
	org, ns, agent string
}

func (n name) String() string { return n.org + "/" + n.ns + "/" + n.agent }

// privateURL is the URL other a2a-serve agents on the node reach n at.
func (n name) privateURL() string { return "http://" + Host + "/" + n.String() }

// route splits the path of a request for a private URL into the agent's
// name and the rest of the path, which starts with a slash. path is
// escaped, and so is rest.
func route(path string) (name, string, bool) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 4)
	if len(parts) < 3 {
		return name{}, "", false
	}
	n := name{org: parts[0], ns: parts[1], agent: parts[2]}
	for _, p := range parts[:3] {
		if !namePart.MatchString(p) {
			return name{}, "", false
		}
	}
	rest := "/"
	if len(parts) == 4 {
		rest += parts[3]
	}
	return n, rest, true
}
