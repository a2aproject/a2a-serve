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

// Package relay runs a relay: one a2a-serve process that serves the relay's
// public port and runs Agent Mesh's agentmesh-one behind it on loopback.
package relay

import (
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/relay/meshadmin"
	"github.com/a2aproject/a2a-serve/internal/relay/signin"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// SignInSession is how long a signed-in agent stays on the relay without
// a fresh ID token. Agents renew it with theirs every [signin.IDTokenTTL],
// and the relay bans agents its allowlist drops when it starts.
const SignInSession = 24 * time.Hour

// MeshConfig is what agentmesh-one runs with.
type MeshConfig struct {
	// URL is the relay's public URL, which the front serves.
	URL     string
	Port    int
	DataDir string
	// AdminToken authenticates the front on the admin API. On a relay
	// whose data directory doesn't last, it also keeps the router's peer ID.
	AdminToken string
	// Database is a PostgreSQL DSN, or "" for SQLite in DataDir.
	Database string
	// Issuer is the URL of the relay's identity provider, or "" when
	// agents can't sign in.
	Issuer string
}

// MeshArgs returns the flags of agentmesh-one. No other code builds them.
func MeshArgs(c MeshConfig) []string {
	args := []string{
		"--bind-address", "127.0.0.1",
		"--port", strconv.Itoa(c.Port),
		"--external-url", c.URL,
		"--data-dir", c.DataDir,
		// Agents join by sign-in or with an invite.
		"--no-join-token",
		"--enroll-qr=false",
		"--router-allow-loopback=" + strconv.FormatBool(relaydir.Loopback(c.URL)),
	}
	if c.Database != "" {
		args = append(args, "--db-driver", "postgres")
	}
	if c.Issuer != "" {
		// A workload issuer's tokens may enroll nodes, and Agent Mesh refuses
		// them on its user API, which would let anyone signed in mint join
		// tokens past the allowlist. --issuer advertises it on /info.
		args = append(args,
			"--issuer", c.Issuer, "--workload-issuer", c.Issuer,
			"--oidc-client-id", signin.ClientID, "--allowed-audiences", signin.ClientID,
			"--control-plane-workload-session-ttl", SignInSession.String(),
		)
	}
	return args
}

// MeshEnv returns the environment of agentmesh-one: base without Agent
// Mesh's own settings, and the secrets, which don't belong on a command line.
func MeshEnv(c MeshConfig, base []string) []string {
	var env []string
	for _, kv := range base {
		if !strings.HasPrefix(kv, "AGENTMESH_") {
			env = append(env, kv)
		}
	}
	env = append(env, "AGENTMESH_ADMIN_TOKEN="+c.AdminToken)
	if c.Database != "" {
		env = append(env, "AGENTMESH_DB_DSN="+c.Database)
	}
	return env
}

// Policy returns the mesh policy. Every node may call, publish and expose
// A2A services, the one kind an agent serves. Public URLs forward requests
// unchanged, so the agent's own auth applies there. With signIn, everyone
// the relay's identity provider vouches for joins as a node; it is the only
// issuer Agent Mesh trusts, and it checks the allowlist.
func Policy(signIn bool) meshadmin.Policy {
	p := meshadmin.Policy{Roles: []meshadmin.Role{
		{Name: meshadmin.RoleRouter, AllowedServices: []string{"*"}, AllowedTargets: []string{"*"}},
		{
			Name:            meshadmin.RoleNode,
			AllowedServices: []string{"a2a://*"},
			AllowedTargets:  []string{"*"},
			AllowedPublish:  []string{"a2a://*"},
			AllowedIngress: []meshadmin.IngressGrant{{
				Services:         []string{"a2a://*"},
				AllowedAuthModes: []string{meshadmin.IngressPassthrough},
			}},
		},
	}}
	if signIn {
		p.Bindings = []meshadmin.Binding{{Role: meshadmin.RoleNode, Members: []string{meshadmin.SystemAuthenticated}}}
	}
	return p
}
