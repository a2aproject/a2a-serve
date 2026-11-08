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

package egress

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// What the Docker sandbox may reach.
const (
	// ModeInternet allows the public internet and the allowed hosts.
	ModeInternet = "internet"
	// ModeAllowlist allows only the allowed hosts.
	ModeAllowlist = "allowlist"
	// ModeUnrestricted gives the sandbox Docker's own network, with no
	// boundary.
	ModeUnrestricted = "unrestricted"
)

// Modes are the values of the egress setting.
var Modes = []string{ModeInternet, ModeAllowlist, ModeUnrestricted}

// HostGateway is the name through which a container reaches the host it
// runs on.
const HostGateway = "host.docker.internal"

// Policy decides where the sandbox may connect.
type Policy struct {
	Mode  string
	Allow []Rule
}

// NewPolicy returns the policy of mode, empty for internet, and the allow
// rules.
func NewPolicy(mode string, allow []string) (Policy, error) {
	switch mode {
	case "":
		mode = ModeInternet
	case ModeInternet, ModeAllowlist, ModeUnrestricted:
	default:
		return Policy{}, fmt.Errorf("unknown egress %q: want %s", mode, strings.Join(Modes, ", "))
	}
	p := Policy{Mode: mode}
	for _, s := range allow {
		r, err := ParseRule(s)
		if err != nil {
			return Policy{}, err
		}
		p.Allow = append(p.Allow, r)
	}
	return p, nil
}

// NameRule returns the rule that allows the normalized name host on port.
func (p Policy) NameRule(host string, port uint16) (Rule, bool) {
	for _, r := range p.Allow {
		if r.matchesName(host) && r.matchesPort(port) {
			return r, true
		}
	}
	return Rule{}, false
}

// AddrRule returns the rule that allows addr on port.
func (p Policy) AddrRule(addr netip.Addr, port uint16) (Rule, bool) {
	addr = addr.Unmap()
	for _, r := range p.Allow {
		if r.prefix.IsValid() && r.prefix.Contains(addr) && r.matchesPort(port) {
			return r, true
		}
	}
	return Rule{}, false
}

// NamesHost reports whether a rule names host on any port, so that a
// refusal can tell a wrong port from an unlisted host.
func (p Policy) NamesHost(host string) bool {
	addr, err := netip.ParseAddr(host)
	for _, r := range p.Allow {
		if err == nil && r.prefix.IsValid() && r.prefix.Contains(addr.Unmap()) || err != nil && r.matchesName(host) {
			return true
		}
	}
	return false
}

// Rule allows a host, a name suffix, an address or a range, on one port or
// all of them.
type Rule struct {
	// name is an exact name, and suffix ".example.com" for *.example.com.
	name, suffix string
	prefix       netip.Prefix
	port         uint16
}

// String returns the rule in its normal form, such as nas.lan:22,
// *.example.com, 192.168.1.0/24 or [fd00::1]:22.
func (r Rule) String() string {
	var host string
	switch {
	case r.name != "":
		host = r.name
	case r.suffix != "":
		host = "*" + r.suffix
	case r.prefix.IsSingleIP():
		host = r.prefix.Addr().String()
	default:
		host = r.prefix.String()
	}
	if r.port == 0 {
		return host
	}
	if r.prefix.IsValid() && r.prefix.Addr().Is6() {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(int(r.port))
}

func (r Rule) matchesName(host string) bool {
	return r.name != "" && host == r.name || r.suffix != "" && strings.HasSuffix(host, r.suffix)
}

func (r Rule) matchesPort(port uint16) bool { return r.port == 0 || r.port == port }

// ParseRule parses an allow rule: a name such as nas.lan, *.example.com for
// the names under example.com, an IP address or a CIDR range, each with an
// optional :port. An IPv6 address takes brackets with a port, such as
// [fd00::1]:22.
func ParseRule(s string) (Rule, error) {
	r, err := parseRule(strings.TrimSpace(s))
	if err != nil {
		return Rule{}, fmt.Errorf("invalid egress rule %q: %w; want a host, *.domain, an IP address or a CIDR range, with an optional :port", s, err)
	}
	if r.prefix.IsValid() && tailnetV4.Overlaps(r.prefix) {
		return Rule{}, fmt.Errorf("invalid egress rule %q: the sandbox's resolver uses the addresses of %s; allow a tailnet host by its name, such as nas.example.ts.net", s, tailnetV4)
	}
	return r, nil
}

// tailnetV4 holds the IPv4 addresses of the tailnet, which the resolver in
// the sandbox hands out for the names it resolves.
var tailnetV4 = netip.MustParsePrefix("100.64.0.0/10")

func parseRule(s string) (Rule, error) {
	if s == "" {
		return Rule{}, errors.New("empty")
	}
	if r, ok := parseAddrOrPrefix(s); ok {
		return r, nil
	}
	host, port := s, ""
	if strings.HasPrefix(s, "[") {
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return Rule{}, err
		}
		host, port = h, p
	} else if i := strings.LastIndexByte(s, ':'); i >= 0 {
		host, port = s[:i], s[i+1:]
	}
	var r Rule
	if a, ok := parseAddrOrPrefix(host); ok {
		r = a
	} else if strings.HasPrefix(s, "[") {
		return Rule{}, errors.New("brackets hold an IPv6 address")
	} else if rest, ok := strings.CutPrefix(host, "*."); ok {
		name, err := NormalizeHost(rest)
		if err != nil {
			return Rule{}, err
		}
		r.suffix = "." + name
	} else {
		name, err := NormalizeHost(host)
		if err != nil {
			return Rule{}, err
		}
		r.name = name
	}
	if port != "" || strings.HasSuffix(s, ":") {
		n, err := ParsePort(port)
		if err != nil {
			return Rule{}, err
		}
		r.port = n
	}
	return r, nil
}

func parseAddrOrPrefix(s string) (Rule, bool) {
	if a, err := netip.ParseAddr(s); err == nil && a.Zone() == "" {
		a = a.Unmap()
		return Rule{prefix: netip.PrefixFrom(a, a.BitLen())}, true
	}
	if p, err := netip.ParsePrefix(s); err == nil && p.Addr().Zone() == "" {
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return Rule{}, false
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		return Rule{prefix: p.Masked()}, true
	}
	return Rule{}, false
}

// ParsePort parses a decimal port in 1-65535.
func ParsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 || s[0] == '+' || s[0] == '0' {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return uint16(n), nil
}

// NormalizeHost returns a host name in lower case, without a trailing dot
// and in A-labels, or an error when it isn't a valid name: dot-separated
// labels of letters, digits, hyphens and underscores.
func NormalizeHost(name string) (string, error) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	ascii, err := idna.Lookup.ToASCII(name)
	if err != nil {
		// Lookup rejects underscores, which some service names use.
		ascii = name
	}
	if len(ascii) == 0 || len(ascii) > 253 {
		return "", errors.New("invalid host name")
	}
	for label := range strings.SplitSeq(ascii, ".") {
		if len(label) == 0 || len(label) > 63 {
			return "", errors.New("invalid host name")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", errors.New("invalid host name")
			}
		}
	}
	return ascii, nil
}
