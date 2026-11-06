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

package agentsrv

import (
	"context"
	"errors"
	"time"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// localOwnerName names the owner token the server keeps in the credentials
// of the user running it.
const localOwnerName = pairings.LocalOwner

// saveLocalOwnerToken makes sure the user running the server has an owner
// token for its local URL, so `a2a send` from this machine works without
// pairing. Loopback connections aren't trusted instead, because tunnels
// and proxies reach the server from loopback too.
func (s *Server) saveLocalOwnerToken(ctx context.Context) error {
	if err := s.credentials.Prune(); err != nil {
		return err
	}
	local, _, _ := s.URLs()
	saved, ok, err := s.credentials.Lookup(local)
	if err != nil {
		return err
	}
	if ok && saved.Kind != credstore.KindNode && s.ownsSaved(saved) {
		t, err := s.store.LookupToken(ctx, saved.Token)
		if err == nil && t.Pairing == localOwnerName {
			return s.renameSavedOwnerToken(saved)
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	stale, err := s.staleOwnerURLs(ctx, local)
	if err != nil {
		return err
	}
	if err := s.store.DeleteTokens(ctx, localOwnerName); err != nil {
		return err
	}
	token, principal, err := auth.IssueToken(ctx, s.store, localOwnerName, local, "")
	if err != nil {
		return err
	}
	if err := s.credentials.Put(credstore.Credential{URL: local, Token: token, Agent: s.rt.Config().Name, Principal: principal, Created: time.Now().UTC(), Dir: s.dir.Path()}); err != nil {
		return err
	}
	for _, u := range stale {
		if _, err := s.credentials.Delete(u); err != nil {
			return err
		}
	}
	return nil
}

// forgetLocalOwnerToken removes the owner tokens of this agent from the
// credentials, so that no token is offered to whatever listens on its
// local URL after it stops.
func (s *Server) forgetLocalOwnerToken(context.Context) error {
	return s.credentials.DeleteDir(s.dir.Path())
}

// ownsSaved reports whether this agent may have saved c. A copy of the
// agent restored from a backup has the same tokens in its database, so
// entries saved by an agent in another directory belong to that agent.
// Entries without a directory predate it being recorded.
func (s *Server) ownsSaved(c credstore.Credential) bool {
	return c.Dir == "" || c.Dir == s.dir.Path()
}

// renameSavedOwnerToken keeps the name of the agent in the saved credential
// current after the agent was renamed.
func (s *Server) renameSavedOwnerToken(saved credstore.Credential) error {
	name := s.rt.Config().Name
	if saved.Agent == name {
		return nil
	}
	saved.Agent = name
	return s.credentials.Put(saved)
}

// staleOwnerURLs returns the URLs other than local with a saved token of
// the local pairing, which issuing a new token revokes. They are the
// URLs the agent had before its port changed.
func (s *Server) staleOwnerURLs(ctx context.Context, local string) ([]string, error) {
	saved, err := s.credentials.List()
	if err != nil {
		return nil, err
	}
	var urls []string
	for _, c := range saved {
		if c.Kind == credstore.KindNode || c.URL == credstore.Normalize(local) || !s.ownsSaved(c) {
			continue
		}
		t, err := s.store.LookupToken(ctx, c.Token)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if t.Pairing == localOwnerName {
			urls = append(urls, c.URL)
		}
	}
	return urls, nil
}
