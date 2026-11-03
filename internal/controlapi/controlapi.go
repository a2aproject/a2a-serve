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

// Package controlapi is the API of an agent server's control socket, through
// which management commands change the running server without a restart.
package controlapi

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/project"
	"github.com/a2aproject/a2a-serve/internal/schedule"
)

// Status describes a running agent server.
type Status struct {
	Name    string    `json:"name"`
	Harness string    `json:"harness"`
	Dir     string    `json:"dir"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Local   string    `json:"local"`
	Private string    `json:"private,omitempty"`
	// PrivateNote explains a private URL people can't open, such as one on
	// an Agent Mesh relay.
	PrivateNote string          `json:"privateNote,omitempty"`
	Public      string          `json:"public,omitempty"`
	Channels    []ChannelStatus `json:"channels,omitempty"`
	Tasks       map[string]int  `json:"tasks"`
	Owners      []string        `json:"owners,omitempty"`
	Contacts    int             `json:"contacts"`
	Nodes       int             `json:"nodes"`
	// MCPServers and Skills are what the project brings for the harness.
	MCPServers []MCPServerStatus `json:"mcpServers,omitempty"`
	Skills     []project.Skill   `json:"skills,omitempty"`
	// ProjectError is why the project's MCP servers or skills couldn't be read.
	ProjectError string        `json:"projectError,omitempty"`
	Backup       *BackupStatus `json:"backup,omitempty"`
}

// MCPServerStatus describes an MCP server of the project's .mcp.json.
type MCPServerStatus struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Summary string `json:"summary"`
	// Missing are the variables it references which have no value, so the
	// harness doesn't get it.
	Missing []string `json:"missing,omitempty"`
}

// BackupStatus describes the agent's backup snapshots.
type BackupStatus struct {
	To        string    `json:"to"`
	Last      time.Time `json:"last,omitzero"`
	LastError string    `json:"lastError,omitempty"`
	Next      time.Time `json:"next,omitzero"`
}

// URL scopes of an agent.
const (
	ScopeLocal   = "local"
	ScopePrivate = "private"
	ScopePublic  = "public"
)

// URL returns the agent's URL in scope, or with an empty scope the one
// which reaches furthest: public, then private, then local.
func (s *Status) URL(scope string) (string, error) {
	switch scope {
	case "":
		return cmp.Or(s.Public, s.Private, s.Local), nil
	case ScopeLocal:
		return s.Local, nil
	case ScopePrivate:
		if s.Private == "" {
			return "", fmt.Errorf("%s has no private URL; add one with `a2a serve config set --private tailscale` (or lan), then `a2a serve restart`", s.Name)
		}
		return s.Private, nil
	case ScopePublic:
		if s.Public == "" {
			return "", fmt.Errorf("%s has no public URL; add one with `a2a serve config set --public tailscale` (or ngrok), then `a2a serve restart`", s.Name)
		}
		return s.Public, nil
	}
	return "", fmt.Errorf("unknown URL scope %q", scope)
}

// LocalOnly reports whether rawURL only works on this machine.
func LocalOnly(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Hostname() == "localhost" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

// ChannelStatus describes a channel of the agent.
type ChannelStatus struct {
	Name    string `json:"name"`
	Account string `json:"account,omitempty"`
	// Error is set while the channel is failing and waits to restart.
	Error string `json:"error,omitempty"`
}

// PairCodeRequest asks for a pairing code, for a new pairing or another
// device or account of an existing one.
//
// With Webhook, Name is a new webhook node. A Name which already is a node
// gets a new node code.
type PairCodeRequest struct {
	Name        string            `json:"name"`
	Role        string            `json:"role,omitempty"`
	Webhook     string            `json:"webhook,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Description string            `json:"description,omitempty"`
	// Advertised, when set, advertises the webhook node to the agents of the
	// private network, or stops advertising it.
	Advertised *bool `json:"advertised,omitempty"`
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
	// Hints tell a person how to link a channel identity with the code.
	Hints []string `json:"hints,omitempty"`
	// Node is set for node codes.
	Node *NodeInfo `json:"node,omitempty"`
	// Instructions, set for node codes, is the message for the agent behind
	// the webhook: where the node's skill is and the code to pair with.
	Instructions string `json:"instructions,omitempty"`
}

// Pairing describes a caller or a webhook node paired with the agent.
type Pairing struct {
	Name string `json:"name"`
	// Kind is [pairapi.KindCaller] or [pairapi.KindNode].
	Kind string `json:"kind"`
	// Role is member or owner, for callers.
	Role      string          `json:"role,omitempty"`
	Principal string          `json:"principal"`
	Created   time.Time       `json:"created,omitzero"`
	LastUsed  time.Time       `json:"lastUsed,omitzero"`
	Tokens    []PairingToken  `json:"tokens"`
	Accounts  []LinkedAccount `json:"accounts"`
	// PendingCodes are the expiry times of the codes not used yet. Codes
	// are stored hashed, so they can't be shown again.
	PendingCodes []time.Time  `json:"pendingCodes"`
	Node         *NodeDetails `json:"node,omitempty"`
}

// PairingToken describes a token issued to a pairing.
type PairingToken struct {
	URL      string    `json:"url"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed,omitzero"`
}

// LinkedAccount is a messenger account linked to a pairing.
type LinkedAccount struct {
	Channel string `json:"channel"`
	Account string `json:"account"`
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
	Role        string            `json:"role,omitempty"`
	Webhook     string            `json:"webhook,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Description *string           `json:"description,omitempty"`
	Advertised  *bool             `json:"advertised,omitempty"`
	// Rotate revokes every token of the pairing and issues a new code.
	Rotate bool `json:"rotate,omitempty"`
}

// PairingUpdateResponse is the changed pairing, and the new code of a
// rotation.
type PairingUpdateResponse struct {
	Pairing Pairing           `json:"pairing"`
	Code    *PairCodeResponse `json:"code,omitempty"`
}

// RevokeResponse tells what `pair rm` removed.
type RevokeResponse struct {
	// Kind is [pairapi.KindCaller] or [pairapi.KindNode].
	Kind string `json:"kind"`
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
	Name     string `json:"name"`
	URL      string `json:"url"`
	Via      string `json:"via"`
	HasToken bool   `json:"hasToken"`
}

// ChannelRequest adds or replaces a channel. Token is a secret reference
// already stored in the secrets file.
type ChannelRequest struct {
	Kind  string                `json:"kind"`
	Token string                `json:"token,omitempty"`
	Email *agentdir.EmailConfig `json:"email,omitempty"`
	// Secrets are new secret values the channel refers to. They are saved
	// with the channel once it is validated.
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

// PairCommand is the command a caller or a node runs to pair with code.
func PairCommand(agentURL, code string) string {
	return fmt.Sprintf("a2a serve pair join %s %s", agentURL, code)
}

// NodeInstructions is the message which tells the agent behind webhook node
// to pair with the agent at agentURL using code.
func NodeInstructions(agentURL, node, code string) string {
	agentURL = strings.TrimRight(agentURL, "/")
	return fmt.Sprintf("You are the webhook node %q of the agent at %s.\nRead %s/mailbox/skill.md and follow it.\nYour pairing code is %s.", node, agentURL, agentURL, code)
}

// ChannelRequestFromConfig is the request which starts the channel of kind
// as configured in conf.
func ChannelRequestFromConfig(conf *agentdir.Config, kind string) ChannelRequest {
	return ChannelRequest{Kind: kind, Token: conf.ChannelToken(kind), Email: conf.Channels.Email}
}

// ErrNotRunning is returned when no agent server answers on the control socket.
var ErrNotRunning error = &notRunningError{name: "the agent server"}

// NotRunning returns an error, matching [ErrNotRunning], for the agent name.
func NotRunning(name string) error { return &notRunningError{name: name} }

type notRunningError struct{ name string }

func (e *notRunningError) Error() string {
	return e.name + " is not running; start it with `a2a serve run --detach`"
}

func (e *notRunningError) Is(target error) bool { return target == ErrNotRunning }

// NotRunning marks the error for the CLI's exit code.
func (e *notRunningError) NotRunning() bool { return true }

// ErrUnsupported is returned when the running server is older than the
// client and doesn't know the call.
var ErrUnsupported = errors.New("the running agent server doesn't support this call; restart it with `a2a serve restart`")

// ReloadResponse lists the settings a server applied from config.yaml.
type ReloadResponse struct {
	Applied []string `json:"applied"`
}

// Client talks to a running agent server over its control socket.
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
	c := &Client{dir: dir, hc: &http.Client{
		Timeout: DefaultTimeout,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := fsutil.CheckPrivate(filepath.Dir(path)); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil, &net.OpError{Op: "dial", Net: "unix", Err: err}
				}
				return nil, fmt.Errorf("not trusting the control socket %s: %w", path, err)
			}
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}},
	}}
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
		// Errors after the dial come from a server that runs.
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return ErrNotRunning
		}
		return err
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
		return errors.New(jsonhttp.ErrorMessage(resp.Status, data))
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
// apply without a restart, other than channels.
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

// DeleteContact removes a contact.
func (c *Client) DeleteContact(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/contacts/"+url.PathEscape(name), nil, nil)
}

// AddChannel adds or replaces a channel.
func (c *Client) AddChannel(ctx context.Context, req ChannelRequest) (*ChannelStatus, error) {
	var out ChannelStatus
	return &out, c.do(ctx, http.MethodPost, "/channels", req, &out)
}

// DeleteChannel removes a channel.
func (c *Client) DeleteChannel(ctx context.Context, kind string) error {
	return c.do(ctx, http.MethodDelete, "/channels/"+url.PathEscape(kind), nil, nil)
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
	// Deliver is the chat the runs are shown in, <channel>:<who>.
	Deliver      string    `json:"deliver,omitempty"`
	Conversation string    `json:"conversation"`
	CreatedBy    string    `json:"createdBy"`
	NextRun      time.Time `json:"nextRun,omitzero"`
	LastRun      time.Time `json:"lastRun,omitzero"`
	LastError    string    `json:"lastError,omitempty"`
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
