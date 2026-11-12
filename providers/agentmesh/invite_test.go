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

package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/sdk/meshnet"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
)

func recordInvite(t *testing.T, dir, token string) {
	t.Helper()
	sw, err := useInvite(dir, token)
	if err != nil {
		t.Fatalf("useInvite(%q) error = %v", token, err)
	}
	if err := sw.record(); err != nil {
		t.Fatalf("inviteSwitch.record() error = %v", err)
	}
}

func TestExplainJoin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		err         error
		wantProblem string
	}{
		{name: "truncated invite", err: errors.New("enrollment failed: Invalid bootstrap token"), wantProblem: "the relay doesn't know this invite; check that it was copied whole"},
		{name: "used invite", err: errors.New("enrollment failed: Bootstrap token max usages exceeded"), wantProblem: "the invite was already used"},
		{name: "expired invite", err: errors.New("enrollment failed: Bootstrap token expired"), wantProblem: "the invite expired"},
		{name: "revoked invite", err: errors.New("enrollment failed: Bootstrap token revoked"), wantProblem: "the invite was revoked"},
		{name: "spent invite", err: errors.New("enrollment failed: Bootstrap token expired, revoked or exhausted"), wantProblem: "the invite expired, was revoked or was already used"},
		{name: "invite of a removed member", err: errors.New("enrollment failed: Bootstrap token owner is banned"), wantProblem: "the relay removed the member who created the invite"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := explainJoin(tc.err)
			var invite *inviteError
			if !errors.As(err, &invite) {
				t.Fatalf("explainJoin() error = %v, want an inviteError", err)
			}
			if invite.Problem != tc.wantProblem {
				t.Fatalf("explainJoin() problem = %q, want %q", invite.Problem, tc.wantProblem)
			}
		})
	}
}

func TestExplainJoinKeepsOtherErrors(t *testing.T) {
	t.Parallel()
	other := errors.New("dial tcp: connection refused")
	if err := explainJoin(other); err != other {
		t.Fatalf("explainJoin() error = %v, want %v", err, other)
	}
}

func TestUseInvite(t *testing.T) {
	t.Parallel()
	enrolled := &api.MemberCredential{ControlPlaneUrl: "https://relay.example.com", Biscuit: []byte("biscuit")}
	setAside := &api.MemberCredential{ControlPlaneUrl: "https://relay.example.com"}
	tests := []struct {
		name       string
		lastToken  string
		token      string
		wantSwitch bool
		wantCred   *api.MemberCredential
	}{
		{name: "unrecorded token sets the credential aside", token: "invite-1", wantSwitch: true, wantCred: setAside},
		{name: "same token keeps the credential", lastToken: "invite-1", token: "invite-1", wantCred: enrolled},
		{name: "new token sets the credential aside", lastToken: "invite-1", token: "invite-2", wantSwitch: true, wantCred: setAside},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			key := marshalLibp2pEd25519Key(t)
			if tc.lastToken != "" {
				recordInvite(t, dir, tc.lastToken)
			}
			if err := meshnet.WriteStateDir(dir, key, enrolled); err != nil {
				t.Fatalf("meshnet.WriteStateDir() error = %v", err)
			}

			sw, err := useInvite(dir, tc.token)
			if err != nil {
				t.Fatalf("useInvite(%q) error = %v", tc.token, err)
			}

			if got := sw != nil; got != tc.wantSwitch {
				t.Fatalf("useInvite(%q) switches = %v, want %v", tc.token, got, tc.wantSwitch)
			}
			if sw != nil && sw.recorded != (tc.lastToken != "") {
				t.Fatalf("useInvite(%q) recorded = %v, want %v", tc.token, sw.recorded, tc.lastToken != "")
			}
			gotKey, gotCred, err := meshnet.ReadStateDir(dir)
			if err != nil {
				t.Fatalf("meshnet.ReadStateDir() error = %v", err)
			}
			if diff := cmp.Diff(key, gotKey); diff != "" {
				t.Fatalf("meshnet.ReadStateDir() identity key wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantCred, gotCred, protocmp.Transform()); diff != "" {
				t.Fatalf("meshnet.ReadStateDir() credential wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestUseInviteWithoutState(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "relay")
	for _, token := range []string{"invite-1", "invite-2"} {
		recordInvite(t, dir, token)
	}
	if hasMeshIdentity(dir) {
		t.Fatalf("hasMeshIdentity() = true after invites without a join, want false")
	}
	if restored, err := restoreMeshCredential(dir); err != nil || restored {
		t.Fatalf("restoreMeshCredential() = %v, %v, want false, nil", restored, err)
	}
}

func TestRestoreMeshCredential(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	enrolled := &api.MemberCredential{ControlPlaneUrl: "https://relay.example.com", Biscuit: []byte("biscuit")}
	if err := meshnet.WriteStateDir(dir, marshalLibp2pEd25519Key(t), enrolled); err != nil {
		t.Fatalf("meshnet.WriteStateDir() error = %v", err)
	}
	for _, token := range []string{"invite-1", "invite-2"} {
		if _, err := useInvite(dir, token); err != nil {
			t.Fatalf("useInvite(%q) error = %v", token, err)
		}
	}

	restored, err := restoreMeshCredential(dir)
	if err != nil || !restored {
		t.Fatalf("restoreMeshCredential() = %v, %v, want true, nil", restored, err)
	}
	_, got, err := meshnet.ReadStateDir(dir)
	if err != nil {
		t.Fatalf("meshnet.ReadStateDir() error = %v", err)
	}
	if diff := cmp.Diff(enrolled, got, protocmp.Transform()); diff != "" {
		t.Fatalf("meshnet.ReadStateDir() credential wrong result (-want +got) diff = %s", diff)
	}
	if restored, err := restoreMeshCredential(dir); err != nil || restored {
		t.Fatalf("restoreMeshCredential() again = %v, %v, want false, nil", restored, err)
	}
}
