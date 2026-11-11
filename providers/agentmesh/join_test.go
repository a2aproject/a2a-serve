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
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/sdk/meshnet"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/a2aproject/a2a-serve/netprovider"
)

func marshalLibp2pEd25519Key(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, 1)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendBytes(b, priv)
	return b
}

func TestHasMeshIdentity(t *testing.T) {
	t.Parallel()
	privBytes := marshalLibp2pEd25519Key(t)

	interruptedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(interruptedDir, "agent.db"), []byte("bbolt"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	emptyBiscuitDir := t.TempDir()
	if err := meshnet.WriteStateDir(emptyBiscuitDir, privBytes, &api.MemberCredential{ControlPlaneUrl: "https://relay.example.com"}); err != nil {
		t.Fatalf("meshnet.WriteStateDir() error = %v", err)
	}

	enrolledDir := t.TempDir()
	if err := meshnet.WriteStateDir(enrolledDir, privBytes, &api.MemberCredential{
		ControlPlaneUrl: "https://relay.example.com",
		Biscuit:         []byte("enrolled-biscuit"),
	}); err != nil {
		t.Fatalf("meshnet.WriteStateDir() error = %v", err)
	}

	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{name: "empty dir", dir: t.TempDir(), want: false},
		{name: "interrupted first run with agent.db only", dir: interruptedDir, want: false},
		{name: "saved key without biscuit", dir: emptyBiscuitDir, want: false},
		{name: "enrolled credential with biscuit", dir: enrolledDir, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hasMeshIdentity(tc.dir); got != tc.want {
				t.Fatalf("hasMeshIdentity(%q) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
}

func TestSavedPeerID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := meshnet.WriteStateDir(dir, marshalLibp2pEd25519Key(t), &api.MemberCredential{ControlPlaneUrl: "https://relay.example.com", Biscuit: []byte("biscuit")}); err != nil {
		t.Fatalf("meshnet.WriteStateDir() error = %v", err)
	}

	if got := savedPeerID(dir); !strings.HasPrefix(got, "12D3KooW") {
		t.Fatalf("savedPeerID() = %q, want an Ed25519 peer ID", got)
	}
	if got := savedPeerID(t.TempDir()); got != "" {
		t.Fatalf("savedPeerID() of an empty directory = %q, want none", got)
	}
}

// fakeMeshRelay joins agents like meshnet: a join resumes the credential in
// the state directory, and enrolls when there is none.
type fakeMeshRelay struct {
	stateDir string
	// revoked is the credential routers refuse.
	revoked []byte
	// enrolled is the credential an enrollment saves, or nil when the relay
	// refuses it.
	enrolled []byte
	// enrollErr fails an enrollment.
	enrollErr error
	// joinErr fails a join after it enrolled.
	joinErr error
	joins   atomic.Int32
}

func (r *fakeMeshRelay) serve(*meshnet.Server) meshServer { return &fakeJoin{relay: r} }

type fakeJoin struct {
	fakeMeshSession
	relay *fakeMeshRelay
}

func (j *fakeJoin) Up(context.Context) (*meshnet.Status, error) {
	r := j.relay
	r.joins.Add(1)
	key, cred, err := meshnet.ReadStateDir(r.stateDir)
	if err != nil {
		return nil, err
	}
	switch {
	case len(cred.GetBiscuit()) > 0 && string(cred.GetBiscuit()) == string(r.revoked):
		return nil, errors.New("failed to authenticate with any router: all connection attempts failed")
	case len(cred.GetBiscuit()) > 0:
		return &meshnet.Status{PeerID: savedPeerID(r.stateDir)}, nil
	case r.enrollErr != nil:
		return nil, r.enrollErr
	case r.enrolled == nil:
		return nil, errors.New("the relay refused the sign-in")
	}
	if err := meshnet.WriteStateDir(r.stateDir, key, &api.MemberCredential{ControlPlaneUrl: cred.GetControlPlaneUrl(), Biscuit: r.enrolled}); err != nil {
		return nil, err
	}
	return &meshnet.Status{PeerID: savedPeerID(r.stateDir)}, r.joinErr
}

func closeMesh(t *testing.T, a *agentMesh) {
	t.Helper()
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("agentMesh.Close() error = %v", err)
		}
	})
}

func TestSignedInAgentSignsInAgainWhenRoutersRefuseItsCredential(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		token       string
		saved       string
		enrolled    string
		joinErr     error
		wantErr     bool
		wantCred    string
		wantJoins   int32
		wantNotices []string
	}{
		{name: "refused credential is replaced", saved: "revoked", enrolled: "new", wantCred: "new", wantJoins: 2, wantNotices: []string{"! relay: no router admitted the agent's saved credential; signing in again"}},
		{name: "refused sign-in keeps the saved credential", saved: "revoked", wantErr: true, wantCred: "revoked", wantJoins: 2, wantNotices: []string{"! relay: no router admitted the agent's saved credential; signing in again"}},
		{name: "failed join after the sign-in keeps the new credential", saved: "revoked", enrolled: "new", joinErr: errors.New("the relay is restarting"), wantErr: true, wantCred: "new", wantJoins: 2, wantNotices: []string{"! relay: no router admitted the agent's saved credential; signing in again"}},
		{name: "admitted credential is kept", saved: "valid", enrolled: "new", wantCred: "valid", wantJoins: 1},
		{name: "invited agent doesn't sign in", token: "invite", saved: "revoked", enrolled: "new", wantErr: true, wantCred: "revoked", wantJoins: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			relay := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(relay.Close)
			dir := t.TempDir()
			if tc.token != "" {
				recordInvite(t, dir, tc.token)
			}
			if err := meshnet.WriteStateDir(dir, marshalLibp2pEd25519Key(t), &api.MemberCredential{ControlPlaneUrl: relay.URL, Biscuit: []byte(tc.saved)}); err != nil {
				t.Fatalf("meshnet.WriteStateDir() error = %v", err)
			}
			fake := &fakeMeshRelay{stateDir: dir, revoked: []byte("revoked"), joinErr: tc.joinErr}
			if tc.enrolled != "" {
				fake.enrolled = []byte(tc.enrolled)
			}
			h := &fakeHost{}
			a := newAgentMesh(h, fake.serve)
			closeMesh(t, a)

			_, err := a.Join(t.Context(), netprovider.JoinRequest{Agent: "coder", URL: relay.URL, StateDir: dir, Token: tc.token, Private: true})

			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("agentMesh.Join() error = %v, want error: %v", err, tc.wantErr)
			}
			if got := fake.joins.Load(); got != tc.wantJoins {
				t.Fatalf("agentMesh.Join() joins = %d, want %d", got, tc.wantJoins)
			}
			_, cred, err := meshnet.ReadStateDir(dir)
			if err != nil {
				t.Fatalf("meshnet.ReadStateDir() error = %v", err)
			}
			if got := string(cred.GetBiscuit()); got != tc.wantCred {
				t.Fatalf("meshnet.ReadStateDir() credential = %q, want %q", got, tc.wantCred)
			}
			if _, err := os.Stat(filepath.Join(dir, setAsideCredentialFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("os.Stat(%s) error = %v, want the set-aside credential removed", setAsideCredentialFile, err)
			}
			if diff := cmp.Diff(tc.wantNotices, h.noticeLines()); diff != "" {
				t.Fatalf("agentMesh.Join() notices wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSignedInAgentKeepsItsCredentialWhenSigningInAgainFails(t *testing.T) {
	t.Parallel()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(relay.Close)
	dir := t.TempDir()
	enrolled := &api.MemberCredential{ControlPlaneUrl: relay.URL, Biscuit: []byte("biscuit")}
	if err := meshnet.WriteStateDir(dir, marshalLibp2pEd25519Key(t), enrolled); err != nil {
		t.Fatalf("meshnet.WriteStateDir() error = %v", err)
	}
	// A start that stopped while signing in again.
	if err := setAsideMeshCredential(dir); err != nil {
		t.Fatalf("setAsideMeshCredential() error = %v", err)
	}
	a := newAgentMesh(&fakeHost{}, func(srv *meshnet.Server) meshServer { return srv })
	closeMesh(t, a)

	if _, err := a.Join(t.Context(), netprovider.JoinRequest{Agent: "coder", URL: relay.URL, StateDir: dir, Private: true}); err == nil {
		t.Fatal("agentMesh.Join() error = nil, want the unavailable relay")
	}

	_, got, err := meshnet.ReadStateDir(dir)
	if err != nil {
		t.Fatalf("meshnet.ReadStateDir() error = %v", err)
	}
	if diff := cmp.Diff(enrolled, got, protocmp.Transform()); diff != "" {
		t.Fatalf("meshnet.ReadStateDir() credential wrong result (-want +got) diff = %s", diff)
	}
}

func TestJoin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		token string
		// banned bans the agent's saved peer ID at the relay.
		banned    bool
		saved     string
		enrollErr error
		owner     string
		want      netprovider.JoinResult
		wantErr   *netprovider.Error
		// wantPlainErr starts the text of an error without a kind.
		wantPlainErr string
	}{
		{name: "signed-in agent says who owns it", saved: "valid", owner: "ben@example.com", want: netprovider.JoinResult{Owner: "ben@example.com"}},
		{name: "invited agent says no owner", token: "invite", saved: "valid", owner: "ben@example.com"},
		{
			name:      "used invite",
			token:     "invite",
			enrollErr: errors.New("enrollment failed: Bootstrap token max usages exceeded"),
			wantErr:   &netprovider.Error{Kind: netprovider.KindToken, Problem: "the invite was already used", Fix: "ask the relay's admin for a new invite (`a2a-serve relay invite coder`)"},
		},
		{
			name:      "invited agent the relay removed",
			token:     "invite",
			banned:    true,
			enrollErr: errors.New("enrollment failed: Bootstrap token owner is banned"),
			wantErr:   &netprovider.Error{Kind: netprovider.KindRemoved, Problem: "the relay {url} removed this agent (peer {peer}); ask its operator to let it back with `a2a-serve relay invite coder`, then run `a2a-serve restart`"},
		},
		{
			name:         "relay that refuses for another reason",
			token:        "invite",
			enrollErr:    errors.New("connection refused"),
			wantPlainErr: "joining the relay {url}: connection refused",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.token != "" && tc.saved != "" {
				recordInvite(t, dir, tc.token)
			}
			key := marshalLibp2pEd25519Key(t)
			if err := meshnet.WriteStateDir(dir, key, &api.MemberCredential{ControlPlaneUrl: "https://relay.example.com", Biscuit: []byte(tc.saved)}); err != nil {
				t.Fatalf("meshnet.WriteStateDir() error = %v", err)
			}
			peerID := savedPeerID(dir)
			relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				payload := encodeControlPlaneInfo("", "", "")
				if tc.banned {
					payload = protowire.AppendTag(payload, 5, protowire.BytesType)
					payload = protowire.AppendString(payload, peerID)
				}
				if _, err := w.Write(payload); err != nil {
					t.Errorf("writing /info: %v", err)
				}
			}))
			t.Cleanup(relay.Close)
			if tc.owner != "" {
				if err := saveOwner(dir, tc.owner); err != nil {
					t.Fatalf("saveOwner() error = %v", err)
				}
			}
			fake := &fakeMeshRelay{stateDir: dir, enrollErr: tc.enrollErr}
			a := newAgentMesh(&fakeHost{}, fake.serve)
			closeMesh(t, a)
			expand := func(s string) string {
				return strings.NewReplacer("{url}", relay.URL, "{peer}", peerID).Replace(s)
			}

			got, err := a.Join(t.Context(), netprovider.JoinRequest{Agent: "coder", URL: relay.URL, StateDir: dir, Token: tc.token, Private: true})

			switch {
			case tc.wantErr != nil:
				var perr *netprovider.Error
				if !errors.As(err, &perr) {
					t.Fatalf("agentMesh.Join() error = %v, want a %s error", err, tc.wantErr.Kind)
				}
				want := *tc.wantErr
				want.Problem = expand(want.Problem)
				if diff := cmp.Diff(want, *perr); diff != "" {
					t.Fatalf("agentMesh.Join() error wrong result (-want +got) diff = %s", diff)
				}
			case tc.wantPlainErr != "":
				var perr *netprovider.Error
				if err == nil || errors.As(err, &perr) || !strings.HasPrefix(err.Error(), expand(tc.wantPlainErr)) {
					t.Fatalf("agentMesh.Join() error = %v, want %q without a kind", err, expand(tc.wantPlainErr))
				}
			default:
				if err != nil {
					t.Fatalf("agentMesh.Join() error = %v", err)
				}
				want := tc.want
				want.Node = peerID
				if diff := cmp.Diff(want, got); diff != "" {
					t.Fatalf("agentMesh.Join() wrong result (-want +got) diff = %s", diff)
				}
			}
		})
	}
}

func TestJoinTwice(t *testing.T) {
	t.Parallel()
	relay := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(relay.Close)
	dir := t.TempDir()
	if err := meshnet.WriteStateDir(dir, marshalLibp2pEd25519Key(t), &api.MemberCredential{ControlPlaneUrl: relay.URL, Biscuit: []byte("valid")}); err != nil {
		t.Fatalf("meshnet.WriteStateDir() error = %v", err)
	}
	fake := &fakeMeshRelay{stateDir: dir}
	a := newAgentMesh(&fakeHost{}, fake.serve)
	closeMesh(t, a)
	req := netprovider.JoinRequest{Agent: "coder", URL: relay.URL, StateDir: dir, Private: true}
	if _, err := a.Join(t.Context(), req); err != nil {
		t.Fatalf("agentMesh.Join() error = %v", err)
	}

	if _, err := a.Join(t.Context(), req); err == nil {
		t.Fatal("agentMesh.Join() again error = nil, want error")
	}
}
