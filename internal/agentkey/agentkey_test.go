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

package agentkey

import (
	"testing"

	"github.com/a2aproject/a2a-serve/internal/auth"
)

func TestOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		p    auth.Principal
		want string
	}{
		{name: "owner", p: auth.Owner(), want: ""},
		{name: "guest", p: auth.Principal{ID: auth.GuestID, Role: auth.RoleGuest}, want: Guest},
		{name: "member", p: auth.Principal{ID: "pair:anna", Role: auth.RoleMember}, want: "user:pair:anna"},
		{
			name: "caller of an agent", want: Caller("pair:agent1", "m13"),
			p: auth.Principal{ID: "pair:agent1", Role: auth.RoleMember, Identity: auth.Identity{Pairing: "agent1", Hops: 1, Caller: "m13"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Of(tc.p, true); got != tc.want {
				t.Fatalf("Of() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCallerKeepsCallersApart(t *testing.T) {
	t.Parallel()
	m13, m03 := Caller("pair:agent1", "m13"), Caller("pair:agent1", "m03")
	if m13 == m03 || m13 == User("pair:agent1") {
		t.Fatalf("Caller() = %q and %q, want keys of their own beside %q", m13, m03, User("pair:agent1"))
	}
	if again := Caller("pair:agent1", "m13"); again != m13 {
		t.Fatalf("Caller() again = %q, want %q", again, m13)
	}
}

func TestUserOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		key        string
		wantID     string
		wantCaller bool
		wantOK     bool
	}{
		{name: "member", key: User("pair:anna"), wantID: "pair:anna", wantOK: true},
		{name: "caller of an agent", key: Caller("pair:agent1", "m13"), wantID: "pair:agent1", wantCaller: true, wantOK: true},
		{name: "email with a hash", key: User("email:a#0123456789abcdef@example.com"), wantID: "email:a#0123456789abcdef@example.com", wantOK: true},
		{name: "guest", key: Guest},
		{name: "owner", key: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id, ok := UserOf(tc.key)
			if id != tc.wantID || ok != tc.wantOK {
				t.Fatalf("UserOf(%q) = %q, %v, want %q, %v", tc.key, id, ok, tc.wantID, tc.wantOK)
			}
			if got := CallerOf(tc.key) != ""; got != tc.wantCaller {
				t.Fatalf("CallerOf(%q) != \"\" = %v, want %v", tc.key, got, tc.wantCaller)
			}
		})
	}
}
