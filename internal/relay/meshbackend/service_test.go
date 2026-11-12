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

package meshbackend_test

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/backoff"
	"github.com/a2aproject/a2a-serve/internal/googleauth"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/meshadmin"
	"github.com/a2aproject/a2a-serve/internal/relay/meshadmin/meshtest"
	"github.com/a2aproject/a2a-serve/internal/relay/meshbackend"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// testHTTP has a transport of its own: httptest.Server.Close closes the idle
// connections of http.DefaultTransport, which breaks requests that parallel
// tests send through it at the same time.
var testHTTP = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}

const (
	adminToken = "admin-secret"
	relayURL   = "https://relay.example.com"
)

var (
	joined = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	seen   = time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
)

var policy = meshadmin.Policy{Roles: []meshadmin.Role{{Name: meshadmin.RoleNode, AllowedTargets: []string{"*"}}}}

func newMesh(t *testing.T) *meshtest.Mesh {
	t.Helper()
	m := meshtest.New(t, adminToken)
	m.AddNode(meshadmin.Node{PeerID: "12D3router", Role: meshadmin.RoleRouter, Enrolled: joined}, seen)
	m.AddNode(meshadmin.Node{PeerID: "12D3coder", Role: meshadmin.RoleNode, EnrollmentType: "oidc", ClaimsJSON: `{"email":"Anna@example.com","email_verified":true,"hd":"example.com"}`, Enrolled: joined}, seen, "coder")
	m.AddNode(meshadmin.Node{PeerID: "12D3reviewer", Role: meshadmin.RoleNode, EnrollmentType: "oidc", ClaimsJSON: `{"email":"anna@example.com","hd":"example.com"}`, Enrolled: joined.Add(time.Minute)}, seen, "reviewer")
	m.AddNode(meshadmin.Node{PeerID: "12D3bot", Role: meshadmin.RoleNode, EnrollmentType: "bootstrap", Enrolled: joined.Add(time.Hour)}, seen, "ci-bot")
	m.AddNode(meshadmin.Node{PeerID: "12D3gone", Role: meshadmin.RoleNode, EnrollmentType: "bootstrap", Enrolled: joined.Add(-time.Hour), Banned: true}, seen, "old")
	return m
}

func newService(t *testing.T, m *meshtest.Mesh) *meshbackend.Service {
	t.Helper()
	return newServiceAllowing(t, m, "*@example.com")
}

// newServiceAllowing returns the service of a relay whose allowlist is allow.
func newServiceAllowing(t *testing.T, m *meshtest.Mesh, allow ...string) *meshbackend.Service {
	t.Helper()
	return newServiceIn(m, filepath.Join(t.TempDir(), "removed.json"), allow...)
}

// newServiceIn returns the service of a relay that remembers removed agents
// in the file removed, and invite names next to it.
func newServiceIn(m *meshtest.Mesh, removed string, allow ...string) *meshbackend.Service {
	return meshbackend.NewService(meshbackend.ServiceOptions{
		Mesh:        meshadmin.New(m.URL, adminToken, testHTTP),
		Removed:     meshbackend.NewRemovedAgents(removed),
		InviteNames: meshbackend.NewInviteNames(filepath.Join(filepath.Dir(removed), "invite-names.json")),
		Policy:      policy,
		Status: func() control.Status {
			return control.Status{URL: relayURL, PID: 42, Started: joined, Versions: control.Versions{A2AServe: "v1.0.0", AgentMesh: "v0.2.0"}, Join: "invites"}
		},
		Admits: func(m control.Member) bool {
			return googleauth.Allowed(allow, googleauth.Claims{Email: m.Email, EmailVerified: true, HostedDomain: m.Domain})
		},
		Now: func() time.Time { return meshtest.Issued },
	})
}

// privateDir returns a directory closed to others, which a control socket
// needs. Its path is short enough for a Unix socket.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := testutil.ShortTempDir(t)
	return dir
}

// socketClient serves svc on a control socket and returns its client.
func socketClient(t *testing.T, svc *meshbackend.Service, restart func()) *control.Client {
	t.Helper()
	path := filepath.Join(privateDir(t), "relay.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: control.Handler(svc, restart), ReadHeaderTimeout: time.Second}
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Error(err)
		}
	})
	return control.NewSocketClient(path, 10*time.Second)
}

// remoteClient serves svc behind the admin token and returns a client
// with token.
func remoteClient(t *testing.T, svc *meshbackend.Service, token string) *control.Client {
	t.Helper()
	failures := backoff.NewFailures(time.Millisecond, time.Millisecond, time.Now)
	srv := httptest.NewServer(http.StripPrefix(control.RemotePath, control.RequireToken(adminToken, failures, control.Handler(svc, nil))))
	t.Cleanup(srv.Close)
	return control.NewRemoteClient(srv.URL, token, 10*time.Second, nil)
}

func TestOperationsOverEachTransport(t *testing.T) {
	t.Parallel()
	transports := map[string]func(*testing.T, *meshbackend.Service) *control.Client{
		"socket": func(t *testing.T, svc *meshbackend.Service) *control.Client { return socketClient(t, svc, nil) },
		"remote": func(t *testing.T, svc *meshbackend.Service) *control.Client { return remoteClient(t, svc, adminToken) },
	}
	for name, newClient := range transports {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMesh(t)
			c := newClient(t, newService(t, m))
			ctx := t.Context()

			inv, err := c.Invite(ctx, control.InviteRequest{Name: "ci-bot", Hours: 48})
			if err != nil {
				t.Fatalf("Client.Invite() error = %v", err)
			}
			wantInvite := &control.Invite{ID: "1", Name: "ci-bot", Token: "mesh-bt-1", Expires: meshtest.Issued.Add(48 * time.Hour), URL: relayURL}
			if diff := cmp.Diff(wantInvite, inv); diff != "" {
				t.Fatalf("Client.Invite() wrong result (-want +got) diff = %s", diff)
			}

			members, err := c.Members(ctx, false)
			if err != nil {
				t.Fatalf("Client.Members() error = %v", err)
			}
			wantMembers := []control.Member{
				{Agent: "coder", Email: "anna@example.com", Domain: "example.com", Joined: joined, Seen: seen, Peer: "12D3coder"},
				{Agent: "reviewer", Email: "anna@example.com", Domain: "example.com", Joined: joined.Add(time.Minute), Seen: seen, Peer: "12D3reviewer"},
				{Agent: "ci-bot", Invite: true, Joined: joined.Add(time.Hour), Seen: seen, Peer: "12D3bot"},
				{Invite: true, InviteName: "ci-bot", Unused: true, InviteID: "1", Expires: meshtest.Issued.Add(48 * time.Hour)},
			}
			if diff := cmp.Diff(wantMembers, members); diff != "" {
				t.Fatalf("Client.Members() wrong result (-want +got) diff = %s", diff)
			}

			st, err := c.Status(ctx)
			if err != nil {
				t.Fatalf("Client.Status() error = %v", err)
			}
			wantStatus := &control.Status{URL: relayURL, PID: 42, Started: joined, Versions: control.Versions{A2AServe: "v1.0.0", AgentMesh: "v0.2.0"}, Join: "invites", Members: 3}
			if diff := cmp.Diff(wantStatus, st); diff != "" {
				t.Fatalf("Client.Status() wrong result (-want +got) diff = %s", diff)
			}

			res, err := c.Remove(ctx, control.RemoveRequest{Target: "ci-bot"})
			if err != nil {
				t.Fatalf("Client.Remove() error = %v", err)
			}
			if diff := cmp.Diff([]string{"12D3bot", "12D3gone"}, m.Banned()); diff != "" || len(res.Removed) != 2 {
				t.Fatalf("Client.Remove() banned wrong nodes (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff([]string{"1"}, m.Revoked()); diff != "" {
				t.Fatalf("Client.Remove() revoked wrong invites (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestReconcile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		allow        []string
		before       []string
		wantBanned   []string
		wantPolicies []meshadmin.Policy
	}{
		{name: "everyone still allowed", allow: []string{"*@example.com"}, wantBanned: []string{"12D3gone"}},
		{name: "email dropped", allow: []string{"ben@example.com"}, wantBanned: []string{"12D3coder", "12D3reviewer", "12D3gone"}},
		{name: "sign-in off", wantBanned: []string{"12D3coder", "12D3reviewer", "12D3gone"}},
		{name: "email allowed again", allow: []string{"anna@example.com"}, before: []string{"12D3coder", "12D3reviewer"}, wantBanned: []string{"12D3gone"}, wantPolicies: []meshadmin.Policy{policy}},
		{name: "removed invite stays removed", allow: []string{"*@example.com"}, before: []string{"12D3bot"}, wantBanned: []string{"12D3bot", "12D3gone"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := newMesh(t)
			for _, peer := range tt.before {
				if _, err := newService(t, m).Remove(t.Context(), control.RemoveRequest{Target: peer}); err != nil {
					t.Fatal(err)
				}
			}

			if _, _, err := newServiceAllowing(t, m, tt.allow...).Reconcile(t.Context()); err != nil {
				t.Fatalf("Service.Reconcile() error = %v", err)
			}

			if diff := cmp.Diff(tt.wantBanned, m.Banned(), cmp.Transformer("sort", sorted)); diff != "" {
				t.Fatalf("Service.Reconcile() left wrong nodes banned (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tt.wantPolicies, m.Policies()); diff != "" {
				t.Fatalf("Service.Reconcile() wrong policies applied (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestRemoveReportsPeopleTheRelayLetsBack(t *testing.T) {
	t.Parallel()
	m := newMesh(t)

	res, err := newService(t, m).Remove(t.Context(), control.RemoveRequest{Target: "anna@example.com"})

	if err != nil {
		t.Fatalf("Service.Remove() error = %v", err)
	}
	if diff := cmp.Diff([]string{"anna@example.com"}, res.Admitted); diff != "" {
		t.Fatalf("Service.Remove() admitted wrong result (-want +got) diff = %s", diff)
	}
}

func TestInvite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		req     control.InviteRequest
		want    []meshadmin.BootstrapTokenRequest
		wantErr bool
	}{
		{
			name: "default lifetime",
			req:  control.InviteRequest{Name: "ci-bot"},
			want: []meshadmin.BootstrapTokenRequest{{Role: meshadmin.RoleNode, TTLHours: 24, MaxUsages: 1, Description: "a2a-serve invite: ci-bot", AutonomousRecovery: true}},
		},
		{
			name: "a week",
			req:  control.InviteRequest{Name: "ci-bot", Hours: 168},
			want: []meshadmin.BootstrapTokenRequest{{Role: meshadmin.RoleNode, TTLHours: 168, MaxUsages: 1, Description: "a2a-serve invite: ci-bot", AutonomousRecovery: true}},
		},
		{name: "longer than a week", req: control.InviteRequest{Name: "ci-bot", Hours: 169}, wantErr: true},
		{name: "no name", req: control.InviteRequest{Name: ""}, wantErr: true},
		{name: "space in the name", req: control.InviteRequest{Name: "bad name!"}, wantErr: true},
		{name: "name starting with a dot", req: control.InviteRequest{Name: ".bot"}, wantErr: true},
		{name: "name too long", req: control.InviteRequest{Name: strings.Repeat("a", 65)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := newMesh(t)

			_, err := newService(t, m).Invite(t.Context(), tt.req)

			if tt.wantErr {
				if !agenterr.IsRequest(err) {
					t.Fatalf("Service.Invite() error = %v, want an invalid request", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Service.Invite() error = %v", err)
			}
			if diff := cmp.Diff(tt.want, m.Tokens()); diff != "" {
				t.Fatalf("Service.Invite() wrong token request (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestInviteLetsBackARemovedAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		remove     []string
		invite     string
		wantBack   *control.Member
		wantBanned []string
	}{
		{
			name:       "invited agent",
			remove:     []string{"ci-bot"},
			invite:     "ci-bot",
			wantBack:   &control.Member{Agent: "ci-bot", Invite: true, Joined: joined.Add(time.Hour), Peer: "12D3bot"},
			wantBanned: []string{"12D3gone"},
		},
		{name: "another name", remove: []string{"ci-bot"}, invite: "coder", wantBanned: []string{"12D3bot", "12D3gone"}},
		{name: "agent that signed in", remove: []string{"reviewer"}, invite: "reviewer", wantBanned: []string{"12D3reviewer", "12D3gone"}},
		{name: "removed before the relay remembered names", invite: "old", wantBanned: []string{"12D3gone"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := newMesh(t)
			removed := filepath.Join(t.TempDir(), "removed.json")
			for _, target := range tt.remove {
				if _, err := newServiceIn(m, removed).Remove(t.Context(), control.RemoveRequest{Target: target}); err != nil {
					t.Fatalf("Service.Remove() error = %v", err)
				}
			}

			inv, err := newServiceIn(m, removed).Invite(t.Context(), control.InviteRequest{Name: tt.invite})

			if err != nil {
				t.Fatalf("Service.Invite() error = %v", err)
			}
			if diff := cmp.Diff(tt.wantBack, inv.LetBack); diff != "" {
				t.Fatalf("Service.Invite() wrong agent let back (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tt.wantBanned, m.Banned(), cmp.Transformer("sort", sorted)); diff != "" {
				t.Fatalf("Service.Invite() left wrong nodes banned (-want +got) diff = %s", diff)
			}
			var wantPolicies []meshadmin.Policy
			if tt.wantBack != nil {
				wantPolicies = []meshadmin.Policy{policy}
			}
			if diff := cmp.Diff(wantPolicies, m.Policies()); diff != "" {
				t.Fatalf("Service.Invite() wrong policies applied (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestInviteLetsBackARemovedAgentOnce(t *testing.T) {
	t.Parallel()
	m := newMesh(t)
	svc := newService(t, m)
	if _, err := svc.Remove(t.Context(), control.RemoveRequest{Target: "ci-bot"}); err != nil {
		t.Fatalf("Service.Remove() error = %v", err)
	}
	if _, err := svc.Invite(t.Context(), control.InviteRequest{Name: "ci-bot"}); err != nil {
		t.Fatalf("Service.Invite() error = %v", err)
	}

	inv, err := svc.Invite(t.Context(), control.InviteRequest{Name: "ci-bot"})

	if err != nil {
		t.Fatalf("Service.Invite() error = %v", err)
	}
	if inv.LetBack != nil {
		t.Fatalf("Service.Invite() let back %+v again, want nothing", inv.LetBack)
	}
}

func TestMembersNamesRemovedAgents(t *testing.T) {
	t.Parallel()
	m := newMesh(t)
	svc := newService(t, m)
	if _, err := svc.Remove(t.Context(), control.RemoveRequest{Target: "ci-bot"}); err != nil {
		t.Fatalf("Service.Remove() error = %v", err)
	}

	ms, err := svc.Members(t.Context(), true)

	if err != nil {
		t.Fatalf("Service.Members() error = %v", err)
	}
	var got []string
	for _, mem := range ms {
		if mem.Banned {
			got = append(got, mem.Peer+"="+mem.Agent)
		}
	}
	if diff := cmp.Diff([]string{"12D3gone=", "12D3bot=ci-bot"}, got); diff != "" {
		t.Fatalf("Service.Members() wrong removed agents (-want +got) diff = %s", diff)
	}
}

func TestMembersNameTheInviteAgentsJoinedWith(t *testing.T) {
	t.Parallel()
	m := newMesh(t)
	svc := newService(t, m)
	inv, err := svc.Invite(t.Context(), control.InviteRequest{Name: "build-box"})
	if err != nil {
		t.Fatalf("Service.Invite() error = %v", err)
	}
	m.Enroll(inv.ID, meshadmin.Node{PeerID: "12D3box", Role: meshadmin.RoleNode, EnrollmentType: "bootstrap", Enrolled: joined.Add(2 * time.Hour)}, seen, "ci-bot")
	want := []control.Member{
		{Agent: "coder", Email: "anna@example.com", Domain: "example.com", Joined: joined, Seen: seen, Peer: "12D3coder"},
		{Agent: "reviewer", Email: "anna@example.com", Domain: "example.com", Joined: joined.Add(time.Minute), Seen: seen, Peer: "12D3reviewer"},
		{Agent: "ci-bot", Invite: true, Joined: joined.Add(time.Hour), Seen: seen, Peer: "12D3bot"},
		{Agent: "ci-bot", Invite: true, InviteName: "build-box", Joined: joined.Add(2 * time.Hour), Seen: seen, Peer: "12D3box"},
	}

	for _, step := range []string{"enrolled", "enrollment forgotten"} {
		ms, err := svc.Members(t.Context(), false)
		if err != nil {
			t.Fatalf("Service.Members() %s error = %v", step, err)
		}
		if diff := cmp.Diff(want, ms); diff != "" {
			t.Fatalf("Service.Members() %s wrong result (-want +got) diff = %s", step, diff)
		}
		m.ForgetEnrollments()
	}
}

func TestRemoveRevokesUnusedInvites(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		target      string
		wantRevoked []string
		wantBanned  []string
		wantErr     error
	}{
		{name: "by name", target: "build-box", wantRevoked: []string{"1", "2"}, wantBanned: []string{"12D3gone"}},
		{name: "by ID", target: "2", wantRevoked: []string{"2"}, wantBanned: []string{"12D3gone"}},
		{name: "with the agent of that name", target: "ci-bot", wantRevoked: []string{"3"}, wantBanned: []string{"12D3bot", "12D3gone"}},
		{name: "used invite", target: "worker", wantBanned: []string{"12D3gone"}, wantErr: &agenterr.NotFoundError{Kind: "member", Name: "worker"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := newMesh(t)
			svc := newService(t, m)
			for _, name := range []string{"build-box", "build-box", "ci-bot", "worker"} {
				if _, err := svc.Invite(t.Context(), control.InviteRequest{Name: name}); err != nil {
					t.Fatalf("Service.Invite() error = %v", err)
				}
			}
			m.Enroll("4", meshadmin.Node{PeerID: "12D3worker", Role: meshadmin.RoleNode, Enrolled: joined}, seen, "builder")

			_, err := svc.Remove(t.Context(), control.RemoveRequest{Target: tt.target})

			if diff := cmp.Diff(tt.wantErr, err, cmp.Comparer(func(a, b error) bool { return a.Error() == b.Error() })); diff != "" {
				t.Fatalf("Service.Remove() wrong error (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tt.wantRevoked, m.Revoked()); diff != "" {
				t.Fatalf("Service.Remove() revoked wrong invites (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tt.wantBanned, m.Banned(), cmp.Transformer("sort", sorted)); diff != "" {
				t.Fatalf("Service.Remove() banned wrong nodes (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		target     string
		wantBanned []string
		wantErr    error
	}{
		{name: "by agent", target: "reviewer", wantBanned: []string{"12D3reviewer", "12D3gone"}},
		{name: "by peer ID", target: "12D3bot", wantBanned: []string{"12D3bot", "12D3gone"}},
		{name: "every agent of an email", target: "ANNA@example.com", wantBanned: []string{"12D3coder", "12D3reviewer", "12D3gone"}},
		{name: "unknown", target: "nobody", wantBanned: []string{"12D3gone"}, wantErr: &agenterr.NotFoundError{Kind: "member", Name: "nobody"}},
		{name: "already removed", target: "old", wantBanned: []string{"12D3gone"}, wantErr: &agenterr.NotFoundError{Kind: "member", Name: "old"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := newMesh(t)

			_, err := newService(t, m).Remove(t.Context(), control.RemoveRequest{Target: tt.target})

			if diff := cmp.Diff(tt.wantErr, err, cmp.Comparer(func(a, b error) bool { return a.Error() == b.Error() })); diff != "" {
				t.Fatalf("Service.Remove() wrong error (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tt.wantBanned, m.Banned(), cmp.Transformer("sort", sorted)); diff != "" {
				t.Fatalf("Service.Remove() banned wrong nodes (-want +got) diff = %s", diff)
			}
		})
	}
}

func sorted(s []string) []string { return slices.Sorted(slices.Values(s)) }

func TestRemoveRefusesAnAmbiguousAgent(t *testing.T) {
	t.Parallel()
	m := newMesh(t)
	m.AddNode(meshadmin.Node{PeerID: "12D3coder2", Role: meshadmin.RoleNode, Enrolled: joined}, seen, "coder")

	_, err := newService(t, m).Remove(t.Context(), control.RemoveRequest{Target: "coder"})

	var conflict *agenterr.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Service.Remove() error = %v, want a conflict", err)
	}
	if got := m.Banned(); len(got) != 1 {
		t.Fatalf("Service.Remove() banned %v, want nothing new", got)
	}
}

func TestRemoteNeedsTheAdminToken(t *testing.T) {
	t.Parallel()
	svc := newService(t, newMesh(t))

	_, err := remoteClient(t, svc, "wrong").Status(t.Context())

	if err == nil {
		t.Fatal("Client.Status() with a wrong token error = nil, want an error")
	}
	if _, err := remoteClient(t, svc, adminToken).Status(t.Context()); err != nil {
		t.Fatalf("Client.Status() with the admin token error = %v", err)
	}
}

func TestRequireTokenBacksOffWrongTokensOnly(t *testing.T) {
	t.Parallel()
	failures := backoff.NewFailures(time.Minute, time.Minute, time.Now)
	h := control.RequireToken(adminToken, failures, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	call := func(token string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := call("wrong"); got != http.StatusUnauthorized {
		t.Fatalf("a wrong token got status %d, want %d", got, http.StatusUnauthorized)
	}
	if got := call("wrong again"); got != http.StatusTooManyRequests {
		t.Fatalf("a wrong token during the backoff got status %d, want %d", got, http.StatusTooManyRequests)
	}
	if got := call(adminToken); got != http.StatusOK {
		t.Fatalf("the admin token during the backoff got status %d, want %d", got, http.StatusOK)
	}
}

func TestSocketClient(t *testing.T) {
	t.Parallel()
	t.Run("not running", func(t *testing.T) {
		t.Parallel()
		c := control.NewSocketClient(filepath.Join(privateDir(t), "relay.sock"), time.Second)
		if _, err := c.Status(t.Context()); !errors.Is(err, control.ErrNotRunning) {
			t.Fatalf("Client.Status() error = %v, want %v", err, control.ErrNotRunning)
		}
	})
	t.Run("socket that others can reach", func(t *testing.T) {
		t.Parallel()
		dir := privateDir(t)
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		c := control.NewSocketClient(filepath.Join(dir, "relay.sock"), time.Second)
		if _, err := c.Status(t.Context()); err == nil || errors.Is(err, control.ErrNotRunning) {
			t.Fatalf("Client.Status() error = %v, want a refusal of the socket", err)
		}
	})
	t.Run("restart", func(t *testing.T) {
		t.Parallel()
		restarted := make(chan struct{})
		c := socketClient(t, newService(t, newMesh(t)), func() { close(restarted) })
		if err := c.Restart(t.Context()); err != nil {
			t.Fatalf("Client.Restart() error = %v", err)
		}
		select {
		case <-restarted:
		case <-time.After(5 * time.Second):
			t.Fatal("Client.Restart() didn't reach the relay")
		}
	})
	t.Run("restart is local only", func(t *testing.T) {
		t.Parallel()
		if err := remoteClient(t, newService(t, newMesh(t)), adminToken).Restart(t.Context()); err == nil {
			t.Fatal("Client.Restart() over the relay's URL error = nil, want an error")
		}
	})
}
