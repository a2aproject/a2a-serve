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
	"net/netip"
	"testing"
)

func TestParseRuleRejects(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "*", "a..b", "nas.lan:0", "nas.lan:70000", "nas.lan:x", "[nas.lan]:22", "a b", "host/path", "[fd00::1", "nas.lan:022", "100.101.102.103", "100.64.0.0/10:22", "::ffff:10.0.0.0/80", "fe80::1%eth0"} {
		if _, err := ParseRule(s); err == nil {
			t.Errorf("ParseRule(%q) error = nil, want an error", s)
		}
	}
}

func TestPolicyRules(t *testing.T) {
	t.Parallel()
	p, err := NewPolicy("", []string{"NAS.lan.", "*.corp.example:443", "10.0.0.5:22", "192.168.0.0/16", "fd00::/8", "[fd01::1]:8080", "bücher.example"})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	if p.Mode != ModeInternet {
		t.Fatalf("NewPolicy().Mode = %q, want %q", p.Mode, ModeInternet)
	}
	names := []struct {
		host string
		port uint16
		want bool
	}{
		{"nas.lan", 22, true},
		{"nas.lan", 443, true},
		{"x.nas.lan", 22, false},
		{"git.corp.example", 443, true},
		{"a.b.corp.example", 443, true},
		{"git.corp.example", 22, false},
		{"corp.example", 443, false},
		{"xn--bcher-kva.example", 443, true},
	}
	for _, tc := range names {
		if _, got := p.NameRule(tc.host, tc.port); got != tc.want {
			t.Errorf("NameRule(%q, %d) = %v, want %v", tc.host, tc.port, got, tc.want)
		}
	}
	addrs := []struct {
		addr string
		port uint16
		want bool
	}{
		{"10.0.0.5", 22, true},
		{"10.0.0.5", 23, false},
		{"::ffff:10.0.0.5", 22, true},
		{"192.168.4.4", 1, true},
		{"172.16.0.1", 80, false},
		{"fd12::1", 80, true},
		{"fd01::1", 8080, true},
		{"fe80::1", 8080, false},
	}
	for _, tc := range addrs {
		if _, got := p.AddrRule(netip.MustParseAddr(tc.addr), tc.port); got != tc.want {
			t.Errorf("AddrRule(%s, %d) = %v, want %v", tc.addr, tc.port, got, tc.want)
		}
	}
	if !p.NamesHost("git.corp.example") || !p.NamesHost("10.0.0.5") || p.NamesHost("example.com") {
		t.Fatalf("NamesHost() doesn't follow the rules")
	}
}

func TestRuleString(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"NAS.lan.", "nas.lan"},
		{"*.Corp.Example:443", "*.corp.example:443"},
		{"10.0.0.5:22", "10.0.0.5:22"},
		{"192.168.1.7/24", "192.168.1.0/24"},
		{"::ffff:10.0.0.0/104", "10.0.0.0/8"},
		{"FD01::1", "fd01::1"},
		{"[fd01::1]:8080", "[fd01::1]:8080"},
		{"bücher.example", "xn--bcher-kva.example"},
		{"http://nas.lan", "nas.lan:80"},
		{"https://Grafana.corp:3000/d/abc?x=1", "grafana.corp:3000"},
		{"ssh://git.lan/repo", "git.lan"},
		{"http://[fd01::1]:8080/", "[fd01::1]:8080"},
	}
	for _, tc := range tests {
		r, err := ParseRule(tc.in)
		if err != nil {
			t.Fatalf("ParseRule(%q) error = %v", tc.in, err)
		}
		if got := r.String(); got != tc.want {
			t.Errorf("ParseRule(%q).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewPolicyRejectsUnknownMode(t *testing.T) {
	t.Parallel()
	if _, err := NewPolicy("strict", nil); err == nil {
		t.Fatalf("NewPolicy(strict) error = nil, want an error")
	}
}

func TestPublicSpecialPurpose(t *testing.T) {
	t.Parallel()
	for _, ip := range []string{"64:ff9b::a00:1", "2002:a00:1::1", "198.18.0.1", "240.0.0.1", "192.0.0.8", "::7f00:1", "fec0::1", "2001:db8::1"} {
		if Public(netip.MustParseAddr(ip)) {
			t.Errorf("Public(%s) = true, want false", ip)
		}
	}
}
