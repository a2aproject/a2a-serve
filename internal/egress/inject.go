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
	"net/http"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// HTTPSPort is the port of the hosts that a header rule names without one.
const HTTPSPort = 443

// Provider is a model provider whose API key the server can add to the
// sandbox's requests.
type Provider struct {
	Name string
	// Host is the API host, reached on HTTPSPort.
	Host string
	// Header carries an API key, after Scheme.
	Header, Scheme string
	// TokenHeader and TokenScheme carry the keys of TokenKeys, such as
	// OAuth tokens.
	TokenHeader, TokenScheme string
	TokenKeys                []string
	// QueryKey is the query parameter that some clients put the key in.
	QueryKey string
	// Endpoints are the requests that get the key, as "METHOD pattern",
	// where the pattern is a path in the syntax of [path.Match], so that *
	// stands for one segment or part of one. They are the inference
	// endpoints and the model lists. Other endpoints of the API, such as
	// files, batches and keys, can read the data that other apps stored
	// with the key, or make new keys.
	Endpoints []string
	// prefix and tokenPrefix start the placeholders, so that the format
	// checks of the provider's SDKs accept them.
	prefix, tokenPrefix string
}

var providers = []Provider{
	{
		Name: "anthropic", Host: "api.anthropic.com", Header: "X-Api-Key",
		TokenHeader: "Authorization", TokenScheme: "Bearer ", TokenKeys: []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"},
		Endpoints: []string{"POST /v1/messages", "POST /v1/messages/count_tokens", "GET /v1/models", "GET /v1/models/*"},
		prefix:    "sk-ant-api03-", tokenPrefix: "sk-ant-oat01-",
	},
	{
		Name: "gemini", Host: "generativelanguage.googleapis.com", Header: "X-Goog-Api-Key", QueryKey: "key",
		Endpoints: geminiEndpoints(),
		prefix:    "AIza",
	},
	{
		Name: "openai", Host: "api.openai.com", Header: "Authorization", Scheme: "Bearer ",
		Endpoints: []string{"POST /v1/responses", "POST /v1/chat/completions", "POST /v1/completions", "POST /v1/embeddings", "GET /v1/models", "GET /v1/models/*"},
		prefix:    "sk-proj-",
	},
	{
		Name: "openrouter", Host: "openrouter.ai", Header: "Authorization", Scheme: "Bearer ",
		Endpoints: []string{"POST /api/v1/chat/completions", "POST /api/v1/completions", "POST /api/v1/embeddings", "GET /api/v1/models"},
		prefix:    "sk-or-v1-",
	},
}

// geminiEndpoints are the inference endpoints and the model lists of the
// Gemini API, in both of its versions; batch jobs and operations stay out.
func geminiEndpoints() []string {
	var out []string
	for _, v := range []string{"/v1beta", "/v1"} {
		for _, action := range []string{"generateContent", "streamGenerateContent", "countTokens", "embedContent", "batchEmbedContents"} {
			out = append(out, "POST "+v+"/models/*:"+action)
		}
		out = append(out, "GET "+v+"/models", "GET "+v+"/models/*")
	}
	return out
}

// MatchEndpoint reports whether a request with method to p is one of
// endpoints, in the form of [Provider.Endpoints].
func MatchEndpoint(endpoints []string, method, p string) bool {
	for _, e := range endpoints {
		m, pattern, _ := strings.Cut(e, " ")
		if ok, err := path.Match(pattern, p); m == method && err == nil && ok {
			return true
		}
	}
	return false
}

// LookupProvider returns the model provider name.
func LookupProvider(name string) (Provider, bool) {
	i := slices.IndexFunc(providers, func(p Provider) bool { return p.Name == name })
	if i < 0 {
		return Provider{}, false
	}
	return providers[i], true
}

// ProviderNames returns the names of the model providers.
func ProviderNames() []string {
	out := make([]string, len(providers))
	for i, p := range providers {
		out[i] = p.Name
	}
	return out
}

// UnknownProvider is the error of a model provider the server doesn't know.
func UnknownProvider(name string) error {
	return fmt.Errorf("unknown model provider %q: want %s; use --inject-header for another host", name, strings.Join(ProviderNames(), ", "))
}

// Credential returns where the provider takes the variable key: the header
// and the scheme before its value.
func (p Provider) Credential(key string) (header, scheme string) {
	if slices.Contains(p.TokenKeys, key) {
		return p.TokenHeader, p.TokenScheme
	}
	return p.Header, p.Scheme
}

// Placeholder returns the value the sandbox gets for the variable key in
// place of the key.
func (p Provider) Placeholder(key string) string {
	if slices.Contains(p.TokenKeys, key) {
		return p.tokenPrefix + placeholderText
	}
	return p.prefix + placeholderText
}

// Placeholder is the value the sandbox gets for a secret of a header rule.
const Placeholder = placeholderText

const placeholderText = "a2a-serve-placeholder"

// CredentialHeaders are the headers that carry API keys. The server
// removes them from a request to a model provider and sets its own.
var CredentialHeaders = []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key"}

// HeaderRule is a header the server adds to the sandbox's requests to a
// host: Name: Value, with $NAME or ${NAME} standing for the secret NAME.
type HeaderRule struct {
	Host string
	Port uint16
	Name string
	// Value holds the references to secrets.
	Value string
}

// Target returns host, or host:port for a port other than HTTPSPort.
func (r HeaderRule) Target() string { return target(r.Host, r.Port) }

// String returns the rule as --inject-header takes it.
func (r HeaderRule) String() string { return r.Target() + "=" + r.Name + ": " + r.Value }

func target(host string, port uint16) string {
	if port == HTTPSPort {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

// Secrets returns the names of the secrets that the value references, in
// order and without repeats.
func (r HeaderRule) Secrets() []string {
	var out []string
	for _, m := range secretRef.FindAllStringSubmatch(r.Value, -1) {
		if name := m[1] + m[2]; !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// Expand returns the value with each reference replaced by value(name).
func (r HeaderRule) Expand(value func(name string) string) string {
	return secretRef.ReplaceAllStringFunc(r.Value, func(ref string) string {
		m := secretRef.FindStringSubmatch(ref)
		return value(m[1] + m[2])
	})
}

var secretRef = regexp.MustCompile(`\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))`)

// ParseHeaderRule parses an --inject-header rule: host[:port]=Name: value,
// such as "api.github.com=Authorization: Bearer $GITHUB_TOKEN".
func ParseHeaderRule(s string) (HeaderRule, error) {
	dest, header, ok := strings.Cut(strings.TrimSpace(s), "=")
	if !ok {
		return HeaderRule{}, fmt.Errorf("invalid header rule %q: want host=Name: value, such as 'api.github.com=Authorization: Bearer $GITHUB_TOKEN'", s)
	}
	host, port, err := ParseInjectTarget(dest)
	if err != nil {
		return HeaderRule{}, fmt.Errorf("invalid header rule %q: %w", s, err)
	}
	name, value, ok := strings.Cut(header, ":")
	name, value = strings.TrimSpace(name), strings.TrimSpace(value)
	r := HeaderRule{Host: host, Port: port, Name: http.CanonicalHeaderKey(name), Value: value}
	switch {
	case !ok || !httpguts.ValidHeaderFieldName(name):
		return HeaderRule{}, fmt.Errorf("invalid header rule %q: want a header Name: value after the host", s)
	case slices.Contains(reservedHeaders, r.Name) || strings.HasPrefix(r.Name, "Proxy-"):
		return HeaderRule{}, fmt.Errorf("invalid header rule %q: the server can't set the header %s", s, r.Name)
	case !httpguts.ValidHeaderFieldValue(value):
		return HeaderRule{}, fmt.Errorf("invalid header rule %q: the value has a control character", s)
	case len(r.Secrets()) == 0:
		return HeaderRule{}, fmt.Errorf("invalid header rule %q: the value names no secret; write it as $NAME, and quote the rule in single quotes so that the shell keeps $NAME", s)
	case strings.Count(value, "$") != len(secretRef.FindAllString(value, -1)):
		return HeaderRule{}, fmt.Errorf("invalid header rule %q: a $ starts a $NAME or ${NAME}", s)
	}
	return r, nil
}

// reservedHeaders frame the request or the connection, which the server
// writes itself.
var reservedHeaders = []string{"Host", "Content-Length", "Transfer-Encoding", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"}

// ParseInjectTarget parses the host[:port] of a rule that adds a header: a
// host name, with the HTTPS port by default.
func ParseInjectTarget(s string) (string, uint16, error) {
	s = strings.TrimSpace(s)
	host, port := s, HTTPSPort
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := ParsePort(p)
		if err != nil {
			return "", 0, err
		}
		host, port = h, int(n)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return "", 0, errors.New("want a host name; the server can't add headers for an IP address")
	}
	if strings.HasPrefix(host, "*.") {
		return "", 0, errors.New("want one host name; name each host in a rule of its own")
	}
	name, err := NormalizeHost(host)
	if err != nil {
		return "", 0, err
	}
	return name, uint16(port), nil
}
