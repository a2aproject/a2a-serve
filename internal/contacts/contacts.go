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

// Package contacts adds, lists and removes the agents an agent may call. The
// control socket of a running agent and the CLI of a stopped one share it.
package contacts

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/cardinfo"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Contact routes: how the agent reaches a contact, whatever the scheme of
// its URL.
const (
	// ViaPrivate goes through the private network provider's HTTP client.
	ViaPrivate = "private"
	// ViaDirect is an ordinary connection from this machine.
	ViaDirect = "direct"
)

// ErrNeedsNetwork is returned for a contact found by name, which needs the
// private network of a running agent. It matches [agenterr.ErrNotRunning].
var ErrNeedsNetwork error = &agenterr.NotRunningError{Msg: "finding an agent by name needs its private network", Hint: agenterr.HintStartOrURL}

// errRefreshNeedsNetwork is returned for a contact on the private network
// whose card is refreshed while the agent is stopped.
var errRefreshNeedsNetwork error = &agenterr.NotRunningError{Msg: "fetching the card of a contact on the private network needs the agent's private network", Hint: agenterr.HintStart}

// Service adds, lists and removes the agents an agent may call.
type Service struct {
	Store *store.Store
	// Locate returns the URL of the contact name on the private network
	// when url is empty, and the route to the URL. Nil adds only contacts
	// with a URL, directly.
	Locate func(ctx context.Context, name, url string) (string, string, error)
	// HTTPClient returns the client which reaches a contact. Nil sends the
	// contact's token over the default client, and reaches no contacts on
	// the private network.
	HTTPClient func(c store.Contact) *http.Client
	// Changed is called after a contact is added or removed, or its card
	// changed. Optional.
	Changed func(ctx context.Context)
}

// Request adds a contact. Without URL, the contact is looked up by name
// among the peers on the private network. Code pairs with the contact;
// Token sets the outbound token directly.
type Request struct {
	Name  string
	URL   string
	Code  string
	Token string
}

// Add pairs with an agent with a code, or stores its token, and checks that
// it answers.
func (m Service) Add(ctx context.Context, req Request) (store.Contact, error) {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || strings.ContainsAny(req.Name, " /") {
		return store.Contact{}, agenterr.Invalidf("contact name must be non-empty, without spaces or slashes")
	}
	if req.Name == pairings.SchedulesAgent {
		return store.Contact{}, agenterr.Invalidf("%q is the built-in agent that manages schedules; pick another name", pairings.SchedulesAgent)
	}
	if _, err := m.Store.Node(ctx, req.Name); err == nil {
		return store.Contact{}, agenterr.Conflictf("%q is the name of a webhook node; pick another name", req.Name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Contact{}, err
	}
	c := store.Contact{Name: req.Name, URL: strings.TrimSuffix(req.URL, "/"), Token: req.Token, Via: ViaDirect}
	switch {
	case m.Locate != nil:
		u, via, err := m.Locate(ctx, req.Name, c.URL)
		if err != nil {
			return store.Contact{}, err
		}
		c.URL, c.Via = u, via
	case c.URL == "":
		return store.Contact{}, ErrNeedsNetwork
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if req.Code != "" {
		resp, err := pairapi.Pair(ctx, m.client(c), c.URL, req.Code, pairapi.PairingAgent)
		if err != nil {
			return store.Contact{}, err
		}
		if resp.Kind == pairapi.KindNode {
			return store.Contact{}, fmt.Errorf("pairing with %s: the code is for a webhook node, not a caller", c.URL)
		}
		c.Token = resp.Token
	}
	card, err := network.FetchCard(ctx, m.client(c), c.URL)
	if err != nil {
		return store.Contact{}, fmt.Errorf("can't reach %s: %w", c.URL, err)
	}
	c.Card = cardinfo.Of(card)
	if err := m.Store.PutContact(ctx, c); err != nil {
		return store.Contact{}, err
	}
	m.changed(ctx)
	return c, nil
}

func (m Service) client(c store.Contact) *http.Client {
	if m.HTTPClient != nil {
		return m.HTTPClient(c)
	}
	return TokenClient(http.DefaultClient, c)
}

func (m Service) changed(ctx context.Context) {
	if m.Changed != nil {
		m.Changed(ctx)
	}
}

// Refresh fetches the cards of the contacts names, or of all contacts
// without names, and stores those that changed. It returns the contacts it
// refreshed, and an error naming each contact it couldn't.
func (m Service) Refresh(ctx context.Context, names ...string) ([]store.Contact, error) {
	var all []store.Contact
	if len(names) == 0 {
		var err error
		if all, err = m.Store.Contacts(ctx); err != nil {
			return nil, err
		}
	}
	for _, name := range names {
		c, err := m.Store.Contact(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return nil, &agenterr.NotFoundError{Kind: "contact", Name: name}
		}
		if err != nil {
			return nil, err
		}
		all = append(all, c)
	}
	var (
		out     []store.Contact
		errs    []error
		changed bool
	)
	for _, c := range all {
		card, err := m.fetchCard(ctx, c)
		if err != nil {
			errs = append(errs, fmt.Errorf("refreshing the card of %s: %w", c.Name, err))
			continue
		}
		if !card.Equal(c.Card) {
			err := m.Store.SetContactCard(ctx, c.Name, c.URL, card)
			if errors.Is(err, store.ErrNotFound) {
				// The contact was removed or replaced meanwhile.
				continue
			}
			if err != nil {
				return out, err
			}
			changed = true
		}
		c.Card = card
		out = append(out, c)
	}
	if changed {
		m.changed(ctx)
	}
	return out, errors.Join(errs...)
}

func (m Service) fetchCard(ctx context.Context, c store.Contact) (cardinfo.Summary, error) {
	if c.Via == ViaPrivate && m.HTTPClient == nil {
		return cardinfo.Summary{}, errRefreshNeedsNetwork
	}
	card, err := network.FetchCard(ctx, m.client(c), c.URL)
	if err != nil {
		return cardinfo.Summary{}, err
	}
	return cardinfo.Of(card), nil
}

// List returns the contacts.
func (m Service) List(ctx context.Context) ([]store.Contact, error) {
	return m.Store.Contacts(ctx)
}

// Remove removes a contact. It returns [store.ErrNotFound] for a name which
// isn't one.
func (m Service) Remove(ctx context.Context, name string) error {
	err := m.Store.DeleteContact(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return &agenterr.NotFoundError{Kind: "contact", Name: name}
	}
	if err != nil {
		return err
	}
	m.changed(ctx)
	return nil
}

// TokenClient returns base, which sends the token of c with requests to the
// contact's host. Requests redirected elsewhere go without it, so that the
// token doesn't leak to another host.
func TokenClient(base *http.Client, c store.Contact) *http.Client {
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	h := http.Header{}
	if c.Token != "" {
		h.Set("Authorization", "Bearer "+c.Token)
	}
	host := ""
	if u, err := url.Parse(c.URL); err == nil {
		host = u.Host
	}
	return &http.Client{Transport: &headerTransport{base: rt, host: host, header: h}, CheckRedirect: base.CheckRedirect}
}

type headerTransport struct {
	base   http.RoundTripper
	host   string
	header http.Header
}

func (t *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.EqualFold(r.URL.Host, t.host) {
		return t.base.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	maps.Copy(r.Header, t.header)
	return t.base.RoundTrip(r)
}
