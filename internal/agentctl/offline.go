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

package agentctl

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/gossip"
	"github.com/a2aproject/a2a-serve/internal/manage"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/schedule"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// placeholderURL stands for the agent's URL in codes issued before its
// first start picked a port.
const placeholderURL = "<agent-url>"

// Offline manages the pairings, contacts and schedules of a stopped agent in
// its database, with the methods of [controlapi.Client] that do so. The
// agent picks the changes up when it starts.
type Offline struct {
	store     *store.Store
	pairings  *pairings.Service
	contacts  manage.Contacts
	schedules manage.Schedules
}

// OpenOffline opens the database of the agent in d.
func OpenOffline(d agentdir.Dir) (*Offline, error) {
	cfg, err := d.LoadConfig()
	if err != nil {
		return nil, err
	}
	if err := d.Init(); err != nil {
		return nil, err
	}
	st, err := store.Open(d.DBPath())
	if err != nil {
		return nil, err
	}
	ads, err := gossip.Open(filepath.Join(d.StateDir(), "ads.json"), cfg.Name)
	if err != nil {
		return nil, errors.Join(err, st.Close())
	}
	base := cfg.LocalURL()
	if base == "" {
		base = placeholderURL
	}
	return &Offline{
		store:     st,
		pairings:  &pairings.Service{Store: st, BaseURL: func() string { return base }, Ads: ads},
		contacts:  manage.Contacts{Store: st},
		schedules: manage.Schedules{Store: st},
	}, nil
}

// Close closes the database.
func (o *Offline) Close() error { return o.store.Close() }

// Pair issues a pairing code.
func (o *Offline) Pair(ctx context.Context, req controlapi.PairCodeRequest) (*controlapi.PairCodeResponse, error) {
	return o.pairings.Invite(ctx, req)
}

// Pairings lists the pairings.
func (o *Offline) Pairings(ctx context.Context) ([]controlapi.Pairing, error) {
	return o.pairings.List(ctx)
}

// Pairing describes a pairing.
func (o *Offline) Pairing(ctx context.Context, name string) (*controlapi.Pairing, error) {
	return o.pairings.Get(ctx, name)
}

// UpdatePairing changes a pairing.
func (o *Offline) UpdatePairing(ctx context.Context, name string, req controlapi.PairingUpdate) (*controlapi.PairingUpdateResponse, error) {
	return o.pairings.Update(ctx, name, req)
}

// Revoke revokes a pairing.
func (o *Offline) Revoke(ctx context.Context, name string) (*controlapi.RevokeResponse, error) {
	kind, err := o.pairings.Remove(ctx, name)
	if err != nil {
		return nil, err
	}
	return &controlapi.RevokeResponse{Kind: kind}, nil
}

// Contacts lists the contacts.
func (o *Offline) Contacts(ctx context.Context) ([]controlapi.ContactInfo, error) {
	return o.contacts.List(ctx)
}

// AddContact adds a contact with a URL; finding one by name needs the
// private network of the running agent.
func (o *Offline) AddContact(ctx context.Context, req controlapi.ContactRequest) (*controlapi.ContactInfo, error) {
	c, err := o.contacts.Add(ctx, req)
	if err != nil {
		return nil, err
	}
	info := manage.ContactInfo(c)
	return &info, nil
}

// DeleteContact removes a contact.
func (o *Offline) DeleteContact(ctx context.Context, name string) error {
	err := o.contacts.Remove(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return clierr.Usage("no contact " + name)
	}
	return err
}

// Schedules lists the schedules.
func (o *Offline) Schedules(ctx context.Context) ([]controlapi.ScheduleInfo, error) {
	all, err := o.schedules.Visible(ctx, auth.Owner())
	if err != nil {
		return nil, err
	}
	out := []controlapi.ScheduleInfo{}
	for _, s := range all {
		out = append(out, manage.ScheduleInfo(s))
	}
	return out, nil
}

// AddSchedule adds a schedule made by the owner.
func (o *Offline) AddSchedule(ctx context.Context, req schedule.Request) (*controlapi.ScheduleInfo, error) {
	s, err := o.schedules.Add(ctx, req, auth.Owner(), "")
	if err != nil {
		return nil, err
	}
	info := manage.ScheduleInfo(s)
	return &info, nil
}

// DeleteSchedule removes a schedule.
func (o *Offline) DeleteSchedule(ctx context.Context, name string) error {
	err := o.schedules.Remove(ctx, name, auth.Owner())
	if errors.Is(err, store.ErrNotFound) {
		return clierr.Usage("no schedule " + name)
	}
	return err
}
