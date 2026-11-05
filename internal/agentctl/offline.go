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
	"maps"
	"os"
	"path/filepath"

	"github.com/a2aproject/a2a-serve/internal/access"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel/configured"
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

// offline is the [Manager] of a stopped agent, which changes its database
// and config.yaml while it holds the agent directory locked. The agent
// picks the changes up when it starts.
type offline struct {
	dir       agentdir.Dir
	release   func() error
	store     *store.Store
	pairings  *pairings.Service
	contacts  manage.Contacts
	schedules manage.Schedules
}

// openOffline opens the database of the agent in d, whose directory lock
// release releases.
func openOffline(d agentdir.Dir, release func() error) (*offline, error) {
	cfg, err := d.LoadConfig()
	if err != nil {
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
	return &offline{
		dir:     d,
		release: release,
		store:   st,
		pairings: &pairings.Service{Store: st, BaseURL: func() string { return base }, Ads: ads, Access: func(ctx context.Context) (*controlapi.Access, error) {
			all, err := st.Pairings(ctx)
			if err != nil {
				return nil, err
			}
			return access.Describe(cfg.Policy, access.NetworkOf(cfg), access.CountPairings(all)), nil
		}, Homes: d},
		contacts:  manage.Contacts{Store: st},
		schedules: manage.Schedules{Store: st},
	}, nil
}

// savedAccess describes who may call the agent in d with its saved config
// cfg. The pairings come from the running server, or from the database of
// a stopped agent.
func savedAccess(ctx context.Context, d agentdir.Dir, cfg *agentdir.Config, client *controlapi.Client, running bool) (_ *controlapi.Access, err error) {
	var n access.Pairings
	switch _, statErr := os.Stat(d.DBPath()); {
	case running:
		st, err := client.Status(ctx)
		if err != nil {
			return nil, err
		}
		if st.Access != nil {
			n = access.Pairings{All: st.Access.Pairings, Owners: st.Access.OwnerPairings}
		}
	case statErr == nil:
		st, err := store.Open(d.DBPath())
		if err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, st.Close()) }()
		all, err := st.Pairings(ctx)
		if err != nil {
			return nil, err
		}
		n = access.CountPairings(all)
	}
	return access.Describe(cfg.Policy, access.NetworkOf(cfg), n), nil
}

// Close closes the database and releases the agent directory.
func (o *offline) Close() error { return errors.Join(o.store.Close(), o.release()) }

func (o *offline) Running() bool { return false }

// Pair issues a pairing code.
func (o *offline) Pair(ctx context.Context, req controlapi.PairCodeRequest) (*controlapi.PairCodeResponse, error) {
	return o.pairings.Invite(ctx, req)
}

// Pairings lists the pairings.
func (o *offline) Pairings(ctx context.Context) ([]controlapi.Pairing, error) {
	return o.pairings.List(ctx)
}

// Pairing describes a pairing.
func (o *offline) Pairing(ctx context.Context, name string) (*controlapi.Pairing, error) {
	return o.pairings.Get(ctx, name)
}

// UpdatePairing changes a pairing.
func (o *offline) UpdatePairing(ctx context.Context, name string, req controlapi.PairingUpdate) (*controlapi.PairingUpdateResponse, error) {
	return o.pairings.Update(ctx, name, req)
}

// Revoke revokes a pairing.
func (o *offline) Revoke(ctx context.Context, name string) (*controlapi.RevokeResponse, error) {
	return o.pairings.Remove(ctx, name)
}

// SignOut revokes the Google sign-in tokens of an email.
func (o *offline) SignOut(ctx context.Context, email string) (*controlapi.SignOutResponse, error) {
	return o.pairings.SignOut(ctx, email)
}

// Contacts lists the contacts.
func (o *offline) Contacts(ctx context.Context) ([]controlapi.ContactInfo, error) {
	return o.contacts.List(ctx)
}

// AddContact adds a contact with a URL; finding one by name needs the
// private network of the running agent.
func (o *offline) AddContact(ctx context.Context, req controlapi.ContactRequest) (*controlapi.ContactInfo, error) {
	c, err := o.contacts.Add(ctx, req)
	if err != nil {
		return nil, err
	}
	info := manage.ContactInfo(c)
	return &info, nil
}

// DeleteContact removes a contact.
func (o *offline) DeleteContact(ctx context.Context, name string) error {
	return o.contacts.Remove(ctx, name)
}

// Schedules lists the schedules.
func (o *offline) Schedules(ctx context.Context) ([]controlapi.ScheduleInfo, error) {
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
func (o *offline) AddSchedule(ctx context.Context, req schedule.Request) (*controlapi.ScheduleInfo, error) {
	s, err := o.schedules.Add(ctx, req, auth.Owner(), "")
	if err != nil {
		return nil, err
	}
	info := manage.ScheduleInfo(s)
	return &info, nil
}

// DeleteSchedule removes a schedule.
func (o *offline) DeleteSchedule(ctx context.Context, name string) error {
	return o.schedules.Remove(ctx, name, auth.Owner())
}

// AddChannel checks the channel's credentials and saves it for the next
// start.
func (o *offline) AddChannel(ctx context.Context, req controlapi.ChannelRequest) (*controlapi.ChannelStatus, error) {
	cfg, err := o.dir.LoadConfig()
	if err != nil {
		return nil, err
	}
	if err := cfg.SetChannel(req.Kind, req.Token, req.Email); err != nil {
		return nil, agenterr.Invalid(err)
	}
	secrets, err := o.dir.LoadSecrets()
	if err != nil {
		return nil, err
	}
	maps.Copy(secrets, req.Secrets)
	if err := configured.Validate(ctx, cfg, secrets, req.Kind); err != nil {
		return nil, err
	}
	if err := o.dir.SaveSecrets(req.Secrets); err != nil {
		return nil, err
	}
	if err := o.dir.SaveConfig(cfg); err != nil {
		return nil, err
	}
	return &controlapi.ChannelStatus{Name: req.Kind}, nil
}

// DeleteChannel removes a channel from config.yaml.
func (o *offline) DeleteChannel(_ context.Context, kind string) error {
	cfg, err := o.dir.LoadConfig()
	if err != nil {
		return err
	}
	had := cfg.HasChannel(kind)
	if err := cfg.SetChannel(kind, "", nil); err != nil {
		return agenterr.Invalid(err)
	}
	if !had {
		return &agenterr.NotFoundError{Kind: "channel", Name: kind}
	}
	return o.dir.SaveConfig(cfg)
}
