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

// Package pairings manages who is paired with an agent: people, devices and
// other agents, who hold tokens and linked chat accounts under a pairing,
// and webhook nodes. The control socket of a running agent and the CLI of a
// stopped one share it.
package pairings

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
	"github.com/a2aproject/a2a-serve/internal/webhook"
)

// LocalOwner is the built-in pairing of the user running the agent: the
// token of this machine, and the chat accounts linked with the code printed
// at start.
const LocalOwner = auth.LocalPairing

// SchedulesAgent is the built-in agent that manages schedules, which no
// webhook node or contact can be called.
const SchedulesAgent = "schedules"

// Advertiser advertises webhook nodes to the agents of the private network.
type Advertiser interface {
	// Advertise adds or renews the advertisement of the node name.
	Advertise(name, description string) error
	// Withdraw stops advertising the node name.
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
	// Changed is called after a node is added, changed or removed. Optional.
	Changed func(ctx context.Context)
	// Access describes who may call the agent, which responses to changes
	// carry. Optional.
	Access func(ctx context.Context) (*controlapi.Access, error)
	// Limiter limits failed attempts to redeem codes. [Service.Redeem]
	// needs it.
	Limiter *Limiter
	// Users stops what the users of a pairing run while it is removed.
	// Optional: a stopped agent runs nothing.
	Users Users
	// Homes deletes the agent homes of the users of a removed pairing.
	// Optional.
	Homes Homes

	mu       sync.Mutex
	removing map[string]int
}

// Users stops the tasks and agent processes of users.
type Users interface {
	// Stop cancels the unfinished tasks of the users and stops their agent
	// processes. None starts again for them until release is called.
	Stop(ctx context.Context, principals ...string) (release func(), err error)
}

// Homes deletes the homes of users' agent processes. agentdir.Dir is one.
type Homes interface {
	// RemoveUserHome deletes the agent home of the user id, if it exists.
	RemoveUserHome(id string) error
	// UserHomeDir is the path of the agent home of the user id.
	UserHomeDir(id string) string
}

func invalid(format string, args ...any) error {
	return agenterr.Invalidf(format, args...)
}

func notFound(name string) error {
	return &agenterr.NotFoundError{Kind: "pairing", Name: name}
}

func validateName(name string) error {
	if name == "" || strings.ContainsAny(name, " /:") {
		return invalid("the name must be non-empty, without spaces, slashes or colons")
	}
	if name == LocalOwner || name == auth.OwnerName {
		return invalid("%q names the user running the agent; pick another name", name)
	}
	return nil
}

func (s *Service) pairing(ctx context.Context, name string) (store.Pairing, bool, error) {
	p, err := s.Store.Pairing(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.Pairing{}, false, nil
	}
	return p, err == nil, err
}

// Invite issues a pairing code for a new pairing, or for another device or
// account of an existing one, whose tokens keep working. With Webhook, it
// creates a webhook node.
func (s *Service) Invite(ctx context.Context, req controlapi.PairCodeRequest) (*controlapi.PairCodeResponse, error) {
	resp, err := s.invite(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp, s.withAccess(ctx, &resp.Access)
}

func (s *Service) invite(ctx context.Context, req controlapi.PairCodeRequest) (*controlapi.PairCodeResponse, error) {
	req.Name = strings.TrimSpace(req.Name)
	if err := validateName(req.Name); err != nil {
		return nil, err
	}
	p, exists, err := s.pairing(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	isWebhook := exists && p.Kind == pairapi.PairingWebhook
	nodeFlags := req.Webhook != "" || len(req.Headers) > 0 || req.Description != "" || req.Advertised != nil
	switch {
	case nodeFlags && isWebhook:
		return nil, invalid("%s already exists; change it with `a2a-serve pair set %s`", req.Name, req.Name)
	case nodeFlags && exists:
		return nil, agenterr.Conflictf("%s is already paired; pick another name, or run `a2a-serve pair rm %s` first", req.Name, req.Name)
	case nodeFlags && req.Webhook == "":
		return nil, invalid("--webhook-header, --description and --advertised go with --webhook")
	case (nodeFlags || isWebhook) && req.Owner:
		return nil, invalid("--owner doesn't apply to webhook nodes")
	case nodeFlags:
		return s.addNode(ctx, req)
	case isWebhook:
		node, err := s.Store.Node(ctx, req.Name)
		if err != nil {
			return nil, err
		}
		return s.nodeCode(ctx, node, false)
	case exists && req.Owner && p.Role != auth.RoleOwner:
		return nil, invalid("%s is paired as %s; make it an owner with `a2a-serve pair set %s --owner`", req.Name, p.Role, req.Name)
	}
	role := auth.RoleAuthenticated
	if req.Owner {
		role = auth.RoleOwner
	}
	return s.callerCode(ctx, req.Name, func(ctx context.Context, st *store.Store, name string) (string, time.Time, error) {
		return auth.MintInviteCode(ctx, st, store.Pairing{Name: name, Role: role})
	})
}

func (s *Service) callerCode(ctx context.Context, name string, mint func(context.Context, *store.Store, string) (string, time.Time, error)) (*controlapi.PairCodeResponse, error) {
	code, expires, err := mint(ctx, s.Store, name)
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
		return nil, invalid("%q is the built-in agent that manages schedules; pick another name", SchedulesAgent)
	}
	if err := webhook.Validate(req.Webhook); err != nil {
		return nil, invalid("%v", err)
	}
	if _, err := s.Store.Contact(ctx, req.Name); err == nil {
		return nil, agenterr.Conflictf("%q is already a contact", req.Name)
	}
	if req.Advertised != nil && s.Ads == nil {
		return nil, invalid("advertising needs the private network of a running agent")
	}
	_, err := s.Store.Node(ctx, req.Name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	existed := err == nil
	node := store.Node{
		ID: req.Name, Name: req.Name, Description: req.Description, Webhook: req.Webhook, Headers: req.Headers,
		Secret: cryptoutil.RandomString(40, cryptoutil.Alphanumeric),
	}
	err = s.Store.PutNode(ctx, node)
	if errors.Is(err, store.ErrExists) {
		return nil, agenterr.Conflictf("%s is already paired; pick another name", req.Name)
	}
	if err != nil {
		return nil, err
	}
	defer s.changed(ctx)
	resp, err := s.nodeCode(ctx, node, false)
	if err == nil && req.Advertised != nil {
		err = s.setAdvertised(node, *req.Advertised)
		resp.Advertised = *req.Advertised
	}
	if err == nil {
		return resp, nil
	}
	if existed {
		return nil, err
	}
	if derr := s.Store.DeletePairing(ctx, node.ID, auth.PairPrincipal(node.ID), auth.NodePrincipal(node.ID)); derr != nil {
		err = errors.Join(err, fmt.Errorf("removing the node that wasn't set up: %w", derr))
	}
	return nil, err
}

func (s *Service) nodeCode(ctx context.Context, node store.Node, sole bool) (*controlapi.PairCodeResponse, error) {
	mint := auth.MintCode
	if sole {
		mint = auth.MintSoleCode
	}
	code, expires, err := mint(ctx, s.Store, node.ID)
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
		ID: node.ID, Name: node.Name, URL: mailboxapi.NodeURL(base, node.ID),
		Webhook: webhook.RedactURL(node.Webhook), Paired: node.TokenHash != "", Created: node.Created,
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
	resp, err := s.update(ctx, name, req)
	if err != nil {
		return nil, err
	}
	return resp, s.withAccess(ctx, &resp.Access)
}

func (s *Service) update(ctx context.Context, name string, req controlapi.PairingUpdate) (*controlapi.PairingUpdateResponse, error) {
	p, exists, err := s.pairing(ctx, name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, notFound(name)
	}
	if p.Kind == pairapi.PairingWebhook {
		node, err := s.Store.Node(ctx, name)
		if err != nil {
			return nil, err
		}
		return s.updateNode(ctx, node, req)
	}
	if name == LocalOwner {
		return nil, invalid("%s is you, on this machine and in your linked chats; `a2a-serve pair rm %s` revokes its token and chat links, and the next start issues a new token", name, name)
	}
	if req.Webhook != "" || len(req.Headers) > 0 || req.Description != nil || req.Advertised != nil {
		return nil, invalid("--webhook, --webhook-header, --description and --advertised apply to webhook nodes")
	}
	next := p
	if req.Kind != "" {
		if req.Kind == pairapi.PairingWebhook || !slices.Contains(pairapi.PairingKinds, req.Kind) {
			return nil, invalid("--kind must be %s, %s or %s; webhook nodes come from `a2a-serve pair invite --webhook`", pairapi.PairingPerson, pairapi.PairingDevice, pairapi.PairingAgent)
		}
		next.Kind = req.Kind
	}
	if req.Owner != nil {
		next.Role = auth.RoleAuthenticated
		if *req.Owner {
			next.Role = auth.RoleOwner
		}
	}
	if next != p {
		if err := s.Store.UpdatePairing(ctx, next); err != nil {
			return nil, err
		}
	}
	var resp controlapi.PairingUpdateResponse
	if req.Rotate {
		if err := s.Store.DeleteTokens(ctx, name); err != nil {
			return nil, err
		}
		if resp.Code, err = s.callerCode(ctx, name, auth.MintSoleCode); err != nil {
			return nil, err
		}
	}
	got, err := s.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	resp.Pairing = *got
	return &resp, nil
}

func (s *Service) updateNode(ctx context.Context, node store.Node, req controlapi.PairingUpdate) (*controlapi.PairingUpdateResponse, error) {
	prev := node
	if req.Owner != nil || req.Kind != "" {
		return nil, invalid("--owner and --kind don't apply to webhook nodes")
	}
	if req.Webhook != "" {
		if err := webhook.Validate(req.Webhook); err != nil {
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
	advertised := s.Ads != nil && s.Ads.Advertised(node.ID)
	wasAdvertised := advertised
	if req.Advertised != nil {
		advertised = *req.Advertised
	}
	setAds := req.Advertised != nil || (advertised && req.Description != nil)
	// The node is stored last, so that a failure leaves its token valid.
	if setAds {
		if err := s.setAdvertised(node, advertised); err != nil {
			return nil, err
		}
	}
	undo := func(err error) error {
		if !setAds {
			return err
		}
		if rerr := s.setAdvertised(prev, wasAdvertised); rerr != nil {
			return errors.Join(err, fmt.Errorf("restoring the advertisement of %s: %w", node.ID, rerr))
		}
		return err
	}
	var resp controlapi.PairingUpdateResponse
	if req.Rotate {
		code, err := s.nodeCode(ctx, node, true)
		if err != nil {
			return nil, undo(err)
		}
		code.Advertised = advertised
		resp.Code = code
	}
	err := s.Store.UpdateNode(ctx, prev, node)
	if errors.Is(err, store.ErrChanged) {
		return nil, undo(agenterr.Conflictf("%s changed while it was updated; try again", node.ID))
	}
	if err != nil {
		return nil, undo(err)
	}
	s.changed(ctx)
	p, err := s.Get(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	resp.Pairing = *p
	return &resp, nil
}

// Remove deletes a pairing with its tokens, chat links and pending codes,
// a webhook node with it, and the schedules, watches, tasks and questions
// of the users it acts as. It first cancels their tasks, stops their agent
// processes and deletes their agent homes, so a new pairing with the same
// name starts with none of them. The local pairing stays; only its
// credentials are revoked.
func (s *Service) Remove(ctx context.Context, name string) (*controlapi.RevokeResponse, error) {
	p, exists, err := s.pairing(ctx, name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, notFound(name)
	}
	resp := &controlapi.RevokeResponse{Kind: p.Kind}
	if name == LocalOwner {
		resp.Kept = true
		return resp, s.Store.RevokeCredentials(ctx, name)
	}
	endRemoval := s.beginRemoval(name)
	err = s.deletePairing(ctx, name, auth.PairPrincipal(name), auth.NodePrincipal(name))
	endRemoval()
	if err != nil {
		return nil, err
	}
	if p.Kind == pairapi.PairingWebhook {
		defer s.changed(ctx)
		if s.Ads != nil {
			if err := s.Ads.Withdraw(name); err != nil {
				return nil, fmt.Errorf("withdrawing the advertisement of %s: %w", name, err)
			}
		}
		return resp, s.withAccess(ctx, &resp.Access)
	}
	if err := s.withAccess(ctx, &resp.Access); err != nil {
		return nil, err
	}
	if resp.Access != nil {
		resp.StillAllowed = resp.Access.Others
	}
	return resp, nil
}

// PreviewRemove describes what [Service.Remove] would delete for the
// pairing called name, and changes nothing.
func (s *Service) PreviewRemove(ctx context.Context, name string) (*controlapi.RemovalPreview, error) {
	p, exists, err := s.pairing(ctx, name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, notFound(name)
	}
	resp := &controlapi.RemovalPreview{Kind: p.Kind, Accounts: []controlapi.LinkedAccount{}, AgentHomes: []string{}}
	links, err := s.Store.LinksOf(ctx, name)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		resp.Accounts = append(resp.Accounts, controlapi.LinkedAccount{Channel: l.Channel, Account: l.Identity})
	}
	if name == LocalOwner {
		resp.Kept = true
		counts, err := s.Store.PairingRemoval(ctx, name)
		if err != nil {
			return nil, err
		}
		resp.Tokens, resp.Codes = counts.Tokens, counts.Codes
		return resp, nil
	}
	principals := []string{auth.PairPrincipal(name), auth.NodePrincipal(name)}
	counts, err := s.Store.PairingRemoval(ctx, name, principals...)
	if err != nil {
		return nil, err
	}
	resp.Tokens, resp.Codes, resp.Node = counts.Tokens, counts.Codes, p.Kind == pairapi.PairingWebhook
	resp.Schedules, resp.Watches, resp.Approvals = counts.Schedules, counts.Watches, counts.Questions
	resp.Tasks, resp.RunningTasks, resp.Conversations = counts.Tasks, counts.Unfinished, counts.Conversations
	if s.Homes == nil {
		return resp, nil
	}
	for _, id := range principals {
		dir := s.Homes.UserHomeDir(id)
		_, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("checking the agent home of %s: %w", name, err)
		}
		resp.AgentHomes = append(resp.AgentHomes, dir)
	}
	return resp, nil
}

// beginRemoval makes [Service.Redeem] refuse the codes of the pairing name
// until the returned function is called, so no credential is issued while
// the pairing's users are stopped and its homes deleted.
func (s *Service) beginRemoval(name string) (end func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removing == nil {
		s.removing = map[string]int{}
	}
	s.removing[name]++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.removing[name]--; s.removing[name] == 0 {
			delete(s.removing, name)
		}
	}
}

func (s *Service) isRemoving(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removing[name] > 0
}

// deletePairing deletes the pairing called name, and the agent homes of
// principals, while the principals run nothing.
func (s *Service) deletePairing(ctx context.Context, name string, principals ...string) error {
	if s.Users != nil {
		release, err := s.Users.Stop(ctx, principals...)
		if err != nil {
			return fmt.Errorf("stopping what %s runs: %w", name, err)
		}
		defer release()
	}
	if s.Homes != nil {
		for _, p := range principals {
			if err := s.Homes.RemoveUserHome(p); err != nil {
				return fmt.Errorf("deleting the agent home of %s: %w", name, err)
			}
		}
	}
	return s.Store.DeletePairing(ctx, name, principals...)
}

// SignOut revokes the Google sign-in tokens of email at every URL. The
// person can sign in again while --allow-email matches the email.
func (s *Service) SignOut(ctx context.Context, email string) (*controlapi.SignOutResponse, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return nil, invalid("%q is not an email address", email)
	}
	n, err := s.Store.DeleteSignIns(ctx, email)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, &agenterr.NotFoundError{Kind: "Google sign-in of", Name: email}
	}
	resp := &controlapi.SignOutResponse{Email: email, Tokens: int(n)}
	return resp, s.withAccess(ctx, &resp.Access)
}

// withAccess sets dst to who may call the agent, if the service knows.
func (s *Service) withAccess(ctx context.Context, dst **controlapi.Access) error {
	if s.Access == nil {
		return nil
	}
	a, err := s.Access(ctx)
	if err != nil {
		return err
	}
	*dst = a
	return nil
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

// List describes every pairing, by name.
func (s *Service) List(ctx context.Context) ([]controlapi.Pairing, error) {
	all, err := s.Store.Pairings(ctx)
	if err != nil {
		return nil, err
	}
	toks, err := s.Store.Tokens(ctx)
	if err != nil {
		return nil, err
	}
	links, err := s.Store.Links(ctx)
	if err != nil {
		return nil, err
	}
	codes, err := s.Store.PendingCodes(ctx)
	if err != nil {
		return nil, err
	}
	nodeList, err := s.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	nodesByID := map[string]store.Node{}
	for _, n := range nodeList {
		nodesByID[n.ID] = n
	}
	base := s.BaseURL()
	out := make([]controlapi.Pairing, 0, len(all))
	for _, p := range all {
		cp := controlapi.Pairing{
			Name: p.Name, Kind: p.Kind, Role: p.Role, Principal: auth.PairingPrincipal(p.Name), Created: p.Created,
			Tokens: []controlapi.PairingToken{}, Accounts: []controlapi.LinkedAccount{}, PendingCodes: []time.Time{},
		}
		for _, t := range toks {
			if t.Pairing != p.Name {
				continue
			}
			cp.Tokens = append(cp.Tokens, controlapi.PairingToken{URL: t.URL, Created: t.Created, LastUsed: t.LastUsed})
			if t.LastUsed.After(cp.LastUsed) {
				cp.LastUsed = t.LastUsed
			}
		}
		for _, l := range links[p.Name] {
			cp.Accounts = append(cp.Accounts, controlapi.LinkedAccount{Channel: l.Channel, Account: l.Identity})
		}
		for _, c := range codes[p.Name] {
			cp.PendingCodes = append(cp.PendingCodes, c.Expires)
		}
		if n, ok := nodesByID[p.Name]; ok {
			cp.Principal = auth.NodePrincipal(n.ID)
			cp.Node = &controlapi.NodeDetails{
				URL: mailboxapi.NodeURL(base, n.ID), Webhook: webhook.RedactURL(n.Webhook), Description: n.Description,
				HeaderNames: slices.Sorted(maps.Keys(n.Headers)), Paired: n.TokenHash != "",
				Advertised: s.Ads != nil && s.Ads.Advertised(n.ID),
			}
			if s.Mailbox != nil {
				cp.Node.Waiting, cp.Node.Open = s.Mailbox(n.ID)
			}
		}
		out = append(out, cp)
	}
	return out, nil
}
