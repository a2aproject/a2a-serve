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

// Package pairings manages who is paired with an agent: callers, who hold
// tokens and linked messenger accounts under a pairing name, and webhook
// nodes. The control socket of a running agent and the CLI of a stopped one
// share it.
package pairings

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/nodes"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// LocalOwner names the owner token the server saves for this machine.
const LocalOwner = "local"

// SchedulesAgent is the agent the harness manages schedules with, which no
// webhook node or contact can be called.
const SchedulesAgent = "schedules"

// Advertiser advertises webhook nodes to the agents of the private network.
type Advertiser interface {
	Advertise(name, description string) error
	Withdraw(name string) error
	// Advertised reports whether the node is advertised.
	Advertised(name string) bool
}

// Service manages the pairings of an agent.
type Service struct {
	Store *store.Store
	// BaseURL returns the URL codes are redeemed at.
	BaseURL func() string
	// Ads advertises webhook nodes. Optional.
	Ads Advertiser
	// LinkHints tells how to link a messenger account with a code. Optional.
	LinkHints func(code string) []string
	// Mailbox counts the waiting wakes and open tasks of a node. Optional.
	Mailbox func(id string) (waiting, open int)
	// Changed is called after a node is added or removed. Optional.
	Changed func(ctx context.Context)
}

// Kind classifies an [Error].
type Kind int

const (
	// Invalid requests are usage errors.
	Invalid Kind = iota + 1
	// Conflict requests name a pairing of another kind.
	Conflict
	// NotFound requests name no pairing.
	NotFound
)

// Error is a request the service refuses.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// UsageError marks invalid requests for the CLI's exit code.
func (e *Error) UsageError() bool { return e.Kind != NotFound }

func invalid(format string, args ...any) error {
	return &Error{Kind: Invalid, Msg: fmt.Sprintf(format, args...)}
}

func notFound(name string) error {
	return &Error{Kind: NotFound, Msg: "no pairing named " + name}
}

func validateName(name string) error {
	if name == "" || strings.ContainsAny(name, " /:") {
		return invalid("the name must be non-empty, without spaces, slashes or colons")
	}
	if name == LocalOwner {
		return invalid("%q is reserved for the owner token of this machine; pick another name", LocalOwner)
	}
	return nil
}

func (s *Service) node(ctx context.Context, name string) (store.Node, bool, error) {
	n, err := s.Store.Node(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.Node{}, false, nil
	}
	return n, err == nil, err
}

// Invite issues a pairing code for a new pairing, or for another device or
// account of an existing one, whose tokens keep working. With Webhook, it
// creates a webhook node.
func (s *Service) Invite(ctx context.Context, req controlapi.PairCodeRequest) (*controlapi.PairCodeResponse, error) {
	req.Name = strings.TrimSpace(req.Name)
	if err := validateName(req.Name); err != nil {
		return nil, err
	}
	node, isNode, err := s.node(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	caller, err := s.caller(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	nodeFlags := req.Webhook != "" || len(req.Headers) > 0 || req.Description != "" || req.Advertised != nil
	switch {
	case nodeFlags && isNode:
		return nil, invalid("%s already exists; change it with `a2a serve pair set %s`", req.Name, req.Name)
	case nodeFlags && caller != nil:
		return nil, &Error{Kind: Conflict, Msg: req.Name + " is already paired as a caller; pick another name, or run `a2a serve pair rm " + req.Name + "` first"}
	case nodeFlags && req.Webhook == "":
		return nil, invalid("--webhook-header, --description and --advertised go with --webhook")
	case nodeFlags && req.Role != "":
		return nil, invalid("--role doesn't apply to webhook nodes")
	case nodeFlags:
		return s.addNode(ctx, req)
	case isNode && req.Role != "":
		return nil, invalid("--role doesn't apply to webhook nodes")
	case isNode:
		return s.nodeCode(ctx, node, false)
	}
	role := req.Role
	switch {
	case caller != nil && role != "" && role != caller.Role:
		return nil, invalid("%s is %s %s; change the role with `a2a serve pair set %s --role %s`", req.Name, article(caller.Role), caller.Role, req.Name, role)
	case caller != nil:
		role = caller.Role
	case role == "":
		role = auth.RoleMember
	}
	if role != auth.RoleMember && role != auth.RoleOwner {
		return nil, invalid("--role must be member or owner")
	}
	return s.callerCode(ctx, req.Name, role, false)
}

func article(role string) string {
	if role == auth.RoleOwner {
		return "an"
	}
	return "a"
}

func (s *Service) callerCode(ctx context.Context, name, role string, sole bool) (*controlapi.PairCodeResponse, error) {
	mint := auth.MintCode
	if sole {
		mint = auth.MintSoleCode
	}
	code, expires, err := mint(ctx, s.Store, auth.PurposePair, name, role)
	if err != nil {
		return nil, err
	}
	base := s.BaseURL()
	resp := &controlapi.PairCodeResponse{Code: code, Expires: expires, URL: base, PairCommand: controlapi.PairCommand(base, code)}
	if s.LinkHints != nil {
		resp.Hints = s.LinkHints(code)
	}
	return resp, nil
}

func (s *Service) addNode(ctx context.Context, req controlapi.PairCodeRequest) (*controlapi.PairCodeResponse, error) {
	if req.Name == SchedulesAgent {
		return nil, invalid("%q is the agent the harness manages schedules with; pick another name", SchedulesAgent)
	}
	if err := nodes.ValidateWebhook(req.Webhook); err != nil {
		return nil, invalid("%v", err)
	}
	if _, err := s.Store.Contact(ctx, req.Name); err == nil {
		return nil, &Error{Kind: Conflict, Msg: fmt.Sprintf("%q is already a contact", req.Name)}
	}
	node := store.Node{
		ID: req.Name, Name: req.Name, Description: req.Description, Webhook: req.Webhook, Headers: req.Headers,
		Secret: cryptoutil.RandomString(40, cryptoutil.Alphanumeric),
	}
	if err := s.Store.PutNode(ctx, node); err != nil {
		return nil, err
	}
	resp, err := s.nodeCode(ctx, node, false)
	if err != nil {
		return nil, err
	}
	if req.Advertised != nil {
		if err := s.setAdvertised(node, *req.Advertised); err != nil {
			return nil, err
		}
		resp.Advertised = *req.Advertised
	}
	s.changed(ctx)
	return resp, nil
}

func (s *Service) nodeCode(ctx context.Context, node store.Node, sole bool) (*controlapi.PairCodeResponse, error) {
	mint := auth.MintCode
	if sole {
		mint = auth.MintSoleCode
	}
	code, expires, err := mint(ctx, s.Store, auth.PurposeNode, node.ID, "")
	if err != nil {
		return nil, err
	}
	base := s.BaseURL()
	info := NodeInfo(base, node)
	return &controlapi.PairCodeResponse{
		Code: code, Expires: expires, URL: base, PairCommand: controlapi.PairCommand(base, code), Node: &info,
		Instructions: controlapi.NodeInstructions(base, node.Name, code),
	}, nil
}

// NodeInfo describes a node hosted at the agent URL base.
func NodeInfo(base string, node store.Node) controlapi.NodeInfo {
	return controlapi.NodeInfo{
		ID: node.ID, Name: node.Name, URL: strings.TrimSuffix(base, "/") + "/n/" + node.ID,
		Webhook: nodes.RedactURL(node.Webhook), Paired: node.TokenHash != "", Created: node.Created,
	}
}

func (s *Service) setAdvertised(node store.Node, on bool) error {
	if s.Ads == nil {
		return invalid("advertising needs the private network of a running agent")
	}
	if on {
		return s.Ads.Advertise(node.ID, node.Description)
	}
	return s.Ads.Withdraw(node.ID)
}

func (s *Service) changed(ctx context.Context) {
	if s.Changed != nil {
		s.Changed(ctx)
	}
}

// Update changes the pairing called name in place. Rotate revokes its tokens
// and issues a new code, which the response carries.
func (s *Service) Update(ctx context.Context, name string, req controlapi.PairingUpdate) (*controlapi.PairingUpdateResponse, error) {
	node, isNode, err := s.node(ctx, name)
	if err != nil {
		return nil, err
	}
	if isNode {
		return s.updateNode(ctx, node, req)
	}
	caller, err := s.caller(ctx, name)
	if err != nil {
		return nil, err
	}
	if caller == nil {
		return nil, notFound(name)
	}
	if req.Webhook != "" || len(req.Headers) > 0 || req.Description != nil || req.Advertised != nil {
		return nil, invalid("--webhook, --webhook-header, --description and --advertised apply to webhook nodes")
	}
	if req.Role != "" && req.Role != auth.RoleMember && req.Role != auth.RoleOwner {
		return nil, invalid("--role must be member or owner")
	}
	role := caller.Role
	if req.Role != "" && req.Role != role {
		next := auth.PrincipalForCode(name, req.Role)
		if err := s.Store.SetPairingRole(ctx, name, req.Role, next, caller.Principal); err != nil {
			return nil, err
		}
		role = req.Role
	}
	var resp controlapi.PairingUpdateResponse
	if req.Rotate {
		if err := s.Store.DeleteTokens(ctx, name); err != nil {
			return nil, err
		}
		if resp.Code, err = s.callerCode(ctx, name, role, true); err != nil {
			return nil, err
		}
	}
	p, err := s.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	resp.Pairing = *p
	return &resp, nil
}

func (s *Service) updateNode(ctx context.Context, node store.Node, req controlapi.PairingUpdate) (*controlapi.PairingUpdateResponse, error) {
	if req.Role != "" {
		return nil, invalid("--role doesn't apply to webhook nodes")
	}
	if req.Webhook != "" {
		if err := nodes.ValidateWebhook(req.Webhook); err != nil {
			return nil, invalid("%v", err)
		}
		node.Webhook = req.Webhook
	}
	if len(req.Headers) > 0 {
		node.Headers = req.Headers
	}
	if req.Description != nil {
		node.Description = *req.Description
	}
	if req.Rotate {
		node.Secret = cryptoutil.RandomString(40, cryptoutil.Alphanumeric)
		node.TokenHash = ""
	}
	if err := s.Store.PutNode(ctx, node); err != nil {
		return nil, err
	}
	advertised := s.Ads != nil && s.Ads.Advertised(node.ID)
	if req.Advertised != nil {
		advertised = *req.Advertised
	}
	if req.Advertised != nil || (advertised && req.Description != nil) {
		if err := s.setAdvertised(node, advertised); err != nil {
			return nil, err
		}
	}
	var resp controlapi.PairingUpdateResponse
	if req.Rotate {
		code, err := s.nodeCode(ctx, node, true)
		if err != nil {
			return nil, err
		}
		code.Advertised = advertised
		resp.Code = code
	}
	p, err := s.Get(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	resp.Pairing = *p
	return &resp, nil
}

// Remove revokes the tokens, messenger links and pending codes of a caller,
// or removes a webhook node. It returns the kind of pairing removed.
func (s *Service) Remove(ctx context.Context, name string) (string, error) {
	err := s.Store.DeleteNode(ctx, name)
	if err == nil {
		if s.Ads != nil {
			if err := s.Ads.Withdraw(name); err != nil {
				return "", fmt.Errorf("withdrawing the advertisement of %s: %w", name, err)
			}
		}
		s.changed(ctx)
		return pairapi.KindNode, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	n, err := s.Store.RevokeTokens(ctx, name, auth.PrincipalForCode(name, auth.RoleMember))
	if err != nil {
		return "", err
	}
	codes, err := s.Store.DeleteCodes(ctx, auth.PurposePair, name)
	if err != nil {
		return "", err
	}
	if n+codes == 0 {
		return "", notFound(name)
	}
	return pairapi.KindCaller, nil
}

// Get describes the pairing called name.
func (s *Service) Get(ctx context.Context, name string) (*controlapi.Pairing, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(all, func(p controlapi.Pairing) bool { return p.Name == name })
	if i < 0 {
		return nil, notFound(name)
	}
	return &all[i], nil
}

// caller returns the caller pairing called name, or nil.
func (s *Service) caller(ctx context.Context, name string) (*controlapi.Pairing, error) {
	callers, err := s.callers(ctx)
	if err != nil {
		return nil, err
	}
	return callers[name], nil
}

// List describes every pairing, callers by name, then nodes.
func (s *Service) List(ctx context.Context) ([]controlapi.Pairing, error) {
	callers, err := s.callers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]controlapi.Pairing, 0, len(callers))
	for _, name := range slices.Sorted(maps.Keys(callers)) {
		out = append(out, *callers[name])
	}
	all, err := s.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	codes, err := s.Store.PendingCodes(ctx)
	if err != nil {
		return nil, err
	}
	base := s.BaseURL()
	for _, n := range all {
		p := controlapi.Pairing{
			Name: n.ID, Kind: pairapi.KindNode, Principal: auth.NodePrincipal(n.ID), Created: n.Created,
			Tokens: []controlapi.PairingToken{}, Accounts: []controlapi.LinkedAccount{}, PendingCodes: []time.Time{},
			Node: &controlapi.NodeDetails{
				URL: NodeInfo(base, n).URL, Webhook: nodes.RedactURL(n.Webhook), Description: n.Description,
				HeaderNames: slices.Sorted(maps.Keys(n.Headers)), Paired: n.TokenHash != "",
				Advertised: s.Ads != nil && s.Ads.Advertised(n.ID),
			},
		}
		for _, c := range codes[n.ID] {
			if c.Purpose == auth.PurposeNode {
				p.PendingCodes = append(p.PendingCodes, c.Expires)
			}
		}
		if s.Mailbox != nil {
			p.Node.Waiting, p.Node.Open = s.Mailbox(n.ID)
		}
		out = append(out, p)
	}
	return out, nil
}

// callers collects the caller pairings from their tokens, links and pending
// codes, by name.
func (s *Service) callers(ctx context.Context) (map[string]*controlapi.Pairing, error) {
	out := map[string]*controlapi.Pairing{}
	get := func(name, role string) *controlapi.Pairing {
		p := out[name]
		if p == nil {
			p = &controlapi.Pairing{
				Name: name, Kind: pairapi.KindCaller, Role: role, Principal: auth.PrincipalForCode(name, role),
				Tokens: []controlapi.PairingToken{}, Accounts: []controlapi.LinkedAccount{}, PendingCodes: []time.Time{},
			}
			out[name] = p
		}
		return p
	}
	toks, err := s.Store.Tokens(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range toks {
		p := get(t.Name, roleOf(t.Roles))
		p.Tokens = append(p.Tokens, controlapi.PairingToken{URL: t.URL, Created: t.Created, LastUsed: t.LastUsed})
		if p.Created.IsZero() || t.Created.Before(p.Created) {
			p.Created = t.Created
		}
		if t.LastUsed.After(p.LastUsed) {
			p.LastUsed = t.LastUsed
		}
	}
	links, err := s.Store.NamedLinks(ctx)
	if err != nil {
		return nil, err
	}
	for name, ls := range links {
		role := auth.RoleMember
		if ls[0].UserID == auth.OwnerID {
			role = auth.RoleOwner
		}
		p := get(name, role)
		for _, l := range ls {
			p.Accounts = append(p.Accounts, controlapi.LinkedAccount{Channel: l.Channel, Account: l.Identity})
		}
	}
	codes, err := s.Store.PendingCodes(ctx)
	if err != nil {
		return nil, err
	}
	for name, cs := range codes {
		for _, c := range cs {
			if c.Purpose != auth.PurposePair {
				continue
			}
			p := get(name, c.Role)
			p.PendingCodes = append(p.PendingCodes, c.Expires)
		}
	}
	return out, nil
}

func roleOf(roles []string) string {
	if slices.Contains(roles, auth.RoleOwner) {
		return auth.RoleOwner
	}
	return auth.RoleMember
}
