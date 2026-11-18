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
	"context"
	"crypto/x509"
	"net"
	"net/netip"

	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

// NewWithHosts returns a boundary that resolves every name to 127.0.0.1
// and trusts roots for the hosts that get credentials.
func NewWithHosts(cfg Config, roots *x509.CertPool) (*Boundary, error) {
	return newBoundary(cfg, deps{roots: roots, resolver: loopbackResolver{}})
}

type loopbackResolver struct{}

func (loopbackResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if host == "missing.test" {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
}

// EngineModes are the modes of the internal rules, which Mode mirrors.
var EngineModes = egressrules.Modes
