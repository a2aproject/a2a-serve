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

// Package controlapi is the API of a server's control socket, through
// which management commands change the running server without a restart.
package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/cardinfo"
	"github.com/a2aproject/a2a-serve/internal/ctlhttp"
	"github.com/a2aproject/a2a-serve/internal/project"
	"github.com/a2aproject/a2a-serve/internal/schedule"
)

// Status describes a running server.
type Status struct {
	Name    string    `json:"name"`
	Harness string    `json:"agent"`
	Dir     string    `json:"dir"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Local   string    `json:"local"`
	Private string    `json:"private,omitempty"`
	// PrivateNote explains a private URL people can't open, such as one on
	// an Agent Mesh relay.
	PrivateNote string `json:"privateNote,omitempty"`
	Public      string `json:"public,omitempty"`
	// PrivateProblem and PublicProblem say why the private or public URL
	// doesn't work now. A URL that won't work again is left out.
	PrivateProblem *NetworkProblem   `json:"privateProblem,omitempty"`
	PublicProblem  *NetworkProblem   `json:"publicProblem,omitempty"`
	Messengers     []MessengerStatus `json:"messengers,omitempty"`
	Tasks          map[string]int    `json:"tasks"`
	Owners         []string          `json:"owners,omitempty"`
	Contacts       int               `json:"contacts"`
	Nodes          int               `json:"nodes"`
	// Approvals counts the approvals waiting for an answer, which owners can
	// give in the inspector.
	Approvals int `json:"approvals"`
	// MCPServers and Skills are what the project brings for the harness.
	MCPServers []MCPServerStatus `json:"mcpServers,omitempty"`
	Skills     []project.Skill   `json:"skills,omitempty"`
	// ProjectError is why the project's MCP servers or skills couldn't be read.
	ProjectError string `json:"projectError,omitempty"`
	// AllowTools and DenyTools are the rules that decide tool calls without
	// asking.
	AllowTools []string      `json:"allowTools,omitempty"`
	DenyTools  []string      `json:"denyTools,omitempty"`
	Backup     *BackupStatus `json:"backup,omitempty"`
	Access     *Access       `json:"access,omitempty"`
}

// NetworkProblem says why a network doesn't reach the agent now.
type NetworkProblem struct {
	// State is a network.HealthState: removed, for a relay that removed
	// the agent, or disconnected, for a network the server reconnects to.
	State   string `json:"state"`
	Problem string `json:"problem"`
	Fix     string `json:"fix,omitempty"`
}

// Line is the problem and its fix in one line.
func (p *NetworkProblem) Line() string {
	if p.Fix == "" {
		return p.Problem
	}
	return p.Problem + "; " + p.Fix
}

// Access describes who may call the agent.
type Access struct {
	// PrivateAllowIncoming is everyone or paired, set with a private network;
	// PublicAllowIncoming is paired or anyone, set with a public URL.
	PrivateAllowIncoming string `json:"privateAllowIncoming,omitempty"`
	PublicAllowIncoming  string `json:"publicAllowIncoming,omitempty"`
	// Owner, Authenticated and Anonymous are the members of each role.
	Owner         []string `json:"owner"`
	Authenticated []string `json:"authenticated"`
	Anonymous     []string `json:"anonymous"`
	// Pairings counts the pairings besides the local one, and OwnerPairings
	// those of them that are owners.
	Pairings      int `json:"pairings"`
	OwnerPairings int `json:"ownerPairings"`
	// Others describes who may call without a pairing, such as everyone on
	// the tailnet.
	Others []string `json:"others,omitempty"`
	// Summary says who may call, one line each for you, everyone else and
	// the networks that need a pairing, as `a2a-serve status` shows it.
	Summary []string `json:"summary"`
	// Sandbox says what the agent containers of callers get, one line each
	// for you, the sandbox rules and everyone else.
	Sandbox []string `json:"sandbox,omitempty"`
	// SandboxNotes are what the sandbox rules don't do, or how they changed
	// what callers get.
	SandboxNotes []string `json:"sandboxNotes,omitempty"`
}

// MCPServerStatus describes an MCP server of the project's .mcp.json.
type MCPServerStatus struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Summary string `json:"summary"`
	// Missing are the variables it references which have no value, so the
	// harness doesn't get it.
	Missing []string `json:"missing,omitempty"`
	// Disabled is set when the config turns the server off.
	Disabled bool `json:"disabled,omitempty"`
}

// BackupStatus describes the agent's backup snapshots.
type BackupStatus struct {
	To        string    `json:"to"`
	Last      time.Time `json:"last,omitzero"`
	LastError string    `json:"lastError,omitempty"`
	Next      time.Time `json:"next,omitzero"`
}

// MessengerStatus describes a messenger of the agent.
type MessengerStatus struct {
	Name    string `json:"name"`
	Account string `json:"account,omitempty"`
	// Error is set while the messenger can't reach its messenger: it waits
	// to restart, or it reported a lost connection.
	Error string `json:"error,omitempty"`
	// OwnerLink is set when a messenger is added and nobody is linked on it
	// as owner yet.
	OwnerLink *OwnerLink `json:"ownerLink,omitempty"`
}

// OwnerLink is how the user running the agent links a messenger account as
// owner: Hint, such as a bot link, carries the one-time Code.
type OwnerLink struct {
	Hint    string    `json:"hint"`
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
}

// Line is the line that tells the user how to link as owner.
func (l *OwnerLink) Line() string {
	return fmt.Sprintf("→ link yourself as owner: %s   (%d min)", l.Hint, int(math.Round(time.Until(l.Expires).Minutes())))
}

// PairCodeRequest asks for a pairing code, for a new pairing or another
// device or account of an existing one.
//
// With Webhook, Name is a new webhook node. A Name which already is a node
// gets a new node code.
type PairCodeRequest struct {
	Name string `json:"name"`
	// Owner makes a new pairing an owner.
	Owner       bool              `json:"owner,omitempty"`
	Webhook     string            `json:"webhook,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Description string            `json:"description,omitempty"`
	// Advertised, when set, advertises the webhook node to the agents of the
	// private network, or stops advertising it.
	Advertised *bool `json:"advertised,omitempty"`
	// Sandbox, when set, changes the sandbox rule of the pairing.
	Sandbox *SandboxChange `json:"sandbox,omitempty"`
}

// SandboxChange changes the sandbox rule of a pairing: the rule whose only
// matcher is pairing:<name>.
type SandboxChange struct {
	// Storage, when set, replaces the storage of the rule.
	Storage string `json:"storage,omitempty"`
	// Secrets, when set, replace the secrets of the rule; empty gives none.
	Secrets *[]string `json:"secrets,omitempty"`
}

// PairingSandbox is what the agent container of a pairing gets.
type PairingSandbox struct {
	// Storage is shared, read-only, own or empty.
	Storage string `json:"storage"`
	// Secrets are the variables the agent gets: NAME, or NAME=secret:OTHER
	// for the stored secret OTHER passed as NAME.
	Secrets []string `json:"secrets"`
	// Summary describes both, as `a2a-serve pair get` shows it.
	Summary string `json:"summary"`
}

// PairCodeResponse carries a pairing code and how to use it.
type PairCodeResponse struct {
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
	URL     string    `json:"url"`
	// PairCommand is what the caller or the node runs to pair.
	PairCommand string `json:"pairCommand"`
	// Advertised is set when the request advertised the node.
	Advertised bool `json:"advertised,omitempty"`
	// Messenger is set when the agent has a messenger a person can link
	// with the code.
	Messenger bool `json:"messenger,omitempty"`
	// Hints tell a person how to link a messenger identity with the code.
	Hints []string `json:"hints,omitempty"`
	// Node is set for node codes.
	Node *NodeInfo `json:"node,omitempty"`
	// Instructions, set for node codes, is the message for the agent behind
	// the webhook: where the node's skill is and the code to pair with.
	Instructions string `json:"instructions,omitempty"`
	// Access is who may call the agent afterwards.
	Access *Access `json:"access,omitempty"`
	// Sandbox is what the agent container of the pairing gets.
	Sandbox *PairingSandbox `json:"sandbox,omitempty"`
}

// Pairing describes someone paired with the agent: a person, a device,
// another agent or a webhook node.
type Pairing struct {
	Name string `json:"name"`
	// Kind is one of the pairapi.Pairing* kinds, or empty until the first
	// code is redeemed.
	Kind string `json:"kind"`
	// Role is authenticated or owner.
	Role      string          `json:"role"`
	Principal string          `json:"principal"`
	Created   time.Time       `json:"created,omitzero"`
	LastUsed  time.Time       `json:"lastUsed,omitzero"`
	Tokens    []PairingToken  `json:"tokens"`
	Accounts  []LinkedAccount `json:"accounts"`
	// PendingCodes are the expiry times of the codes not used yet. Codes
	// are stored hashed, so they can't be shown again.
	PendingCodes []time.Time  `json:"pendingCodes"`
	Node         *NodeDetails `json:"node,omitempty"`
	// Allowed are the tool calls the pairing allowed in all its
	// conversations.
	Allowed []string `json:"allowed,omitempty"`
	// Sandbox is what the agent container of the pairing gets. Owners share
	// the container of the user running the agent.
	Sandbox *PairingSandbox `json:"sandbox,omitempty"`
}

// PairingToken describes a token issued to a pairing.
type PairingToken struct {
	URL      string    `json:"url"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed,omitzero"`
}

// LinkedAccount is a messenger account linked to a pairing.
type LinkedAccount struct {
	Messenger string `json:"messenger"`
	Account   string `json:"account"`
}

// NodeDetails describe the webhook of a node pairing and its mailbox.
type NodeDetails struct {
	URL         string `json:"url"`
	Webhook     string `json:"webhook"`
	Description string `json:"description,omitempty"`
	// HeaderNames are the names of the headers sent with every wake.
	HeaderNames []string `json:"headerNames,omitempty"`
	Advertised  bool     `json:"advertised"`
	Paired      bool     `json:"paired"`
	// Waiting counts the wakes not delivered yet, and Open the tasks
	// waiting for a reply. Both are zero while the agent is stopped.
	Waiting int `json:"waiting"`
	Open    int `json:"open"`
}

// PairingUpdate changes a pairing in place. Unset fields are kept.
type PairingUpdate struct {
	// Owner, when set, makes the pairing an owner or not.
	Owner *bool `json:"owner,omitempty"`
	// Kind changes the kind of a pairing other than a webhook node.
	Kind        string            `json:"kind,omitempty"`
	Webhook     string            `json:"webhook,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Description *string           `json:"description,omitempty"`
	Advertised  *bool             `json:"advertised,omitempty"`
	// Rotate revokes every token of the pairing and issues a new code.
	Rotate bool `json:"rotate,omitempty"`
	// Sandbox, when set, changes the sandbox rule of the pairing.
	Sandbox *SandboxChange `json:"sandbox,omitempty"`
}

// PairingUpdateResponse is the changed pairing, and the new code of a
// rotation.
type PairingUpdateResponse struct {
	Pairing Pairing           `json:"pairing"`
	Code    *PairCodeResponse `json:"code,omitempty"`
	// Access is who may call the agent afterwards.
	Access *Access `json:"access,omitempty"`
}

// RevokeResponse tells what `pair rm` removed.
type RevokeResponse struct {
	// Kind is the kind of the pairing.
	Kind string `json:"kind"`
	// StillAllowed describes who may call the agent without a pairing,
	// such as everyone on the private network, which can include the
	// person whose pairing was removed.
	StillAllowed []string `json:"stillAllowed,omitempty"`
	// Kept is set when the pairing stays and only its credentials were
	// revoked, as for the local pairing.
	Kept bool `json:"kept,omitempty"`
	// Access is who may call the agent afterwards.
	Access *Access `json:"access,omitempty"`
}

// RemovalPreview tells what `pair rm` would remove, and removes nothing.
type RemovalPreview struct {
	// Kind is the kind of the pairing.
	Kind string `json:"kind"`
	// Kept is set when the pairing stays and only its credentials would be
	// revoked, as for the local pairing.
	Kept   bool `json:"kept,omitempty"`
	Tokens int  `json:"tokens"`
	// Accounts are the messenger accounts linked to the pairing.
	Accounts []LinkedAccount `json:"accounts"`
	// Codes is the number of pairing codes that would be deleted.
	Codes int `json:"codes"`
	// Node is set when a webhook node would be removed.
	Node      bool `json:"node,omitempty"`
	Schedules int  `json:"schedules"`
	Watches   int  `json:"watches"`
	Approvals int  `json:"approvals"`
	Tasks     int  `json:"tasks"`
	// RunningTasks is the number of Tasks that would be canceled first.
	RunningTasks  int `json:"runningTasks"`
	Conversations int `json:"conversations"`
	// Allowed is the number of tool calls allowed in all conversations.
	Allowed int `json:"allowed"`
	// AgentHomes are the agent home directories that would be deleted.
	AgentHomes []string `json:"agentHomes"`
}

// SignOutResponse tells what `pair rm --email` revoked.
type SignOutResponse struct {
	Email string `json:"email"`
	// Tokens is the number of Google sign-in tokens revoked.
	Tokens int `json:"tokens"`
	// Access is who may call the agent afterwards.
	Access *Access `json:"access,omitempty"`
}

// Peer is an agent found on the private network.
type Peer struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	Online      bool   `json:"online"`
}

// ContactRequest adds a contact. Without URL, the contact is looked up by
// name among the peers on the private network. Code pairs with the contact;
// Token sets the outbound token directly.
type ContactRequest struct {
	Name  string `json:"name"`
	URL   string `json:"url,omitempty"`
	Code  string `json:"code,omitempty"`
	Token string `json:"token,omitempty"`
}

// ContactInfo describes a contact.
type ContactInfo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Via is the route to the contact: private for the private network,
	// direct for an ordinary connection. It doesn't depend on the URL scheme.
	Via      string `json:"via"`
	HasToken bool   `json:"hasToken"`
	// Description and Skills are what the contact's agent card said when
	// it was last fetched.
	Description string           `json:"description,omitempty"`
	Skills      []cardinfo.Skill `json:"skills,omitempty"`
}

// ContactRefreshRequest fetches the card of the contact Name, or of every
// contact without one.
type ContactRefreshRequest struct {
	Name string `json:"name,omitempty"`
}

// MessengerRequest adds or replaces a messenger. Token is a secret reference
// already stored in the secrets file.
type MessengerRequest struct {
	Kind  string                `json:"kind"`
	Token string                `json:"token,omitempty"`
	Email *agentdir.EmailConfig `json:"email,omitempty"`
	// Secrets are new secret values the messenger refers to. They are saved
	// with the messenger once it is validated.
	Secrets agentdir.Secrets `json:"secrets,omitempty"`
}

// NodeInfo describes a webhook node.
type NodeInfo struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	URL     string    `json:"url"`
	Webhook string    `json:"webhook"`
	Paired  bool      `json:"paired"`
	Created time.Time `json:"created"`
}

// MessengerRequestFromConfig is the request which starts the messenger of kind
// as configured in conf.
func MessengerRequestFromConfig(conf *agentdir.Config, kind string) MessengerRequest {
	return MessengerRequest{Kind: kind, Token: conf.MessengerToken(kind), Email: conf.Messengers.Email}
}

// WriteError writes err as the response of a failed control call, with the
// status of its [agenterr] type; [Client] turns it back into that type.
func WriteError(w http.ResponseWriter, err error) { ctlhttp.WriteError(w, err) }

// ErrUnsupported is returned when the running server is older than the
// client and doesn't know the call.
var ErrUnsupported = errors.New("the running server doesn't support this call; restart it with `a2a-serve restart`")

// ReloadResponse lists the settings a server applied from config.yaml.
type ReloadResponse struct {
	Applied []string `json:"applied"`
}

// Client talks to a running server over its control socket.
type Client struct {
	dir agentdir.Dir
	hc  *http.Client
}

// DefaultTimeout bounds a control call unless WithTimeout changes it.
const DefaultTimeout = 60 * time.Second

// Option configures a Client.
type Option func(*Client)

// WithTimeout bounds each control call by d; zero keeps DefaultTimeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.hc.Timeout = d
		}
	}
}

// NewClient returns a client for the agent in dir.
func NewClient(dir agentdir.Dir, opts ...Option) *Client {
	path := dir.SocketPath()
	c := &Client{dir: dir, hc: &http.Client{Timeout: DefaultTimeout, Transport: ctlhttp.SocketTransport(path)}}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctlhttp.IsDialError(err) {
			return agenterr.ErrNotRunning
		}
		// The URL of the control socket is internal.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("the server did not answer: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("closing response", "error", err)
		}
	}()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return ErrUnsupported
	}
	if resp.StatusCode/100 != 2 {
		return ctlhttp.ResponseError(resp.Status, resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Status returns the status of the server.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var out Status
	return &out, c.do(ctx, http.MethodGet, "/status", nil, &out)
}

// Stop stops the server.
func (c *Client) Stop(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/stop", nil, nil)
}

// Config returns the config the server runs with, which differs from
// config.yaml by the changes waiting for a restart.
func (c *Client) Config(ctx context.Context) (*agentdir.Config, error) {
	var out agentdir.Config
	return &out, c.do(ctx, http.MethodGet, "/config", nil, &out)
}

// ReloadConfig makes the server apply the settings of config.yaml it can
// apply without a restart, other than messengers.
func (c *Client) ReloadConfig(ctx context.Context) (*ReloadResponse, error) {
	var out ReloadResponse
	return &out, c.do(ctx, http.MethodPost, "/config/reload", nil, &out)
}

// Pair mints a pairing code.
func (c *Client) Pair(ctx context.Context, req PairCodeRequest) (*PairCodeResponse, error) {
	var out PairCodeResponse
	return &out, c.do(ctx, http.MethodPost, "/pair", req, &out)
}

// Revoke removes the webhook node called name, or revokes the tokens and
// links issued under it.
func (c *Client) Revoke(ctx context.Context, name string) (*RevokeResponse, error) {
	var out RevokeResponse
	return &out, c.do(ctx, http.MethodDelete, "/pair/"+url.PathEscape(name), nil, &out)
}

// PreviewRevoke describes what Revoke would remove, and removes nothing.
func (c *Client) PreviewRevoke(ctx context.Context, name string) (*RemovalPreview, error) {
	var out RemovalPreview
	return &out, c.do(ctx, http.MethodGet, "/pair/"+url.PathEscape(name)+"/removal", nil, &out)
}

// SignOut revokes the Google sign-in tokens of email.
func (c *Client) SignOut(ctx context.Context, email string) (*SignOutResponse, error) {
	var out SignOutResponse
	return &out, c.do(ctx, http.MethodDelete, "/signins/"+url.PathEscape(email), nil, &out)
}

// Pairings lists the callers and webhook nodes paired with the agent.
func (c *Client) Pairings(ctx context.Context) ([]Pairing, error) {
	var out []Pairing
	return out, c.do(ctx, http.MethodGet, "/pairings", nil, &out)
}

// Pairing describes the pairing called name.
func (c *Client) Pairing(ctx context.Context, name string) (*Pairing, error) {
	var out Pairing
	return &out, c.do(ctx, http.MethodGet, "/pairings/"+url.PathEscape(name), nil, &out)
}

// UpdatePairing changes the pairing called name.
func (c *Client) UpdatePairing(ctx context.Context, name string, req PairingUpdate) (*PairingUpdateResponse, error) {
	var out PairingUpdateResponse
	return &out, c.do(ctx, http.MethodPatch, "/pairings/"+url.PathEscape(name), req, &out)
}

// Peers lists agents found on the private network.
func (c *Client) Peers(ctx context.Context) ([]Peer, error) {
	var out []Peer
	return out, c.do(ctx, http.MethodGet, "/peers", nil, &out)
}

// Card returns the agent card served on the listener kind: local, private
// or public.
func (c *Client) Card(ctx context.Context, kind string) (*a2a.AgentCard, error) {
	var out a2a.AgentCard
	return &out, c.do(ctx, http.MethodGet, "/card?listener="+url.QueryEscape(kind), nil, &out)
}

// Contacts lists contacts.
func (c *Client) Contacts(ctx context.Context) ([]ContactInfo, error) {
	var out []ContactInfo
	return out, c.do(ctx, http.MethodGet, "/contacts", nil, &out)
}

// AddContact adds a contact.
func (c *Client) AddContact(ctx context.Context, req ContactRequest) (*ContactInfo, error) {
	var out ContactInfo
	return &out, c.do(ctx, http.MethodPost, "/contacts", req, &out)
}

// RefreshContacts fetches the cards of contacts again.
func (c *Client) RefreshContacts(ctx context.Context, req ContactRefreshRequest) ([]ContactInfo, error) {
	var out []ContactInfo
	return out, c.do(ctx, http.MethodPost, "/contacts/refresh", req, &out)
}

// DeleteContact removes a contact.
func (c *Client) DeleteContact(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/contacts/"+url.PathEscape(name), nil, nil)
}

// AddMessenger adds or replaces a messenger.
func (c *Client) AddMessenger(ctx context.Context, req MessengerRequest) (*MessengerStatus, error) {
	var out MessengerStatus
	return &out, c.do(ctx, http.MethodPost, "/messengers", req, &out)
}

// DeleteMessenger removes a messenger.
func (c *Client) DeleteMessenger(ctx context.Context, kind string) error {
	return c.do(ctx, http.MethodDelete, "/messengers/"+url.PathEscape(kind), nil, nil)
}

// ScheduleInfo describes a schedule.
type ScheduleInfo struct {
	Name   string    `json:"name"`
	Cron   string    `json:"cron,omitempty"`
	At     time.Time `json:"at,omitzero"`
	TZ     string    `json:"tz,omitempty"`
	Prompt string    `json:"prompt"`
	// User is the principal the runs are for.
	User string `json:"user"`
	// Chat is the chat the runs are shown in, <messenger>:<who>.
	Chat      string    `json:"chat,omitempty"`
	Thread    string    `json:"thread"`
	CreatedBy string    `json:"createdBy"`
	NextRun   time.Time `json:"nextRun,omitzero"`
	LastRun   time.Time `json:"lastRun,omitzero"`
	LastError string    `json:"lastError,omitempty"`
}

// Schedules lists the schedules.
func (c *Client) Schedules(ctx context.Context) ([]ScheduleInfo, error) {
	var out []ScheduleInfo
	return out, c.do(ctx, http.MethodGet, "/schedules", nil, &out)
}

// AddSchedule adds a schedule.
func (c *Client) AddSchedule(ctx context.Context, req schedule.Request) (*ScheduleInfo, error) {
	var out ScheduleInfo
	return &out, c.do(ctx, http.MethodPost, "/schedules", req, &out)
}

// RunSchedule starts a run of a schedule now.
func (c *Client) RunSchedule(ctx context.Context, name string) (*ScheduleInfo, error) {
	var out ScheduleInfo
	return &out, c.do(ctx, http.MethodPost, "/schedules/"+url.PathEscape(name)+"/run", nil, &out)
}

// DeleteSchedule removes a schedule.
func (c *Client) DeleteSchedule(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/schedules/"+url.PathEscape(name), nil, nil)
}

// Advertisement is a pairing another agent of the private network
// advertises, which this agent can add as the contact Key.
type Advertisement struct {
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Host        string    `json:"host"`
	Description string    `json:"description,omitempty"`
	Expires     time.Time `json:"expires"`
}

// Advertisements lists the pairings other agents advertise.
func (c *Client) Advertisements(ctx context.Context) ([]Advertisement, error) {
	var out []Advertisement
	return out, c.do(ctx, http.MethodGet, "/advertisements", nil, &out)
}

// InspectorLogin opens the web inspector once: Path is relative to the
// agent's local or private URL.
type InspectorLogin struct {
	Path string `json:"path"`
}

// InspectorLogin mints a one-time login link of the web inspector.
func (c *Client) InspectorLogin(ctx context.Context) (*InspectorLogin, error) {
	var out InspectorLogin
	return &out, c.do(ctx, http.MethodPost, "/inspector/login", nil, &out)
}
