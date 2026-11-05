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
	"github.com/a2aproject/a2a-serve/internal/agentops"
	"github.com/a2aproject/a2a-serve/internal/channel/configured"
	"github.com/a2aproject/a2a-serve/internal/contacts"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/gossip"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	schedulesvc "github.com/a2aproject/a2a-serve/internal/schedules"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// placeholderURL stands for the agent's URL in codes issued before its
// first start picked a port.
const placeholderURL = "<agent-url>"

// offline is the [Manager] of a stopped agent, which changes its database
// and config.yaml while it holds the agent directory locked. The agent
// picks the changes up when it starts.
type offline struct {
	*agentops.Service
	dir     agentdir.Dir
	release func() error
	store   *store.Store
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
	ops := agentops.New(agentops.Deps{
		Store:     st,
		Pairings:  pairings.New(pairings.Deps{Store: st, BaseURL: func() string { return base }, Ads: ads, Homes: d}),
		Contacts:  contacts.Service{Store: st},
		Schedules: schedulesvc.Service{Store: st},
		Config:    func() *agentdir.Config { return cfg },
	})
	return &offline{Service: ops, dir: d, release: release, store: st}, nil
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
	return agentops.AccessInfo(access.Describe(cfg.Policy, access.NetworkOf(cfg), n)), nil
}

// Close closes the database and releases the agent directory.
func (o *offline) Close() error { return errors.Join(o.store.Close(), o.release()) }

func (o *offline) Running() bool { return false }

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
	if err := o.dir.UpdateConfig(func(c *agentdir.Config) error { return c.SetChannel(req.Kind, req.Token, req.Email) }); err != nil {
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
	return o.dir.UpdateConfig(func(c *agentdir.Config) error { return c.SetChannel(kind, "", nil) })
}
