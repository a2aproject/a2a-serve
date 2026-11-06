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
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
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

// Deps are the dependencies of [Service].
type Deps struct {
	Store *store.Store
	// BaseURL returns the URL codes are redeemed at.
	BaseURL func() string
	// Ads advertises webhook nodes. Optional.
	Ads Advertiser
	// LinkHints tells how to link a messenger account with a code. Optional.
	LinkHints func(code string) []string
	// Mailbox counts the waiting wakes and open tasks of a node. Optional.
	Mailbox func(id string) (waiting, open int)
	// Forget drops what the server keeps in memory for a removed node.
	// Optional.
	Forget func(id string)
	// Changed is called after a node is added, changed or removed. Optional.
	Changed func(ctx context.Context)
	// Limiter limits failed attempts to redeem codes. Nil limits them on
	// the system clock.
	Limiter *Limiter
	// Users stops what the users of a pairing run while it is removed.
	// Optional: a stopped agent runs nothing.
	Users Users
	// Homes deletes the agent homes of the users of a removed pairing.
	// Optional.
	Homes Homes
}

// Service manages the pairings of an agent.
type Service struct {
	deps Deps

	mu       sync.Mutex
	removing map[string]int
}

// New returns the pairings service over deps.
func New(deps Deps) *Service {
	if deps.Limiter == nil {
		deps.Limiter = NewLimiter(time.Now)
	}
	return &Service{deps: deps}
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

// maxNameLen bounds the names of new pairings, which appear in URLs,
// principals and chat messages.
const maxNameLen = 64

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validateName(name string) error {
	if len(name) > maxNameLen || !namePattern.MatchString(name) {
		return invalid("the name must have up to %d letters, digits, dots, hyphens or underscores, and start with a letter or digit", maxNameLen)
	}
	if strings.EqualFold(name, LocalOwner) || strings.EqualFold(name, auth.OwnerName) {
		return invalid("%q names the user running the agent; pick another name", name)
	}
	return nil
}

// checkCaseClash refuses a new pairing whose name differs from an existing
// one only by case, which people would take for the same pairing.
func (s *Service) checkCaseClash(ctx context.Context, name string) error {
	all, err := s.deps.Store.Pairings(ctx)
	if err != nil {
		return err
	}
	for _, p := range all {
		if strings.EqualFold(p.Name, name) {
			return agenterr.Conflictf("%s differs from the pairing %s only by case; use %s or pick another name", name, p.Name, p.Name)
		}
	}
	return nil
}

func (s *Service) pairing(ctx context.Context, name string) (store.Pairing, bool, error) {
	p, err := s.deps.Store.Pairing(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.Pairing{}, false, nil
	}
	return p, err == nil, err
}

// Invite issues a pairing code for a new pairing, or for another device or
// account of an existing one, whose tokens keep working. With Webhook, it
// creates a webhook node.
func (s *Service) Invite(ctx context.Context, req InviteRequest) (*Code, error) {
	req.Name = strings.TrimSpace(req.Name)
	if err := validateName(req.Name); err != nil {
		return nil, err
	}
	p, exists, err := s.pairing(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := s.checkCaseClash(ctx, req.Name); err != nil {
			return nil, err
		}
	}
	isWebhook := exists && p.Kind == pairapi.PairingWebhook
	nodeFlags := req.Webhook != "" || len(req.Headers) > 0 || req.Description != "" || req.Advertised != nil
	switch {
	case nodeFlags && isWebhook:
		return nil, agenterr.WithHint(invalid("%s already exists", req.Name), agenterr.HintPairSet, req.Name)
	case nodeFlags && exists:
		return nil, agenterr.WithHint(agenterr.Conflictf("%s is already paired", req.Name), agenterr.HintPairRm, req.Name)
	case nodeFlags && req.Webhook == "":
		return nil, invalid("--webhook-header, --description and --advertised go with --webhook")
	case (nodeFlags || isWebhook) && req.Owner:
		return nil, invalid("--owner doesn't apply to webhook nodes")
	case nodeFlags:
		return s.addNode(ctx, req)
	case isWebhook:
		node, err := s.deps.Store.Node(ctx, req.Name)
		if err != nil {
			return nil, err
		}
		return s.nodeCode(ctx, node, false)
	case exists && req.Owner && p.Role != auth.RoleOwner:
		return nil, agenterr.WithHint(invalid("%s is paired as %s", req.Name, p.Role), agenterr.HintPairSetOwner, req.Name)
	}
	role := auth.RoleAuthenticated
	if req.Owner {
		role = auth.RoleOwner
	}
	return s.callerCode(ctx, req.Name, func(ctx context.Context, st *store.Store, name string) (string, time.Time, error) {
		return auth.MintInviteCode(ctx, st, store.Pairing{Name: name, Role: role})
	})
}

func (s *Service) callerCode(ctx context.Context, name string, mint func(context.Context, *store.Store, string) (string, time.Time, error)) (*Code, error) {
	code, expires, err := mint(ctx, s.deps.Store, name)
	if err != nil {
		return nil, err
	}
	base := s.deps.BaseURL()
	resp := &Code{Code: code, Expires: expires, URL: base}
	if s.deps.LinkHints != nil {
		resp.Hints = s.deps.LinkHints(code)
	}
	return resp, nil
}

func (s *Service) addNode(ctx context.Context, req InviteRequest) (*Code, error) {
	if req.Name == SchedulesAgent {
		return nil, invalid("%q is the built-in agent that manages schedules; pick another name", SchedulesAgent)
	}
	if err := mailboxapi.ValidateWebhook(req.Webhook); err != nil {
		return nil, invalid("%v", err)
	}
	if _, err := s.deps.Store.Contact(ctx, req.Name); err == nil {
		return nil, agenterr.Conflictf("%q is already a contact", req.Name)
	}
	if req.Advertised != nil && s.deps.Ads == nil {
		return nil, invalid("advertising needs the private network of a running agent")
	}
	_, err := s.deps.Store.Node(ctx, req.Name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	existed := err == nil
	node := store.Node{
		ID: req.Name, Name: req.Name, Description: req.Description, Webhook: req.Webhook, Headers: req.Headers,
		Secret: cryptoutil.RandomString(40, cryptoutil.Alphanumeric),
	}
	err = s.deps.Store.PutNode(ctx, node)
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
	if derr := s.deps.Store.DeletePairing(ctx, node.ID, auth.PairPrincipal(node.ID), auth.NodePrincipal(node.ID)); derr != nil {
		err = errors.Join(err, fmt.Errorf("removing the node that wasn't set up: %w", derr))
	}
	return nil, err
}

func (s *Service) nodeCode(ctx context.Context, node store.Node, sole bool) (*Code, error) {
	mint := auth.MintCode
	if sole {
		mint = auth.MintSoleCode
	}
	code, expires, err := mint(ctx, s.deps.Store, node.ID)
	if err != nil {
		return nil, err
	}
	base := s.deps.BaseURL()
	info := NodeInfo(base, node)
	return &Code{Code: code, Expires: expires, URL: base, Node: &info}, nil
}

// NodeInfo describes a node hosted at the agent URL base.
func NodeInfo(base string, node store.Node) Node {
	return Node{
		ID: node.ID, Name: node.Name, URL: mailboxapi.NodeURL(base, node.ID),
		Webhook: mailboxapi.RedactWebhookURL(node.Webhook), Paired: node.TokenHash != "", Created: node.Created,
	}
}

func (s *Service) setAdvertised(node store.Node, on bool) error {
	if s.deps.Ads == nil {
		return invalid("advertising needs the private network of a running agent")
	}
	if on {
		return s.deps.Ads.Advertise(node.ID, node.Description)
	}
	return s.deps.Ads.Withdraw(node.ID)
}

func (s *Service) changed(ctx context.Context) {
	if s.deps.Changed != nil {
		s.deps.Changed(ctx)
	}
}

// Update changes the pairing called name in place. Rotate revokes its tokens
// and issues a new code, which the result carries.
func (s *Service) Update(ctx context.Context, name string, req UpdateRequest) (*UpdateResult, error) {
	p, exists, err := s.pairing(ctx, name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, notFound(name)
	}
	if p.Kind == pairapi.PairingWebhook {
		node, err := s.deps.Store.Node(ctx, name)
		if err != nil {
			return nil, err
		}
		return s.updateNode(ctx, node, req)
	}
	if name == LocalOwner {
		return nil, agenterr.WithHint(invalid("%s is you, on this machine and in your linked chats", name), agenterr.HintPairRmLocal, name)
	}
	if req.Webhook != "" || len(req.Headers) > 0 || req.Description != nil || req.Advertised != nil {
		return nil, invalid("--webhook, --webhook-header, --description and --advertised apply to webhook nodes")
	}
	next := p
	if req.Kind != "" {
		if req.Kind == pairapi.PairingWebhook || !slices.Contains(pairapi.PairingKinds, req.Kind) {
			return nil, agenterr.WithHint(invalid("--kind must be %s, %s or %s", pairapi.PairingPerson, pairapi.PairingDevice, pairapi.PairingAgent), agenterr.HintPairInviteWebhook, "")
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
		if err := s.deps.Store.UpdatePairing(ctx, next); err != nil {
			return nil, err
		}
	}
	var resp UpdateResult
	if req.Rotate {
		if err := s.deps.Store.DeleteTokens(ctx, name); err != nil {
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

func (s *Service) updateNode(ctx context.Context, node store.Node, req UpdateRequest) (*UpdateResult, error) {
	prev := node
	if req.Owner != nil || req.Kind != "" {
		return nil, invalid("--owner and --kind don't apply to webhook nodes")
	}
	if req.Webhook != "" {
		if err := mailboxapi.ValidateWebhook(req.Webhook); err != nil {
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
	advertised := s.deps.Ads != nil && s.deps.Ads.Advertised(node.ID)
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
	var resp UpdateResult
	if req.Rotate {
		code, err := s.nodeCode(ctx, node, true)
		if err != nil {
			return nil, undo(err)
		}
		code.Advertised = advertised
		resp.Code = code
	}
	err := s.deps.Store.UpdateNode(ctx, prev, node)
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
func (s *Service) Remove(ctx context.Context, name string) (*RemoveResult, error) {
	p, exists, err := s.pairing(ctx, name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, notFound(name)
	}
	resp := &RemoveResult{Kind: p.Kind}
	if name == LocalOwner {
		resp.Kept = true
		return resp, s.deps.Store.RevokeCredentials(ctx, name)
	}
	endRemoval := s.beginRemoval(name)
	err = s.deletePairing(ctx, name, auth.PairPrincipal(name), auth.NodePrincipal(name))
	endRemoval()
	if err != nil {
		return nil, err
	}
	if p.Kind == pairapi.PairingWebhook {
		defer s.changed(ctx)
		if s.deps.Forget != nil {
			s.deps.Forget(name)
		}
		if s.deps.Ads != nil {
			if err := s.deps.Ads.Withdraw(name); err != nil {
				return nil, fmt.Errorf("withdrawing the advertisement of %s: %w", name, err)
			}
		}
	}
	return resp, nil
}

// PreviewRemove describes what [Service.Remove] would delete for the
// pairing called name, and changes nothing.
func (s *Service) PreviewRemove(ctx context.Context, name string) (*RemovalPreview, error) {
	p, exists, err := s.pairing(ctx, name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, notFound(name)
	}
	resp := &RemovalPreview{Kind: p.Kind, Accounts: []Account{}, AgentHomes: []string{}}
	links, err := s.deps.Store.LinksOf(ctx, name)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		resp.Accounts = append(resp.Accounts, Account{Channel: l.Channel, Account: l.Identity})
	}
	if name == LocalOwner {
		resp.Kept = true
		counts, err := s.deps.Store.PairingRemoval(ctx, name)
		if err != nil {
			return nil, err
		}
		resp.Tokens, resp.Codes = counts.Tokens, counts.Codes
		return resp, nil
	}
	principals := []string{auth.PairPrincipal(name), auth.NodePrincipal(name)}
	counts, err := s.deps.Store.PairingRemoval(ctx, name, principals...)
	if err != nil {
		return nil, err
	}
	resp.Tokens, resp.Codes, resp.Node = counts.Tokens, counts.Codes, p.Kind == pairapi.PairingWebhook
	resp.Schedules, resp.Watches, resp.Approvals = counts.Schedules, counts.Watches, counts.Questions
	resp.Tasks, resp.RunningTasks, resp.Conversations = counts.Tasks, counts.Unfinished, counts.Conversations
	if s.deps.Homes == nil {
		return resp, nil
	}
	for _, id := range principals {
		dir := s.deps.Homes.UserHomeDir(id)
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
	if s.deps.Users != nil {
		release, err := s.deps.Users.Stop(ctx, principals...)
		if err != nil {
			return fmt.Errorf("stopping what %s runs: %w", name, err)
		}
		defer release()
	}
	if s.deps.Homes != nil {
		for _, p := range principals {
			if err := s.deps.Homes.RemoveUserHome(p); err != nil {
				return fmt.Errorf("deleting the agent home of %s: %w", name, err)
			}
		}
	}
	return s.deps.Store.DeletePairing(ctx, name, principals...)
}

// SignOut revokes the Google sign-in tokens of email at every URL. The
// person can sign in again while --allow-email matches the email.
func (s *Service) SignOut(ctx context.Context, email string) (*SignOutResult, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return nil, invalid("%q is not an email address", email)
	}
	n, err := s.deps.Store.DeleteSignIns(ctx, email)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, &agenterr.NotFoundError{Kind: "Google sign-in of", Name: email}
	}
	return &SignOutResult{Email: email, Tokens: int(n)}, nil
}

// Get describes the pairing called name.
func (s *Service) Get(ctx context.Context, name string) (*Pairing, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(all, func(p Pairing) bool { return p.Name == name })
	if i < 0 {
		return nil, notFound(name)
	}
	return &all[i], nil
}

// List describes every pairing, by name.
func (s *Service) List(ctx context.Context) ([]Pairing, error) {
	all, err := s.deps.Store.Pairings(ctx)
	if err != nil {
		return nil, err
	}
	toks, err := s.deps.Store.Tokens(ctx)
	if err != nil {
		return nil, err
	}
	links, err := s.deps.Store.Links(ctx)
	if err != nil {
		return nil, err
	}
	codes, err := s.deps.Store.PendingCodes(ctx)
	if err != nil {
		return nil, err
	}
	nodeList, err := s.deps.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	nodesByID := map[string]store.Node{}
	for _, n := range nodeList {
		nodesByID[n.ID] = n
	}
	base := s.deps.BaseURL()
	out := make([]Pairing, 0, len(all))
	for _, p := range all {
		cp := Pairing{
			Name: p.Name, Kind: p.Kind, Role: p.Role, Principal: auth.PairingPrincipal(p.Name), Created: p.Created,
			Tokens: []Token{}, Accounts: []Account{}, PendingCodes: []time.Time{},
		}
		for _, t := range toks {
			if t.Pairing != p.Name {
				continue
			}
			cp.Tokens = append(cp.Tokens, Token{URL: t.URL, Created: t.Created, LastUsed: t.LastUsed})
			if t.LastUsed.After(cp.LastUsed) {
				cp.LastUsed = t.LastUsed
			}
		}
		for _, l := range links[p.Name] {
			cp.Accounts = append(cp.Accounts, Account{Channel: l.Channel, Account: l.Identity})
		}
		for _, c := range codes[p.Name] {
			cp.PendingCodes = append(cp.PendingCodes, c.Expires)
		}
		if n, ok := nodesByID[p.Name]; ok {
			cp.Principal = auth.NodePrincipal(n.ID)
			cp.Node = &NodeDetails{
				URL: mailboxapi.NodeURL(base, n.ID), Webhook: mailboxapi.RedactWebhookURL(n.Webhook), Description: n.Description,
				HeaderNames: slices.Sorted(maps.Keys(n.Headers)), Paired: n.TokenHash != "",
				Advertised: s.deps.Ads != nil && s.deps.Ads.Advertised(n.ID),
			}
			if s.deps.Mailbox != nil {
				cp.Node.Waiting, cp.Node.Open = s.deps.Mailbox(n.ID)
			}
		}
		out = append(out, cp)
	}
	return out, nil
}

// DirectConv returns the direct chat of user on a channel.
func DirectConv(ctx context.Context, st *store.Store, ch, user string) (string, error) {
	var links []store.Link
	if name, ok := auth.PrincipalPairing(user); ok {
		var err error
		if links, err = st.LinksOf(ctx, name); err != nil {
			return "", err
		}
	}
	for _, l := range links {
		if l.Channel == ch && l.Conv != "" {
			return l.Conv, nil
		}
	}
	return "", agenterr.WithHint(agenterr.Invalidf("%s has no chat on %s", auth.DisplayPrincipal(user), ch), agenterr.HintLinkChat, "")
}
