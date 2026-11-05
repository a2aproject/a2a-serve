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

// Package credstore keeps the pairing tokens `a2a-serve pair join` obtained:
// caller tokens, which `a2a-serve credential get` gives the a2a CLI for
// requests to the same agent, and node tokens, which the `a2a-serve mailbox`
// commands use.
package credstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
)

// KindNode marks the token of a webhook node; other credentials are caller tokens.
const KindNode = pairapi.KindNode

// Credential is a token for one agent URL, or for one webhook node.
type Credential struct {
	URL       string    `json:"url"`
	Kind      string    `json:"kind,omitempty"`
	Token     string    `json:"token"`
	Agent     string    `json:"agent,omitempty"`
	Principal string    `json:"principal,omitempty"`
	Created   time.Time `json:"created"`
}

// Store is a credentials file, readable only by its owner.
type Store struct {
	Path string
}

// Default returns the store at ~/.config/a2a-serve/credentials.json.
func Default() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home directory: %w", err)
	}
	return At(home), nil
}

// At returns the store under the home directory home.
func At(home string) *Store {
	return &Store{Path: filepath.Join(home, ".config", "a2a-serve", "credentials.json")}
}

// List returns the stored credentials.
func (s *Store) List() ([]Credential, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Credential
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", s.Path, err)
	}
	return out, nil
}

func (s *Store) save(creds []Credential) error {
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(s.Path, data, 0o600)
}

// update applies fn to the stored credentials under a lock, so that
// concurrent commands don't lose each other's changes.
func (s *Store) update(fn func([]Credential) ([]Credential, bool)) error {
	return fsutil.WithLock(s.Path, func() error {
		creds, err := s.List()
		if err != nil {
			return err
		}
		next, changed := fn(creds)
		if !changed {
			return nil
		}
		return s.save(next)
	})
}

// Put stores a credential, replacing one of the same kind for the same URL.
func (s *Store) Put(c Credential) error {
	c.URL = Normalize(c.URL)
	return s.update(func(creds []Credential) ([]Credential, bool) {
		creds = slices.DeleteFunc(creds, func(x Credential) bool { return x.URL == c.URL && x.Kind == c.Kind })
		return append(creds, c), true
	})
}

// Delete removes the credentials for url. It reports whether one existed.
func (s *Store) Delete(u string) (bool, error) {
	found := false
	err := s.update(func(creds []Credential) ([]Credential, bool) {
		n := len(creds)
		creds = slices.DeleteFunc(creds, func(x Credential) bool { return x.URL == Normalize(u) })
		found = len(creds) != n
		return creds, found
	})
	return found, err
}

// Has reports whether [Store.Delete] would remove a credential for url.
func (s *Store) Has(u string) (bool, error) {
	creds, err := s.List()
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(creds, func(x Credential) bool { return x.URL == Normalize(u) }), nil
}

// Lookup returns the caller credential whose URL is target or the longest
// prefix of it, so a token for an agent also covers its card and endpoints.
// Without one, it returns a node token of the agent at target, so a webhook
// node can call the agent hosting it.
func (s *Store) Lookup(target string) (Credential, bool, error) {
	creds, err := s.List()
	if err != nil {
		return Credential{}, false, err
	}
	t := Normalize(target)
	var best, node Credential
	for _, c := range creds {
		if c.Kind == KindNode {
			if nc, ok := NodeCredential(c.Token, t); ok && node.URL == "" {
				node = nc
			}
			continue
		}
		if covers(c.URL, t) && len(c.URL) > len(best.URL) {
			best = c
		}
	}
	if best.URL == "" {
		best = node
	}
	return best, best.URL != "", nil
}

// NodeCredential returns a credential for token if it is the node token of
// a node hosted by the agent at target.
func NodeCredential(token, target string) (Credential, bool) {
	agent, ok := mailboxapi.AgentURL(token)
	if !ok {
		return Credential{}, false
	}
	agent = Normalize(agent)
	if !covers(agent, Normalize(target)) {
		return Credential{}, false
	}
	return Credential{URL: agent, Kind: KindNode, Token: token}, true
}

// covers reports whether the normalized target is base or under it.
func covers(base, target string) bool {
	return target == base || strings.HasPrefix(target, base+"/")
}

// Node returns the only saved node token. A machine hosting several nodes
// has to name the token explicitly.
func (s *Store) Node() (Credential, error) {
	creds, err := s.List()
	if err != nil {
		return Credential{}, err
	}
	creds = slices.DeleteFunc(creds, func(c Credential) bool { return c.Kind != KindNode })
	switch len(creds) {
	case 0:
		return Credential{}, ErrNoNode
	case 1:
		return creds[0], nil
	default:
		return Credential{}, fmt.Errorf("%d node tokens saved in %s; pass the one to use with --token", len(creds), s.Path)
	}
}

// ErrNoNode is returned by [Store.Node] when no node token is saved.
var ErrNoNode = errors.New("no node token saved")

// Normalize drops the query, the trailing slash and the well-known card
// path, resolves dot segments, and lowercases the scheme and host.
func Normalize(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSuffix(raw, "/")
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	u.RawQuery, u.Fragment, u.User = "", "", nil
	if u.Path != "" {
		u.Path = path.Clean("/" + u.Path)
	}
	u.Path = strings.TrimSuffix(u.Path, "/.well-known/agent-card.json")
	u.Path = strings.TrimSuffix(u.Path, "/.well-known/agent.json")
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	return u.String()
}

// Pair exchanges a pairing code at agentURL for a credential, as a
// redeemer of kind. A node token must be for a node of the agent at
// agentURL, so that the agent can't have it stored for another one.
func Pair(ctx context.Context, hc *http.Client, agentURL, code, kind string) (Credential, error) {
	base := Normalize(agentURL)
	pr, err := pairapi.Pair(ctx, hc, base, code, kind)
	if err != nil {
		return Credential{}, err
	}
	c := Credential{URL: base, Token: pr.Token, Agent: pr.Agent, Principal: pr.Principal, Created: time.Now().UTC()}
	switch pr.Kind {
	case "", pairapi.KindCaller:
		return c, nil
	case KindNode:
		if err := checkNodeToken(base, pr); err != nil {
			return Credential{}, fmt.Errorf("pairing with %s: %w", base, err)
		}
		c.Kind, c.URL = KindNode, Normalize(pr.URL)
		return c, nil
	}
	return Credential{}, fmt.Errorf("pairing with %s: unknown token kind %q", base, pr.Kind)
}

// checkNodeToken checks that the node token and URL of pr belong to one
// node of the agent at base.
func checkNodeToken(base string, pr pairapi.Response) error {
	apiBase, id, ok := mailboxapi.ParseNodeToken(pr.Token)
	if !ok {
		return errors.New("the agent returned a malformed node token")
	}
	agent := Normalize(strings.TrimSuffix(apiBase, mailboxapi.Prefix))
	if !sameAgent(agent, base) {
		return fmt.Errorf("the agent returned a node token for %s", agent)
	}
	if Normalize(pr.URL) != Normalize(mailboxapi.NodeURL(agent, id)) {
		return fmt.Errorf("the agent returned the node URL %s for node %s", pr.URL, id)
	}
	return nil
}

// sameAgent reports whether the normalized URLs a and b are the same,
// taking every loopback host as one, as an agent's local URL names it by IP.
func sameAgent(a, b string) bool {
	if a == b {
		return true
	}
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil || !isLoopback(ua.Hostname()) || !isLoopback(ub.Hostname()) {
		return false
	}
	return ua.Scheme == ub.Scheme && ua.Port() == ub.Port() && ua.Path == ub.Path
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HelperCommand is the a2a CLI credential helper that reads this store.
const HelperCommand = "a2a-serve credential"

// HelperEntry is the credential-helper entry of the a2a CLI config which
// runs HelperCommand.
const HelperEntry = "{command: [a2a-serve, credential]}"

// HelperSetup tells how to have the a2a CLI attach the stored tokens.
const HelperSetup = "for the a2a CLI to send it, run a2a-serve doctor --fix once"
