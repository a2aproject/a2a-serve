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
	"net/http"
	"path"
	"strings"

	"github.com/a2aproject/a2a-serve/internal/egressrules"
)

// Injection is a header that the router of a policy adds to the
// container's HTTPS requests to Host on Port. The boundary answers the
// container's TLS connections to that host with a certificate of its
// [CA], adds the header, and sends the request on to the host over TLS
// that it checks.
type Injection struct {
	Host string
	Port uint16
	// Header is set to Value.
	Header, Value string
	// Model is set for the key of a model provider. The boundary removes
	// the credential headers and the query parameter QueryKey that the
	// container sent, and sets Header only on the requests of Endpoints,
	// so that the container can't pick how the key reaches the host.
	Model     bool
	QueryKey  string
	Endpoints []string
	// Secrets name the secrets in Value, for the log.
	Secrets []string
}

// wrap returns the middleware of an intercepted connection to the host of
// inj, which puts the header of inj on each request.
func (inj Injection) wrap(next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		inj.apply(r)
		return next.RoundTrip(r)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// apply puts the header of inj on the outgoing request r.
func (inj Injection) apply(r *http.Request) {
	if !inj.Model {
		r.Header.Del(inj.Header)
		r.Header.Set(inj.Header, inj.Value)
		return
	}
	for _, name := range egressrules.CredentialHeaders {
		r.Header.Del(name)
	}
	if inj.QueryKey != "" && r.URL.Query().Has(inj.QueryKey) {
		q := r.URL.Query()
		q.Del(inj.QueryKey)
		r.URL.RawQuery = q.Encode()
	}
	if r.URL.RawPath == "" && plainPath(r.URL.Path) && egressrules.MatchEndpoint(inj.Endpoints, r.Method, r.URL.Path) {
		r.Header.Set(inj.Header, inj.Value)
	}
}

// plainPath reports whether p is a clean path of the characters of API
// paths, so that a host that reads escapes, dot segments or parameters in
// it can't take it for another endpoint than the one it matched.
func plainPath(p string) bool {
	if path.Clean(p) != p || strings.Contains(p, "/.") {
		return false
	}
	return !strings.ContainsFunc(p, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/-_.:~@+", r))
	})
}
