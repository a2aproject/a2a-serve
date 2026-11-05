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

// Package meshadmin is a client of the Agent Mesh admin API that a relay
// manages its agentmesh-one with. The API speaks protojson with proto field
// names; the types here mirror the fields a2a-serve uses, so release builds
// don't need the generated mesh protos.
package meshadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RoleNode is the role of agents on the relay.
const RoleNode = "mesh:role:node"

// RoleRouter is the role of the relay's embedded router.
const RoleRouter = "mesh:role:router"

// SystemAuthenticated is the binding member of every identity an issuer
// Agent Mesh trusts vouches for.
const SystemAuthenticated = "mesh:system:authenticated"

// IngressPassthrough forwards requests to a public URL unchanged, so the
// agent's own auth applies there.
const IngressPassthrough = "INGRESS_AUTH_MODE_PASSTHROUGH"

// Policy is the mesh policy (api.PolicyConfig).
type Policy struct {
	Roles    []Role    `json:"roles"`
	Bindings []Binding `json:"bindings,omitempty"`
}

// Role is a role of the mesh policy (api.PolicyRole).
type Role struct {
	Name            string         `json:"name"`
	AllowedTargets  []string       `json:"allowed_targets,omitempty"`
	AllowedServices []string       `json:"allowed_services,omitempty"`
	AllowedPublish  []string       `json:"allowed_publish,omitempty"`
	AllowedIngress  []IngressGrant `json:"allowed_ingress,omitempty"`
}

// IngressGrant lets a role expose services at public URLs (api.IngressGrant).
type IngressGrant struct {
	Services         []string `json:"services"`
	AllowedAuthModes []string `json:"allowed_auth_modes,omitempty"`
}

// Binding gives members a role (api.PolicyBinding).
type Binding struct {
	Role    string   `json:"role"`
	Members []string `json:"members"`
}

// BootstrapTokenRequest mints a token that enrolls nodes
// (api.BootstrapTokenCreateRequest).
type BootstrapTokenRequest struct {
	Role               string `json:"role"`
	TTLHours           int    `json:"ttl_hours"`
	MaxUsages          int    `json:"max_usages"`
	Description        string `json:"description,omitempty"`
	AutonomousRecovery bool   `json:"autonomous_recovery,omitempty"`
}

// BootstrapToken is a minted token (api.BootstrapTokenCreateResponse).
type BootstrapToken struct {
	ID      string    `json:"id"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expire_time"`
}

// Status is what the admin API reports of the mesh (api.AdminStatusResponse).
type Status struct {
	Nodes   []Node                  `json:"enrolled_nodes"`
	Catalog map[string]NodeServices `json:"node_catalog"`
}

// Node is an enrolled node (api.EnrolledNode).
type Node struct {
	PeerID         string    `json:"peer_id"`
	Role           string    `json:"role"`
	EnrollmentType string    `json:"enrollment_type"`
	ClaimsJSON     string    `json:"claims_json"`
	Enrolled       time.Time `json:"enroll_time"`
	Banned         bool      `json:"banned"`
}

// Email returns the verified email of a node that signed in, or "".
func (n Node) Email() string {
	email, _ := n.account()
	return email
}

// Domain returns the Google Workspace domain of the account the node signed
// in with, or "".
func (n Node) Domain() string {
	email, domain := n.account()
	if email == "" {
		return ""
	}
	return domain
}

// account returns the verified email and the hd claim of the node's
// sign-in.
func (n Node) account() (email, domain string) {
	if n.ClaimsJSON == "" {
		return "", ""
	}
	var claims struct {
		Email    string `json:"email"`
		Verified *bool  `json:"email_verified"`
		Domain   string `json:"hd"`
	}
	if err := json.Unmarshal([]byte(n.ClaimsJSON), &claims); err != nil {
		return "", ""
	}
	if claims.Verified != nil && !*claims.Verified {
		return "", ""
	}
	return strings.ToLower(claims.Email), strings.ToLower(claims.Domain)
}

// NodeServices are the services a node last reported (api.NodeServices).
type NodeServices struct {
	Services []Service `json:"services"`
	Reported time.Time `json:"report_time"`
}

// ServiceTypeA2A is the type of the A2A services agents publish.
const ServiceTypeA2A = "SERVICE_TYPE_A2A"

// Service is a service a node reported (api.ServiceInfo).
type Service struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// Client calls the admin API of one agentmesh-one.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New returns a client of the agentmesh-one at base, which authenticates
// with the admin token.
func New(base, adminToken string, hc *http.Client) *Client {
	return &Client{base: strings.TrimSuffix(base, "/"), token: adminToken, hc: hc}
}

// Ready reports whether agentmesh-one is ready to serve.
func (c *Client) Ready(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/readyz", nil, nil)
}

// SetPolicy replaces the mesh policy.
func (c *Client) SetPolicy(ctx context.Context, p Policy) error {
	return c.do(ctx, http.MethodPost, "/policies", p, nil)
}

// CreateBootstrapToken mints a token that enrolls nodes.
func (c *Client) CreateBootstrapToken(ctx context.Context, req BootstrapTokenRequest) (*BootstrapToken, error) {
	var out BootstrapToken
	if err := c.do(ctx, http.MethodPost, "/admin/bootstrap-tokens", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Status returns the admitted and banned nodes and what they serve.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var out Status
	if err := c.do(ctx, http.MethodGet, "/admin/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Ban bans the node with peerID, and the identity it signed in with. Agent
// Mesh refuses its next request, and its peers drop it.
func (c *Client) Ban(ctx context.Context, peerID string) error {
	return c.do(ctx, http.MethodPost, "/admin/revoke", map[string]string{"peer_id": peerID}, nil)
}

// Unban lifts the ban of the node with peerID and of its identity.
func (c *Client) Unban(ctx context.Context, peerID string) error {
	return c.do(ctx, http.MethodPost, "/admin/nodes/"+url.PathEscape(peerID)+"/unban", nil, nil)
}

// maxResponse bounds what the client reads of a response. The status of a
// large relay is the biggest one.
const maxResponse = 32 << 20

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("calling Agent Mesh %s: %w", path, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("closing an Agent Mesh response", "error", err)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("calling Agent Mesh %s: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		return &Error{Path: path, Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("calling Agent Mesh %s: %w", path, err)
	}
	return nil
}

// Error is a failed admin call.
type Error struct {
	Path    string
	Status  int
	Message string
}

func (e *Error) Error() string {
	msg := e.Message
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return fmt.Sprintf("Agent Mesh %s: %s: %s", e.Path, http.StatusText(e.Status), msg)
}
