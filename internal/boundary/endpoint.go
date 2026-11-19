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

package boundary

import (
	"context"
	"errors"
	"net/netip"
)

// Endpoint is what a sandbox runtime needs of an endpoint of the public
// core/egress package, whose fields that package doesn't export.
type Endpoint struct {
	// Open opens a boundary of the endpoint whose connections to
	// host.docker.internal reach what hostGateway returns. The endpoint
	// closes it when it closes.
	Open func(hostGateway func(ctx context.Context, port uint16) (netip.AddrPort, bool)) (*Boundary, error)
	// CA is the PEM certificate that the client trusts, or nil.
	CA []byte
	// Mode returns the egress mode of the current policy.
	Mode func() string
	// Credentials is set when the endpoint adds credentials to requests.
	Credentials bool
}

var endpointOf func(any) (Endpoint, bool)

// RegisterEndpoint sets how EndpointOf reads a core/egress endpoint. It is
// called once, from the init of core/egress.
func RegisterEndpoint(f func(any) (Endpoint, bool)) { endpointOf = f }

// EndpointOf returns what a sandbox runtime needs of e, a
// *core/egress.Endpoint.
func EndpointOf(e any) (Endpoint, error) {
	if endpointOf != nil {
		if ep, ok := endpointOf(e); ok {
			return ep, nil
		}
	}
	return Endpoint{}, errors.New("invalid egress endpoint: get one from egress.Boundary.Open")
}

var credentialOf func(Injection) any

// RegisterCredential sets how CredentialOf makes a core/egress credential.
// It is called once, from the init of core/egress.
func RegisterCredential(f func(Injection) any) { credentialOf = f }

// CredentialOf returns inj as a core/egress Credential, which an endpoint
// adds as it is, with no placeholder in its Env. It returns nil before
// core/egress is linked in.
func CredentialOf(inj Injection) any {
	if credentialOf == nil {
		return nil
	}
	return credentialOf(inj)
}
