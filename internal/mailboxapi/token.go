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

package mailboxapi

import (
	"encoding/base64"
	"errors"
	"os"
	"strings"
)

// EnvNodeToken is the environment variable the node commands read the node
// token from.
const EnvNodeToken = "A2A_NODE_TOKEN"

// Prefix is the path of the node API and the node skill on an agent server.
const Prefix = "/mailbox"

// EnvToken returns the node token of $A2A_NODE_TOKEN.
func EnvToken() string {
	return os.Getenv(EnvNodeToken)
}

// APIBase turns an agent URL into its node API base.
func APIBase(agentURL string) string {
	u := strings.TrimSuffix(agentURL, "/")
	if strings.HasSuffix(u, Prefix) {
		return u
	}
	return u + Prefix
}

// NodeClient returns a client which acts as the node of token. The token
// carries the URL of the agent server hosting the node.
func NodeClient(token string) (*Client, error) {
	base, _, ok := ParseNodeToken(token)
	if !ok {
		return nil, errors.New("not a node token")
	}
	return &Client{BaseURL: base, Token: token}, nil
}

// A node token is a2rn.<base64url node API base>.<node ID>.<secret>, so a
// node needs a single secret to reach the agent server hosting it.
const nodeTokenPrefix = "a2rn."

// NewNodeToken returns a node token.
func NewNodeToken(apiBase, nodeID, secret string) string {
	return nodeTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(apiBase)) + "." + nodeID + "." + secret
}

// AgentURL returns the URL of the agent server hosting the node of token.
func AgentURL(token string) (string, bool) {
	base, _, ok := ParseNodeToken(token)
	if !ok {
		return "", false
	}
	return strings.TrimSuffix(base, Prefix), true
}

// ParseNodeToken returns the node API base and node ID embedded in a node token.
func ParseNodeToken(token string) (apiBase, nodeID string, ok bool) {
	rest, ok := strings.CutPrefix(token, nodeTokenPrefix)
	if !ok {
		return "", "", false
	}
	// Node IDs may contain dots; base64url and the secret don't.
	enc, rest, ok := strings.Cut(rest, ".")
	if !ok {
		return "", "", false
	}
	i := strings.LastIndex(rest, ".")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	u, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", "", false
	}
	return string(u), rest[:i], true
}
