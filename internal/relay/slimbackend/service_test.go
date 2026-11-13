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
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
)

// clock is a settable time for tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestService(t *testing.T, c *clock) *service {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "members.json"), c.now)
	if err != nil {
		t.Fatal(err)
	}
	return &service{
		store: st, conns: newConns(c.now), now: c.now,
		status: func() control.Status {
			return control.Status{URL: testRelay, Versions: control.Versions{Backend: Name}}
		},
	}
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// join invites and enrolls the agent named name.
func join(t *testing.T, s *service, name string) ed25519.PrivateKey {
	t.Helper()
	inv, err := s.Invite(t.Context(), control.InviteRequest{Name: name})
	if err != nil {
		t.Fatalf("service.Invite() error = %v", err)
	}
	pub, priv := newKey(t)
	if _, err := s.store.enroll(inv.Token, pub); err != nil {
		t.Fatalf("store.enroll() error = %v", err)
	}
	return priv
}

func TestInviteAndEnroll(t *testing.T) {
	t.Parallel()
	c := &clock{testNow}
	s := newTestService(t, c)

	inv, err := s.Invite(t.Context(), control.InviteRequest{Name: "coder", Hours: 2})
	if err != nil {
		t.Fatalf("service.Invite() error = %v", err)
	}
	if !strings.HasPrefix(inv.Token, invitePrefix) || inv.URL != testRelay || !inv.Expires.Equal(testNow.Add(2*time.Hour)) {
		t.Fatalf("service.Invite() = %+v, want a %s token for %s that expires in 2 hours", inv, invitePrefix, testRelay)
	}
	pub, _ := newKey(t)
	c.t = testNow.Add(time.Minute)
	m, err := s.store.enroll(inv.Token, pub)
	if err != nil {
		t.Fatalf("store.enroll() error = %v", err)
	}
	again, err := s.store.enroll(inv.Token, pub)
	if err != nil {
		t.Fatalf("store.enroll() again with the same key error = %v", err)
	}
	if diff := cmp.Diff(m, again); diff != "" {
		t.Fatalf("store.enroll() again wrong result (-want +got) diff = %s", diff)
	}
	other, _ := newKey(t)
	if _, err := s.store.enroll(inv.Token, other); !errors.Is(err, errInvite) {
		t.Fatalf("store.enroll() with a used invite and another key error = %v, want %v", err, errInvite)
	}

	members, err := s.Members(t.Context(), false)
	if err != nil {
		t.Fatalf("service.Members() error = %v", err)
	}

	want := []control.Member{{Agent: "coder", Invite: true, InviteName: "coder", InviteID: inv.ID, Joined: testNow.Add(time.Minute), Peer: "a2a/relay/coder"}}
	if diff := cmp.Diff(want, members); diff != "" {
		t.Fatalf("service.Members() wrong result (-want +got) diff = %s", diff)
	}
}

func TestEnrollRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(t *testing.T, s *service, c *clock) string
		want    func(error) bool
	}{
		{
			name:    "unknown token",
			prepare: func(*testing.T, *service, *clock) string { return invitePrefix + "unknown" },
			want:    func(err error) bool { return errors.Is(err, errInvite) },
		},
		{
			name: "expired invite",
			prepare: func(t *testing.T, s *service, c *clock) string {
				inv, err := s.Invite(t.Context(), control.InviteRequest{Name: "coder", Hours: 1})
				if err != nil {
					t.Fatal(err)
				}
				c.t = testNow.Add(time.Hour)
				return inv.Token
			},
			want: func(err error) bool { return errors.Is(err, errInvite) },
		},
		{
			name: "name taken since the invite",
			prepare: func(t *testing.T, s *service, _ *clock) string {
				inv, err := s.Invite(t.Context(), control.InviteRequest{Name: "coder"})
				if err != nil {
					t.Fatal(err)
				}
				join(t, s, "coder")
				return inv.Token
			},
			want: func(err error) bool {
				var c *agenterr.ConflictError
				return errors.As(err, &c)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &clock{testNow}
			s := newTestService(t, c)
			token := tt.prepare(t, s, c)
			pub, _ := newKey(t)

			_, err := s.store.enroll(token, pub)

			if !tt.want(err) {
				t.Fatalf("store.enroll() error = %v, want a refusal", err)
			}
		})
	}
}

func TestInviteRefusesTheNameOfAMember(t *testing.T) {
	t.Parallel()
	s := newTestService(t, &clock{testNow})
	join(t, s, "coder")

	_, err := s.Invite(t.Context(), control.InviteRequest{Name: "coder"})

	var c *agenterr.ConflictError
	if !errors.As(err, &c) {
		t.Fatalf("service.Invite() of a member's name error = %v, want a conflict", err)
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()
	c := &clock{testNow}
	s := newTestService(t, c)
	join(t, s, "coder")
	join(t, s, "reviewer")
	unused, err := s.Invite(t.Context(), control.InviteRequest{Name: "ci-bot"})
	if err != nil {
		t.Fatal(err)
	}
	c.t = testNow.Add(time.Hour)

	if _, err := s.Remove(t.Context(), control.RemoveRequest{Target: "reviewer"}); err != nil {
		t.Fatalf("service.Remove() of an agent error = %v", err)
	}
	if _, err := s.Remove(t.Context(), control.RemoveRequest{Target: unused.ID}); err != nil {
		t.Fatalf("service.Remove() of an invite by ID error = %v", err)
	}
	_, err = s.Remove(t.Context(), control.RemoveRequest{Target: "reviewer"})
	var nf *agenterr.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("service.Remove() of a removed agent error = %v, want not found", err)
	}

	all, err := s.Members(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range all {
		got = append(got, m.Agent+" "+map[bool]string{true: "removed", false: "member"}[m.Banned])
	}
	if diff := cmp.Diff([]string{"coder member", "reviewer removed"}, got); diff != "" {
		t.Fatalf("service.Members(banned) wrong result (-want +got) diff = %s", diff)
	}
	st, err := s.Status(t.Context())
	if err != nil || st.Members != 1 {
		t.Fatalf("service.Status() = %+v, %v, want 1 member", st, err)
	}
	join(t, s, "reviewer")
}

func TestStoreKeepsItsStateAcrossRestarts(t *testing.T) {
	t.Parallel()
	c := &clock{testNow}
	path := filepath.Join(t.TempDir(), "members.json")
	st, err := openStore(path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	s := &service{store: st, conns: newConns(c.now), now: c.now, status: func() control.Status { return control.Status{} }}
	join(t, s, "coder")

	reopened, err := openStore(path, c.now)
	if err != nil {
		t.Fatalf("openStore() error = %v", err)
	}

	if diff := cmp.Diff(st.snapshot(), reopened.snapshot()); diff != "" {
		t.Fatalf("openStore() wrong state (-want +got) diff = %s", diff)
	}
}
