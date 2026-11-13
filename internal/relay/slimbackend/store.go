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

package slimbackend

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// Bounds of the store. Invites expire within a week, so unused ones go away
// on their own; removed members are kept for `relay members --banned` until
// there are too many.
const (
	maxUnusedInvites  = 1000
	maxRemovedMembers = 1000
)

// invite is an invite no agent joined with yet. The relay keeps the SHA-256
// of its token.
type invite struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
}

// member is an agent that joined the relay, with the key it signs its
// tokens with.
type member struct {
	Agent string `json:"agent"`
	// Key is the agent's Ed25519 public key.
	Key        []byte    `json:"key"`
	Joined     time.Time `json:"joined"`
	InviteID   string    `json:"inviteId"`
	InviteName string    `json:"inviteName"`
	// InviteHash is the hash of the token the agent joined with, so that
	// an agent that retries its enrollment gets the same answer.
	InviteHash string    `json:"inviteHash"`
	Removed    time.Time `json:"removed,omitzero"`
}

func (m member) removed() bool { return !m.Removed.IsZero() }

type state struct {
	Invites []invite `json:"invites"`
	Members []member `json:"members"`
}

// store keeps the relay's invites and members in a file. Every change is
// written before it returns.
type store struct {
	path string
	now  func() time.Time
	mu   sync.Mutex
	st   state
}

func openStore(path string, now func() time.Time) (*store, error) {
	s := &store{path: path, now: now}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.st); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return s, nil
}

// update applies fn to a copy of the state and saves it, unless fn fails.
func (s *store) update(fn func(st *state) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := state{Invites: slices.Clone(s.st.Invites), Members: slices.Clone(s.st.Members)}
	if err := fn(&st); err != nil {
		return err
	}
	now := s.now()
	st.Invites = slices.DeleteFunc(st.Invites, func(i invite) bool { return !now.Before(i.Expires) })
	if n := countFunc(st.Members, member.removed); n > maxRemovedMembers {
		st.Members = dropOldestRemoved(st.Members, n-maxRemovedMembers)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(s.path, data, 0o600); err != nil {
		return err
	}
	s.st = st
	return nil
}

func (s *store) snapshot() state {
	s.mu.Lock()
	defer s.mu.Unlock()
	return state{Invites: slices.Clone(s.st.Invites), Members: slices.Clone(s.st.Members)}
}

// active returns the member named agent that wasn't removed.
func (st *state) active(agent string) (int, bool) {
	i := slices.IndexFunc(st.Members, func(m member) bool { return m.Agent == agent && !m.removed() })
	return i, i >= 0
}

// addInvite stores an invite.
func (s *store) addInvite(inv invite) error {
	return s.update(func(st *state) error {
		if len(st.Invites) >= maxUnusedInvites {
			return agenterr.Conflictf("the relay has %d unused invites; remove some with `a2a-serve relay remove <name>`", len(st.Invites))
		}
		st.Invites = append(st.Invites, inv)
		return nil
	})
}

// errInvite is returned for a token that joins no agent.
var errInvite = errors.New("the invite is unknown, used or expired; ask the relay's admin for a new one with `a2a-serve relay invite <name>`")

// enroll spends the invite of token on key, and returns the member it adds.
// An agent that retries with the same token and key gets its membership
// again.
func (s *store) enroll(token string, key ed25519.PublicKey) (member, error) {
	h := hashToken(token)
	var added member
	err := s.update(func(st *state) error {
		for _, m := range st.Members {
			if !m.removed() && subtle.ConstantTimeCompare([]byte(m.InviteHash), []byte(h)) == 1 {
				if !slices.Equal(m.Key, key) {
					return errInvite
				}
				added = m
				return nil
			}
		}
		i := slices.IndexFunc(st.Invites, func(inv invite) bool {
			return subtle.ConstantTimeCompare([]byte(inv.Hash), []byte(h)) == 1
		})
		if i < 0 || !s.now().Before(st.Invites[i].Expires) {
			return errInvite
		}
		inv := st.Invites[i]
		if _, ok := st.active(inv.Name); ok {
			return agenterr.Conflictf("an agent named %s is on the relay already; ask the relay's admin to remove it, or for an invite with another name", inv.Name)
		}
		added = member{Agent: inv.Name, Key: slices.Clone(key), Joined: s.now(), InviteID: inv.ID, InviteName: inv.Name, InviteHash: h}
		st.Invites = slices.Delete(st.Invites, i, i+1)
		st.Members = append(st.Members, added)
		return nil
	})
	return added, err
}

// remove removes the active members named target, and revokes the unused
// invites with that name or ID. It returns both.
func (s *store) remove(target string) (members []member, invites []invite, err error) {
	err = s.update(func(st *state) error {
		now := s.now()
		for i, m := range st.Members {
			if !m.removed() && (m.Agent == target || SLIMName(m.Agent) == target) {
				st.Members[i].Removed = now
				members = append(members, st.Members[i])
			}
		}
		st.Invites = slices.DeleteFunc(st.Invites, func(inv invite) bool {
			if inv.Name != target && inv.ID != target {
				return false
			}
			invites = append(invites, inv)
			return true
		})
		if len(members) == 0 && len(invites) == 0 {
			return &agenterr.NotFoundError{Kind: "member", Name: target}
		}
		return nil
	})
	return members, invites, err
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func countFunc[T any](s []T, f func(T) bool) int {
	n := 0
	for _, v := range s {
		if f(v) {
			n++
		}
	}
	return n
}

// dropOldestRemoved drops the n removed members that were removed first.
func dropOldestRemoved(ms []member, n int) []member {
	removed := slices.DeleteFunc(slices.Clone(ms), func(m member) bool { return !m.removed() })
	slices.SortFunc(removed, func(a, b member) int { return a.Removed.Compare(b.Removed) })
	cutoff := removed[n-1].Removed
	dropped := 0
	return slices.DeleteFunc(ms, func(m member) bool {
		if dropped < n && m.removed() && !m.Removed.After(cutoff) {
			dropped++
			return true
		}
		return false
	})
}
