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
// pairings, contacts and schedules. The control socket of a running agent
// serves them, and the CLI calls them directly for a stopped one, so both
// behave the same. It maps the results of the domain services to the DTOs
// of the control API.
package agentops

import (
	"context"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/schedules"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Deps are the dependencies of [Service].
type Deps struct {
	Store     *store.Store
	Pairings  *pairings.Service
	Contacts  contacts.Service
	Schedules schedules.Service
	// Config returns the config the agent runs with.
	Config func() *agentdir.Config
	// Network returns the networks the agent is on. Nil derives them from
	// the config.
	Network func() access.Network
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
	cfg := s.deps.Config()
	net := access.NetworkOf(cfg)
	if s.deps.Network != nil {
		net = s.deps.Network()
	}
	return access.Describe(cfg.Policy, net, access.CountPairings(all)), nil
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
	c, err := s.deps.Pairings.Invite(ctx, pairings.InviteRequest(req))
	if err != nil {
		return nil, err
	}
	resp := codeInfo(c)
	if resp.Access, err = s.accessInfo(ctx); err != nil {
		return nil, err
	}
	return resp, nil
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
	return &info, nil
}

// UpdatePairing changes a pairing.
func (s *Service) UpdatePairing(ctx context.Context, name string, req controlapi.PairingUpdate) (*controlapi.PairingUpdateResponse, error) {
	r, err := s.deps.Pairings.Update(ctx, name, pairings.UpdateRequest(req))
	if err != nil {
		return nil, err
	}
	resp := &controlapi.PairingUpdateResponse{Pairing: pairingInfo(r.Pairing)}
	if r.Code != nil {
		resp.Code = codeInfo(r.Code)
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
	return removalPreview(p), nil
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
	if s.deps.RunSchedule == nil {
		return nil, agenterr.ErrNotRunning
	}
	sc, err := s.deps.RunSchedule(ctx, name)
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
