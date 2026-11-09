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

// Package agentops implements the management operations of an agent:
// pairings, contacts, schedules and messengers. The control socket of a
// running agent serves them, and the CLI calls them directly for a stopped
// one, so both behave the same. [Hooks] add what only a running agent
// does, such as starting a messenger. It maps the results of the domain
// services to the DTOs of the control API.
package agentops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/messenger/configured"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/schedulesvc"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Deps are the dependencies of [Service].
type Deps struct {
	Store     *store.Store
	Pairings  *pairings.Service
	Contacts  contacts.Service
	Schedules schedulesvc.Service
	// Runtime has the config and secrets the agent runs with.
	Runtime *agentdir.Runtime
	// Network returns the networks the agent is on. Nil derives them from
	// the config.
	Network func() access.Network
	Hooks   Hooks
}

// Hooks are the side effects of the operations on a running agent. A
// stopped agent has none.
type Hooks struct {
	// NewMessenger creates the messenger of kind configured in cfg. Nil creates
	// it without state, which only suits checking its credentials.
	NewMessenger func(cfg *agentdir.Config, secrets agentdir.Secrets, kind string) (messenger.Messenger, error)
	// MessengerAdded starts a messenger once it is saved, and describes it. Nil
	// describes it by name.
	MessengerAdded func(ctx context.Context, ch messenger.Messenger) *controlapi.MessengerStatus
	// MessengerRemoved stops a messenger once it is removed. Optional.
	MessengerRemoved func(kind string)
	// RunSchedule starts a run of a schedule at once. Nil refuses runs,
	// which need the running server.
	RunSchedule func(ctx context.Context, name string) (store.Schedule, error)
}

// Service runs the management operations of an agent. Only the owner
// reaches them, so changes are made as the owner.
type Service struct{ deps Deps }

// New returns the operations over deps.
func New(deps Deps) *Service { return &Service{deps: deps} }

// Access describes who may call the agent now.
func (s *Service) Access(ctx context.Context) (*access.Description, error) {
	all, err := s.deps.Store.Pairings(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := s.deps.Store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	cfg := s.deps.Runtime.Config()
	net := access.NetworkOf(cfg)
	if s.deps.Network != nil {
		net = s.deps.Network()
	}
	n := access.CountPairings(all, nodes)
	a := access.DescribeConfig(cfg, net, n)
	saved := s.savedSandbox(cfg)
	if saved == nil {
		return a, nil
	}
	// A note that the rules waiting for a restart fix is left out, since
	// their grants wait only for the secrets that the restart loads.
	after := access.DescribeConfig(saved, net, n)
	a.SandboxNotes = slices.DeleteFunc(a.SandboxNotes, func(note string) bool { return !slices.Contains(after.SandboxNotes, note) })
	if after.GuestNote == "" {
		a.GuestNote = ""
	}
	return a, nil
}

// savedSandbox returns config.yaml when its sandbox rules or secrets differ
// from those of cfg, the config in use, as when rules wait for a restart.
func (s *Service) savedSandbox(cfg *agentdir.Config) *agentdir.Config {
	saved, err := s.deps.Runtime.SavedConfig()
	if err != nil {
		slog.Warn("reading config.yaml for the sandbox rules waiting for a restart", "error", err)
		return nil
	}
	if reflect.DeepEqual(saved.Policy.Sandbox, cfg.Policy.Sandbox) && slices.Equal(saved.Secrets, cfg.Secrets) {
		return nil
	}
	return saved
}

func (s *Service) accessInfo(ctx context.Context) (*controlapi.Access, error) {
	a, err := s.Access(ctx)
	if err != nil {
		return nil, err
	}
	return AccessInfo(a), nil
}

// Pair issues a pairing code.
func (s *Service) Pair(ctx context.Context, req controlapi.PairCodeRequest) (*controlapi.PairCodeResponse, error) {
	role, err := s.pairingRole(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	undo, err := s.changeSandbox(req.Name, req.Owner || role == auth.RoleOwner, req.Sandbox)
	if err != nil {
		return nil, err
	}
	c, err := s.deps.Pairings.Invite(ctx, pairings.InviteRequest{
		Name: req.Name, Owner: req.Owner, Webhook: req.Webhook, Headers: req.Headers, Description: req.Description, Advertised: req.Advertised,
	})
	if err != nil {
		return nil, errors.Join(err, undo())
	}
	resp := s.codeInfo(c)
	p, err := s.deps.Pairings.Get(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	resp.Sandbox = s.ruledSandbox(*p)
	resp.Hints = append(resp.Hints, s.sandboxHints(req.Sandbox)...)
	if resp.Access, err = s.accessInfo(ctx); err != nil {
		return nil, err
	}
	return resp, nil
}

// changeSandbox saves ch, if set, as the change of the sandbox rule of the
// pairing name, which is an owner if owner, and returns the function that
// undoes it. Owners refuse a rule of their own.
func (s *Service) changeSandbox(name string, owner bool, ch *controlapi.SandboxChange) (undo func() error, err error) {
	if ch == nil {
		return func() error { return nil }, nil
	}
	if owner {
		return nil, errOwnerSandbox
	}
	return s.setSandbox(name, *ch)
}

// Pairings lists the pairings.
func (s *Service) Pairings(ctx context.Context) ([]controlapi.Pairing, error) {
	all, err := s.deps.Pairings.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]controlapi.Pairing, len(all))
	for i, p := range all {
		out[i] = pairingInfo(p)
		out[i].Sandbox = s.sandboxOf(p)
	}
	return out, nil
}

// Pairing describes a pairing.
func (s *Service) Pairing(ctx context.Context, name string) (*controlapi.Pairing, error) {
	p, err := s.deps.Pairings.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	info := pairingInfo(*p)
	info.Sandbox = s.sandboxOf(*p)
	return &info, nil
}

// UpdatePairing changes a pairing.
func (s *Service) UpdatePairing(ctx context.Context, name string, req controlapi.PairingUpdate) (*controlapi.PairingUpdateResponse, error) {
	role, err := s.pairingRole(ctx, name)
	if err != nil {
		return nil, err
	}
	if role == "" {
		return nil, &agenterr.NotFoundError{Kind: "pairing", Name: name}
	}
	owner := role == auth.RoleOwner
	if req.Owner != nil {
		owner = *req.Owner
	}
	undo, err := s.changeSandbox(name, owner, req.Sandbox)
	if err != nil {
		return nil, err
	}
	r, err := s.deps.Pairings.Update(ctx, name, pairings.UpdateRequest{
		Owner: req.Owner, Kind: req.Kind, Webhook: req.Webhook, Headers: req.Headers, Description: req.Description, Advertised: req.Advertised, Rotate: req.Rotate,
	})
	if err != nil {
		return nil, errors.Join(err, undo())
	}
	hints := s.sandboxHints(req.Sandbox)
	if req.Owner != nil && *req.Owner {
		// Owners share your container, so a rule of the pairing would only
		// come back if it stopped being one.
		removed, err := s.removeSandbox(name)
		if err != nil {
			return nil, err
		}
		if removed {
			hints = append(hints, fmt.Sprintf("removed %s's sandbox rule: owners share your agent container", name))
		}
	}
	resp := &controlapi.PairingUpdateResponse{Pairing: pairingInfo(r.Pairing), Hints: hints}
	resp.Pairing.Sandbox = s.sandboxOf(r.Pairing)
	if r.Code != nil {
		resp.Code = s.codeInfo(r.Code)
	}
	if r.Code != nil && r.Code.Node == nil && req.Rotate {
		// The callers of the pairing joined before, and the code keeps their
		// conversations and tasks.
		resp.Code.PairCommand += " --replace"
	}
	if resp.Access, err = s.accessInfo(ctx); err != nil {
		return nil, err
	}
	return resp, nil
}

// Revoke removes a pairing, or revokes the credentials of the local one.
// Who may still call without a pairing is reported for removed callers,
// since it can include them.
func (s *Service) Revoke(ctx context.Context, name string) (*controlapi.RevokeResponse, error) {
	r, err := s.deps.Pairings.Remove(ctx, name)
	if err != nil {
		return nil, err
	}
	resp := &controlapi.RevokeResponse{Kind: r.Kind, Kept: r.Kept}
	if r.Kept {
		return resp, nil
	}
	// The pairing goes first, so that its callers can't fall back to
	// broader rules meanwhile.
	if _, err := s.removeSandbox(name); err != nil {
		return nil, err
	}
	if resp.Access, err = s.accessInfo(ctx); err != nil {
		return nil, err
	}
	if r.Kind != pairapi.PairingWebhook {
		resp.StillAllowed = resp.Access.Others
	}
	return resp, nil
}

// PreviewRevoke describes what Revoke would remove, and removes nothing.
func (s *Service) PreviewRevoke(ctx context.Context, name string) (*controlapi.RemovalPreview, error) {
	p, err := s.deps.Pairings.PreviewRemove(ctx, name)
	if err != nil {
		return nil, err
	}
	info := removalPreview(p)
	if p.Kept {
		return info, nil
	}
	saved, err := s.deps.Runtime.SavedConfig()
	if err != nil {
		return nil, err
	}
	m := agentdir.PairingMatcher(name)
	for _, r := range saved.Policy.Sandbox {
		if slices.Contains(r.Match, m) {
			info.SandboxRule = agentdir.ShowSandboxRule(r)
		}
	}
	return info, nil
}

// SignOut revokes the Google sign-in tokens of an email.
func (s *Service) SignOut(ctx context.Context, email string) (*controlapi.SignOutResponse, error) {
	r, err := s.deps.Pairings.SignOut(ctx, email)
	if err != nil {
		return nil, err
	}
	resp := &controlapi.SignOutResponse{Email: r.Email, Tokens: r.Tokens}
	if resp.Access, err = s.accessInfo(ctx); err != nil {
		return nil, err
	}
	return resp, nil
}

// Contacts lists the contacts.
func (s *Service) Contacts(ctx context.Context) ([]controlapi.ContactInfo, error) {
	all, err := s.deps.Contacts.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []controlapi.ContactInfo{}
	for _, c := range all {
		out = append(out, contactInfo(c))
	}
	return out, nil
}

// AddContact adds a contact. Finding one by name needs the private network
// of the running agent.
func (s *Service) AddContact(ctx context.Context, req controlapi.ContactRequest) (*controlapi.ContactInfo, error) {
	c, err := s.deps.Contacts.Add(ctx, contacts.Request(req))
	if err != nil {
		return nil, err
	}
	info := contactInfo(c)
	return &info, nil
}

// RefreshContacts fetches the card of the contact req names, or of every
// contact, and returns the contacts refreshed. A contact that can't be
// reached fails the call, after the others are refreshed.
func (s *Service) RefreshContacts(ctx context.Context, req controlapi.ContactRefreshRequest) ([]controlapi.ContactInfo, error) {
	var names []string
	if req.Name != "" {
		names = []string{req.Name}
	}
	refreshed, err := s.deps.Contacts.Refresh(ctx, names...)
	if err != nil {
		return nil, err
	}
	out := []controlapi.ContactInfo{}
	for _, c := range refreshed {
		out = append(out, contactInfo(c))
	}
	return out, nil
}

// DeleteContact removes a contact.
func (s *Service) DeleteContact(ctx context.Context, name string) error {
	return s.deps.Contacts.Remove(ctx, name)
}

// Schedules lists the schedules.
func (s *Service) Schedules(ctx context.Context) ([]controlapi.ScheduleInfo, error) {
	all, err := s.deps.Schedules.Visible(ctx, auth.Owner())
	if err != nil {
		return nil, err
	}
	out := []controlapi.ScheduleInfo{}
	for _, sc := range all {
		out = append(out, scheduleInfo(sc))
	}
	return out, nil
}

// AddSchedule adds a schedule made by the owner.
func (s *Service) AddSchedule(ctx context.Context, req schedule.Request) (*controlapi.ScheduleInfo, error) {
	sc, err := s.deps.Schedules.Add(ctx, req, auth.Owner(), "")
	if err != nil {
		return nil, err
	}
	info := scheduleInfo(sc)
	return &info, nil
}

// RunSchedule starts a run of a schedule at once.
func (s *Service) RunSchedule(ctx context.Context, name string) (*controlapi.ScheduleInfo, error) {
	if s.deps.Hooks.RunSchedule == nil {
		return nil, agenterr.ErrNotRunning
	}
	sc, err := s.deps.Hooks.RunSchedule(ctx, name)
	if err != nil {
		return nil, err
	}
	info := scheduleInfo(sc)
	return &info, nil
}

// DeleteSchedule removes a schedule.
func (s *Service) DeleteSchedule(ctx context.Context, name string) error {
	return s.deps.Schedules.Remove(ctx, name, auth.Owner())
}

// AddMessenger checks the messenger's credentials, saves it and its secrets,
// and starts it on a running agent.
func (s *Service) AddMessenger(ctx context.Context, req controlapi.MessengerRequest) (*controlapi.MessengerStatus, error) {
	before, err := s.Access(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.addMessenger(ctx, req)
	if err != nil {
		return nil, err
	}
	after, err := s.Access(ctx)
	if err != nil {
		return nil, err
	}
	st.Access = ChangedAccess(before, after)
	return st, nil
}

func (s *Service) addMessenger(ctx context.Context, req controlapi.MessengerRequest) (*controlapi.MessengerStatus, error) {
	secrets, err := s.deps.Runtime.SavedSecrets()
	if err != nil {
		return nil, err
	}
	maps.Copy(secrets, req.Secrets)
	next := s.deps.Runtime.Config()
	if err := next.SetMessenger(req.Kind, req.Token, req.Email); err != nil {
		return nil, agenterr.Invalid(err)
	}
	ch, err := s.newMessenger(next, secrets, req.Kind)
	if err != nil {
		return nil, agenterr.Invalid(err)
	}
	if err := messenger.Validate(ctx, ch); err != nil {
		return nil, err
	}
	if err := s.deps.Runtime.SaveSecrets(req.Secrets); err != nil {
		return nil, err
	}
	if err := s.deps.Runtime.Update(func(c *agentdir.Config) error { return c.SetMessenger(req.Kind, req.Token, req.Email) }); err != nil {
		return nil, err
	}
	if s.deps.Hooks.MessengerAdded == nil {
		return &controlapi.MessengerStatus{Name: req.Kind}, nil
	}
	return s.deps.Hooks.MessengerAdded(ctx, ch), nil
}

func (s *Service) newMessenger(cfg *agentdir.Config, secrets agentdir.Secrets, kind string) (messenger.Messenger, error) {
	if s.deps.Hooks.NewMessenger != nil {
		return s.deps.Hooks.NewMessenger(cfg, secrets, kind)
	}
	return configured.New(cfg, secrets, kind, configured.Options{})
}

// DeleteMessenger removes a messenger from the config, and stops it on a
// running agent.
func (s *Service) DeleteMessenger(ctx context.Context, kind string) (*controlapi.MessengerRemoval, error) {
	if err := (&agentdir.Config{}).SetMessenger(kind, "", nil); err != nil {
		return nil, agenterr.Invalid(err)
	}
	if !s.deps.Runtime.Config().HasMessenger(kind) {
		return nil, &agenterr.NotFoundError{Kind: "messenger", Name: kind}
	}
	before, err := s.Access(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Runtime.Update(func(c *agentdir.Config) error { return c.SetMessenger(kind, "", nil) }); err != nil {
		return nil, err
	}
	if s.deps.Hooks.MessengerRemoved != nil {
		s.deps.Hooks.MessengerRemoved(kind)
	}
	after, err := s.Access(ctx)
	if err != nil {
		return nil, err
	}
	return &controlapi.MessengerRemoval{Removed: kind, Access: ChangedAccess(before, after)}, nil
}
