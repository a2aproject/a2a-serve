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
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// Contact routes.
const (
	ViaPrivate = "private"
	ViaHTTPS   = "https"
)

// ErrNeedsNetwork is returned for a contact found by name, which needs the
// private network of a running agent.
var ErrNeedsNetwork error = clierr.NotRunning(errors.New("finding an agent by name needs its private network: start it with `a2a-serve start --detach`, or pass the agent's URL with --url"))

// Service adds, lists and removes the agents an agent may call.
type Service struct {
	Store *store.Store
	// Locate returns the URL of the contact name on the private network
	// when url is empty, and the route to the URL. Nil adds only contacts
	// with a URL, over HTTPS.
	Locate func(ctx context.Context, name, url string) (string, string, error)
	// HTTPClient returns the client which reaches a contact. Nil sends the
	// contact's token over the default client.
	HTTPClient func(c store.Contact) *http.Client
	// Changed is called after a contact is added or removed. Optional.
	Changed func(ctx context.Context)
}

// Add pairs with an agent with a code, or stores its token, and checks that
// it answers.
func (m Service) Add(ctx context.Context, req controlapi.ContactRequest) (store.Contact, error) {
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
	c := store.Contact{Name: req.Name, URL: strings.TrimSuffix(req.URL, "/"), Token: req.Token, Via: ViaHTTPS}
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
	if _, err := network.FetchCard(ctx, m.client(c), c.URL); err != nil {
		return store.Contact{}, fmt.Errorf("can't reach %s: %w", c.URL, err)
	}
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

// List describes the contacts.
func (m Service) List(ctx context.Context) ([]controlapi.ContactInfo, error) {
	cs, err := m.Store.Contacts(ctx)
	if err != nil {
		return nil, err
	}
	out := []controlapi.ContactInfo{}
	for _, c := range cs {
		out = append(out, Info(c))
	}
	return out, nil
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

// Info describes a contact for the control API.
func Info(c store.Contact) controlapi.ContactInfo {
	return controlapi.ContactInfo{Name: c.Name, URL: c.URL, Via: c.Via, HasToken: c.Token != ""}
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
