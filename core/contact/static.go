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

package contact

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"github.com/a2aproject/a2a-serve/internal/egressrules"
	"github.com/a2aproject/a2a-serve/internal/names"
)

// Bounds of a [Static] directory.
const (
	// MaxContacts bounds the contacts of a Static directory.
	MaxContacts = 100
	// maxResponse bounds a response from a contact, except event streams.
	maxResponse = 32 << 20
	// fetchTimeout bounds a fetch of a card.
	fetchTimeout = 10 * time.Second
)

// Entry is a contact of a [Static] directory.
type Entry struct {
	// Name is what the agent calls the contact by.
	Name string
	// URL is the https:// URL of the contact's agent card, or unix://<path>
	// of a socket that serves the contact.
	URL string
	// Auth is the value of the Authorization header of the requests to the
	// contact, or "". It is a secret: it goes only over https to the card's
	// host and port, or through the socket, and is never logged.
	Auth string
}

// StaticConfig configures a [Static] directory.
type StaticConfig struct {
	Contacts []Entry
	// Refresh is how often the cards are fetched again; zero is an hour.
	Refresh time.Duration
	// Retry is how often a card that failed is fetched again; zero is a
	// minute.
	Retry time.Duration
	// Transport sends the requests to https:// contacts; nil uses a clone
	// of http.DefaultTransport.
	Transport http.RoundTripper
	// Log receives the records of the directory; nil uses the default
	// logger.
	Log *slog.Logger
}

// Static is a [Directory] of a fixed list of contacts. It fetches their
// cards at start and on a period. A contact whose card can't be fetched is
// refused until a later fetch succeeds. Every turn may call every contact.
type Static struct {
	refresh, retry time.Duration
	log            *slog.Logger
	contacts       []*staticContact

	stop context.CancelFunc
	done chan struct{}
}

var _ Directory = (*Static)(nil)

// Status is the state of a contact of a [Static] directory.
type Status struct {
	Name string
	// Ready is set while the contact can be called: its last fetch of the
	// card succeeded.
	Ready bool
	// Reason is why the last fetch of the card failed.
	Reason string
	// Card is the description and skills of the contact's card from the
	// last fetch that succeeded, shortened as untrusted text that an agent
	// may read; nil before one.
	Card *a2a.AgentCard
	// Checked is when the card was last fetched.
	Checked time.Time
}

type staticContact struct {
	name    string
	cardURL string
	// display is how errors name the contact's URL, which may carry
	// secrets, and secrets are the parts of the URL that may.
	display string
	secrets []string
	http    *http.Client

	mu      sync.Mutex
	card    *a2a.AgentCard
	raw     []byte
	client  *a2aclient.Client
	reason  string
	checked time.Time
	ok      time.Time
}

// NewStatic validates the contacts of cfg, fetches their cards, and
// fetches them again until ctx ends or Close. It fails only for invalid
// contacts; [Static.Status] tells which cards it fetched.
func NewStatic(ctx context.Context, cfg StaticConfig) (*Static, error) {
	if len(cfg.Contacts) > MaxContacts {
		return nil, fmt.Errorf("contact: %d contacts, want at most %d", len(cfg.Contacts), MaxContacts)
	}
	s := &Static{refresh: cfg.Refresh, retry: cfg.Retry, log: cfg.Log, done: make(chan struct{})}
	if s.refresh <= 0 {
		s.refresh = time.Hour
	}
	if s.retry <= 0 {
		s.retry = time.Minute
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	for _, e := range cfg.Contacts {
		if slices.ContainsFunc(s.contacts, func(c *staticContact) bool { return strings.EqualFold(c.name, e.Name) }) {
			return nil, fmt.Errorf("contact: %q is listed twice", e.Name)
		}
		c, err := newStaticContact(e, cfg.Transport)
		if err != nil {
			return nil, err
		}
		s.contacts = append(s.contacts, c)
	}
	s.fetchAll(ctx, func(*staticContact) bool { return true })
	ctx, s.stop = context.WithCancel(ctx)
	go s.loop(ctx)
	return s, nil
}

func newStaticContact(e Entry, transport http.RoundTripper) (*staticContact, error) {
	if !names.Valid(e.Name) {
		return nil, fmt.Errorf("contact: invalid name %q: want %s", e.Name, names.Rule)
	}
	if strings.ContainsAny(e.Auth, "\r\n") {
		return nil, fmt.Errorf("contact: the auth header of %s has a line break", e.Name)
	}
	u, err := url.Parse(e.URL)
	if err != nil {
		return nil, fmt.Errorf("contact: invalid URL of %s", e.Name)
	}
	c := &staticContact{name: e.Name}
	var rt http.RoundTripper
	cardOrigin := ""
	switch {
	case u.Scheme == "https" && u.Host != "":
		c.cardURL, cardOrigin = e.URL, origin(u)
		c.display = (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
		if p, ok := u.User.Password(); ok {
			c.secrets = append(c.secrets, p)
		}
		if u.RawQuery != "" {
			c.secrets = append(c.secrets, u.RawQuery)
		}
		rt = transport
		if rt == nil {
			rt = http.DefaultTransport.(*http.Transport).Clone()
		}
	case u.Scheme == "unix" && u.Host == "" && strings.HasPrefix(u.Path, "/"):
		c.cardURL = "http://contact"
		rt = unixTransport(u.Path)
	default:
		return nil, fmt.Errorf("contact: the URL of %s must start with https:// or unix:///", e.Name)
	}
	c.http = &http.Client{
		Transport:     &authTransport{base: egressrules.LimitResponses(rt, maxResponse), origin: cardOrigin, auth: e.Auth},
		CheckRedirect: egressrules.CheckRedirect,
	}
	return c, nil
}

// unixTransport sends every request through the socket at path, whatever
// the URL's host.
func unixTransport(path string) http.RoundTripper {
	d := &net.Dialer{Timeout: fetchTimeout}
	return &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx, "unix", path) },
		MaxIdleConns:    10,
		IdleConnTimeout: 90 * time.Second,
	}
}

// authTransport adds the Authorization header to the requests over https
// to origin, the scheme, host and port of the card, or to every request
// when origin is "", for a socket that every request goes through.
type authTransport struct {
	base   http.RoundTripper
	origin string
	auth   string
}

func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.auth == "" || (t.origin != "" && origin(r.URL) != t.origin) {
		return t.base.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", t.auth)
	return t.base.RoundTrip(r)
}

// origin is the https origin of u with its port spelled out, or "" for
// another scheme.
func origin(u *url.URL) string {
	if u.Scheme != "https" {
		return ""
	}
	return "https://" + net.JoinHostPort(strings.ToLower(u.Hostname()), cmp.Or(u.Port(), "443"))
}

func (s *Static) loop(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(min(s.retry, s.refresh))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.fetchAll(ctx, func(c *staticContact) bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.reason != "" {
				return time.Since(c.checked) >= s.retry
			}
			return time.Since(c.ok) >= s.refresh
		})
	}
}

// fetchAll fetches the cards of the contacts that due reports at once.
func (s *Static) fetchAll(ctx context.Context, due func(*staticContact) bool) {
	var wg sync.WaitGroup
	for _, c := range s.contacts {
		if due(c) {
			wg.Go(func() { s.fetch(ctx, c) })
		}
	}
	wg.Wait()
}

func (s *Static) fetch(ctx context.Context, c *staticContact) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	card, client, raw, err := c.connect(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checked = time.Now()
	if err != nil {
		c.reason = c.redact(err)
		s.log.Warn("fetching the card of a contact", "contact", c.name, "error", c.reason)
		return
	}
	c.reason, c.ok = "", c.checked
	if c.client != nil && slices.Equal(raw, c.raw) {
		return
	}
	old := c.client
	c.card, c.raw, c.client = card, raw, client
	if old == nil {
		return
	}
	if err := old.Destroy(); err != nil {
		s.log.Debug("closing the client of a contact", "contact", c.name, "error", err)
	}
}

func (c *staticContact) connect(ctx context.Context) (*a2a.AgentCard, *a2aclient.Client, []byte, error) {
	card, err := agentcard.NewResolver(c.http).Resolve(ctx, c.cardURL)
	if err != nil {
		return nil, nil, nil, err
	}
	raw, err := json.Marshal(card)
	if err != nil {
		return nil, nil, nil, err
	}
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(c.http), a2aclient.WithRESTTransport(c.http))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the card has no interface the server can call: %w", err)
	}
	return card, client, raw, nil
}

// redact words a failed fetch without the contact's URL, which may carry
// secrets.
func (c *staticContact) redact(err error) string {
	msg := err.Error()
	if c.display != "" {
		msg = strings.ReplaceAll(msg, strings.TrimSuffix(c.cardURL, "/"), c.display)
	}
	for _, s := range c.secrets {
		msg = strings.ReplaceAll(msg, s, "REDACTED")
	}
	return msg
}

func (s *Static) contact(name string) *staticContact {
	for _, c := range s.contacts {
		if strings.EqualFold(c.name, name) {
			return c
		}
	}
	return nil
}

// Card returns the card of the contact name, and refuses an unknown
// contact or one whose card can't be fetched.
func (s *Static) Card(_ context.Context, _ Turn, name string) (*a2a.AgentCard, error) {
	card, _, err := s.ready(name)
	return card, err
}

// Target returns the client of the contact name.
func (s *Static) Target(_ context.Context, _ Turn, name string) (Target, error) {
	_, client, err := s.ready(name)
	if err != nil {
		return Target{}, err
	}
	return Client(client, nil), nil
}

// ready returns the card and the client of the contact name, or why it
// can't be called.
func (s *Static) ready(name string) (*a2a.AgentCard, *a2aclient.Client, error) {
	c := s.contact(name)
	if c == nil {
		return nil, nil, Refusef("%q is not a contact you can call; your instructions list them", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reason != "" || c.client == nil {
		return nil, nil, Refusef("%s can't be called now: its card couldn't be fetched (%s)", c.name, c.reason)
	}
	return c.card, c.client, nil
}

// Status returns the state of each contact, in the order of the config.
func (s *Static) Status() []Status {
	out := make([]Status, 0, len(s.contacts))
	for _, c := range s.contacts {
		c.mu.Lock()
		st := Status{Name: c.name, Ready: c.reason == "" && c.client != nil, Reason: c.reason, Checked: c.checked}
		if c.card != nil {
			st.Card = shorten(c.card)
		}
		c.mu.Unlock()
		out = append(out, st)
	}
	return out
}

// Close stops fetching the cards and closes the clients.
func (s *Static) Close() error {
	s.stop()
	<-s.done
	var errs []error
	for _, c := range s.contacts {
		c.mu.Lock()
		if c.client != nil {
			errs = append(errs, c.client.Destroy())
		}
		c.client = nil
		c.mu.Unlock()
	}
	return errors.Join(errs...)
}
