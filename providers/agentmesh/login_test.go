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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/agentmesh/api"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/a2aproject/a2a-serve/netprovider"
)

func fakeIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	data, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(data) + ".sig"
}

// encodeControlPlaneInfo encodes the wire layout of Agent Mesh's
// api.ControlPlaneInfoResponse (1: oidc_issuer, 2: client_id, 3: audience,
// 4: router_addresses).
func encodeControlPlaneInfo(issuer, clientID, audience string, routerAddrs ...string) []byte {
	var b []byte
	appendStr := func(num protowire.Number, v string) {
		if v == "" {
			return
		}
		b = protowire.AppendTag(b, num, protowire.BytesType)
		b = protowire.AppendString(b, v)
	}
	appendStr(1, issuer)
	appendStr(2, clientID)
	appendStr(3, audience)
	for _, addr := range routerAddrs {
		appendStr(4, addr)
	}
	return b
}

// fakeIdP is a relay's /info and identity provider. Its device flow grants
// the token after pending polls, with the refresh token "rt".
type fakeIdP struct {
	*httptest.Server
	idToken string
	pending int32
	polls   atomic.Int32
	// refreshes counts refresh grants; refused makes the relay refuse the
	// refresh token, and down makes it unavailable.
	refreshes atomic.Int32
	refused   atomic.Bool
	down      atomic.Bool
	// badGateways is how many device polls a proxy answers with an HTML 502.
	badGateways atomic.Int32
	// issuer replaces the relay's own identity provider in /info.
	issuer atomic.Pointer[string]
}

// fakeRelay serves a relay's /info and its identity provider's device flow,
// which grants the token after pending polls.
func fakeRelay(t *testing.T, idToken string, pending int, withOIDC, withDevice bool) *fakeIdP {
	t.Helper()
	f := &fakeIdP{idToken: idToken, pending: int32(pending)}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("writing: %v", err)
		}
	}
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/x-protobuf" {
			http.Error(w, "unexpected Accept: "+got, http.StatusNotAcceptable)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		var payload []byte
		issuer := f.URL + "/idp"
		if p := f.issuer.Load(); p != nil {
			issuer = *p
		}
		if withOIDC {
			payload = encodeControlPlaneInfo(issuer, "mesh-client", "mesh", "/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWRouter")
		} else {
			payload = encodeControlPlaneInfo("", "", "", "/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWRouter")
		}
		if _, err := w.Write(payload); err != nil {
			t.Errorf("writing /info: %v", err)
		}
	})
	mux.HandleFunc("GET /idp/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		conf := map[string]any{"token_endpoint": f.URL + "/idp/token"}
		if withDevice {
			conf["device_authorization_endpoint"] = f.URL + "/idp/device"
		}
		write(w, conf)
	})
	mux.HandleFunc("POST /idp/device", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("client_id") != "mesh-client" || r.FormValue("audience") != "mesh" {
			http.Error(w, "bad client", http.StatusBadRequest)
			return
		}
		write(w, map[string]any{"device_code": "dc", "user_code": "ABCD", "verification_uri_complete": f.URL + "/idp/activate?code=ABCD", "interval": 0, "expires_in": 60})
	})
	mux.HandleFunc("POST /idp/token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("grant_type") == "refresh_token" {
			f.refreshes.Add(1)
			switch {
			case f.down.Load():
				w.WriteHeader(http.StatusServiceUnavailable)
				write(w, map[string]string{"error": "temporarily_unavailable"})
			case f.refused.Load() || r.FormValue("refresh_token") != "rt" || r.FormValue("client_id") != "mesh-client":
				w.WriteHeader(http.StatusBadRequest)
				write(w, map[string]string{"error": "invalid_grant", "error_description": "ben@example.com may no longer join this relay\x1b[2J"})
			default:
				write(w, map[string]string{"id_token": f.idToken, "refresh_token": "rt"})
			}
			return
		}
		if r.FormValue("device_code") != "dc" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		if f.badGateways.Add(-1) >= 0 {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			if _, err := w.Write([]byte("<html>502 Bad Gateway</html>")); err != nil {
				t.Errorf("writing: %v", err)
			}
			return
		}
		if f.polls.Add(1) <= f.pending {
			w.WriteHeader(http.StatusBadRequest)
			write(w, map[string]string{"error": "authorization_pending"})
			return
		}
		write(w, map[string]string{"id_token": f.idToken, "refresh_token": "rt"})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func TestRelayLogin(t *testing.T) {
	t.Parallel()
	token := fakeIDToken(t, map[string]any{"email": "Ben@example.com", "email_verified": true})
	relay := fakeRelay(t, token, 2, true, true)
	dir := t.TempDir()
	var logs []string
	l := &relayLogin{RelayURL: relay.URL, StateDir: dir, HTTP: relay.Client(), Poll: time.Millisecond,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}

	got, err := l.Token(context.Background())
	if err != nil {
		t.Fatalf("relayLogin.Token() error = %v", err)
	}
	if got != token {
		t.Fatalf("relayLogin.Token() = %q, want the ID token", got)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], relay.URL+"/idp/activate?code=ABCD") {
		t.Fatalf("relayLogin printed %q, want the sign-in link", logs)
	}
	if owner := savedOwner(dir); owner != "ben@example.com" {
		t.Fatalf("savedOwner() = %q, want the email signed in with", owner)
	}
}

func TestRelayLoginErrors(t *testing.T) {
	t.Parallel()
	unverified := fakeIDToken(t, map[string]any{"email": "ben@example.com"})
	badWire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf")
		if _, err := w.Write([]byte{0xff, 0xff}); err != nil {
			t.Errorf("writing malformed /info: %v", err)
		}
	}))
	t.Cleanup(badWire.Close)
	cleartextIssuer := fakeRelay(t, unverified, 0, true, true)
	issuer := "http://idp.example.com"
	cleartextIssuer.issuer.Store(&issuer)

	tests := []struct {
		name      string
		relay     *httptest.Server
		wantErr   bool
		wantOwner string
	}{
		{name: "no oidc on relay", relay: fakeRelay(t, unverified, 0, false, false).Server, wantErr: true},
		{name: "malformed protobuf info", relay: badWire, wantErr: true},
		{name: "no device sign-in", relay: fakeRelay(t, unverified, 0, true, false).Server, wantErr: true},
		{name: "identity provider over http", relay: cleartextIssuer.Server, wantErr: true},
		{name: "unverified email", relay: fakeRelay(t, unverified, 0, true, true).Server},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			l := &relayLogin{RelayURL: tc.relay.URL, StateDir: dir, HTTP: tc.relay.Client(), Poll: time.Millisecond}
			if _, err := l.Token(context.Background()); (err != nil) != tc.wantErr {
				t.Fatalf("relayLogin.Token() error = %v, want error %v", err, tc.wantErr)
			}
			if owner := savedOwner(dir); owner != tc.wantOwner {
				t.Fatalf("savedOwner() = %q, want %q", owner, tc.wantOwner)
			}
		})
	}
}

func TestRelayLoginRenewsWithTheRefreshToken(t *testing.T) {
	t.Parallel()
	token := fakeIDToken(t, map[string]any{"email": "ben@example.com", "email_verified": true})
	relay := fakeRelay(t, token, 0, true, true)
	dir := t.TempDir()
	var logs []string
	l := &relayLogin{RelayURL: relay.URL, StateDir: dir, HTTP: relay.Client(), Poll: time.Millisecond,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	if _, err := l.Token(context.Background()); err != nil {
		t.Fatalf("relayLogin.Token() error = %v", err)
	}
	logs = nil

	got, err := l.Token(context.Background())

	if err != nil {
		t.Fatalf("relayLogin.Token() renewal error = %v", err)
	}
	if got != token {
		t.Fatalf("relayLogin.Token() renewal = %q, want the ID token", got)
	}
	if relay.refreshes.Load() != 1 || relay.polls.Load() != 1 {
		t.Fatalf("renewal used %d refresh grants and %d device polls in all, want 1 and 1", relay.refreshes.Load(), relay.polls.Load())
	}
	if len(logs) != 0 {
		t.Fatalf("relayLogin printed %q on renewal, want nothing", logs)
	}
}

func TestRelayLoginSignsInAgainWhenTheRelayRefusesTheRefreshToken(t *testing.T) {
	t.Parallel()
	token := fakeIDToken(t, map[string]any{"email": "ben@example.com", "email_verified": true})
	relay := fakeRelay(t, token, 0, true, true)
	dir := t.TempDir()
	var logs []string
	l := &relayLogin{RelayURL: relay.URL, StateDir: dir, HTTP: relay.Client(), Poll: time.Millisecond,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	if _, err := l.Token(context.Background()); err != nil {
		t.Fatalf("relayLogin.Token() error = %v", err)
	}
	relay.refused.Store(true)
	logs = nil

	if _, err := l.Token(context.Background()); err != nil {
		t.Fatalf("relayLogin.Token() error = %v", err)
	}

	want := []string{
		"! relay: ben@example.com may no longer join this relay[2J",
		"→ sign in to the relay " + relay.URL + ": " + relay.URL + "/idp/activate?code=ABCD (code ABCD)",
		"✓ signed in to the relay as ben@example.com",
	}
	if diff := cmp.Diff(want, logs); diff != "" {
		t.Fatalf("relayLogin printed wrong result (-want +got) diff = %s", diff)
	}
}

func TestRelayLoginSignsInAgainAtANewRelay(t *testing.T) {
	t.Parallel()
	token := fakeIDToken(t, map[string]any{"email": "ben@example.com", "email_verified": true})
	signIn := func(t *testing.T, relay *fakeIdP, dir string) {
		t.Helper()
		l := &relayLogin{RelayURL: relay.URL, StateDir: dir, HTTP: relay.Client(), Poll: time.Millisecond}
		if _, err := l.Token(context.Background()); err != nil {
			t.Fatalf("relayLogin.Token() error = %v", err)
		}
	}
	tests := []struct {
		name string
		// setup prepares dir before the agent moves to relay, and returns
		// the identity provider that issued the saved token, if any.
		setup func(t *testing.T, dir string, relay *fakeIdP) *fakeIdP
	}{
		{name: "signed in at another relay", setup: func(t *testing.T, dir string, _ *fakeIdP) *fakeIdP {
			old := fakeRelay(t, token, 0, true, true)
			signIn(t, old, dir)
			return old
		}},
		{name: "new relay names the old relay's identity provider", setup: func(t *testing.T, dir string, relay *fakeIdP) *fakeIdP {
			old := fakeRelay(t, token, 0, true, true)
			signIn(t, old, dir)
			issuer := old.URL + "/idp"
			relay.issuer.Store(&issuer)
			return old
		}},
		{name: "token saved without its relay", setup: func(t *testing.T, dir string, _ *fakeIdP) *fakeIdP {
			if err := os.WriteFile(filepath.Join(dir, legacyRefreshFile), []byte("rt"), 0o600); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			relay := fakeRelay(t, token, 0, true, true)
			idps := []*fakeIdP{relay}
			if old := tc.setup(t, dir, relay); old != nil {
				idps = append(idps, old)
			}
			grants := func() (refreshes, polls int32) {
				for _, f := range idps {
					refreshes += f.refreshes.Load()
					polls += f.polls.Load()
				}
				return refreshes, polls
			}
			_, pollsBefore := grants()

			signIn(t, relay, dir)

			if refreshes, polls := grants(); refreshes != 0 || polls != pollsBefore+1 {
				t.Fatalf("joining the new relay used %d refresh grants and %d device sign-ins, want 0 and 1", refreshes, polls-pollsBefore)
			}
			if _, err := os.Stat(filepath.Join(dir, legacyRefreshFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("os.Stat(%s) error = %v, want %v", legacyRefreshFile, err, os.ErrNotExist)
			}
			signIn(t, relay, dir)
			if refreshes, _ := grants(); refreshes != 1 {
				t.Fatalf("renewing at the new relay used %d refresh grants, want 1", refreshes)
			}
		})
	}
}

func TestRelayLoginKeepsTheRefreshTokenWhileTheRelayIsDown(t *testing.T) {
	t.Parallel()
	token := fakeIDToken(t, map[string]any{"email": "ben@example.com", "email_verified": true})
	relay := fakeRelay(t, token, 0, true, true)
	dir := t.TempDir()
	l := &relayLogin{RelayURL: relay.URL, StateDir: dir, HTTP: relay.Client(), Poll: time.Millisecond}
	if _, err := l.Token(context.Background()); err != nil {
		t.Fatalf("relayLogin.Token() error = %v", err)
	}
	relay.down.Store(true)

	if _, err := l.Token(context.Background()); err == nil {
		t.Fatalf("relayLogin.Token() while the relay is down error = nil, want error")
	}
	relay.down.Store(false)
	if _, err := l.Token(context.Background()); err != nil {
		t.Fatalf("relayLogin.Token() after the relay is back error = %v", err)
	}

	if got := relay.polls.Load(); got != 1 {
		t.Fatalf("device polls = %d, want 1: the agent keeps its refresh token while the relay is down", got)
	}
}

func TestRelayLoginWaitsThroughAFailedPoll(t *testing.T) {
	t.Parallel()
	token := fakeIDToken(t, map[string]any{"email": "ben@example.com", "email_verified": true})
	relay := fakeRelay(t, token, 0, true, true)
	relay.badGateways.Store(2)
	l := &relayLogin{RelayURL: relay.URL, StateDir: t.TempDir(), HTTP: relay.Client(), Poll: time.Millisecond}

	got, err := l.Token(context.Background())

	if err != nil {
		t.Fatalf("relayLogin.Token() error = %v", err)
	}
	if got != token {
		t.Fatalf("relayLogin.Token() = %q, want the ID token", got)
	}
}

func TestRelayRemoval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		token   string
		banned  []string
		wantErr string
	}{
		{
			name:    "invited agent",
			token:   "mesh-bt-1",
			banned:  []string{"12D3KooWOther", "12D3KooWMe"},
			wantErr: "the relay {url} removed this agent (peer 12D3KooWMe); ask its operator to let it back with `a2a-serve relay invite coder`, then run `a2a-serve restart`",
		},
		{
			name:    "agent that signed in",
			banned:  []string{"12D3KooWMe"},
			wantErr: "the relay {url} removed this agent (peer 12D3KooWMe); ask its operator to let you back on --allow-email and restart the relay",
		},
		{name: "agent on the relay", token: "mesh-bt-1", banned: []string{"12D3KooWOther"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				payload := encodeControlPlaneInfo("", "", "", "/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWRouter")
				for _, id := range tt.banned {
					payload = protowire.AppendTag(payload, 5, protowire.BytesType)
					payload = protowire.AppendString(payload, id)
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
				if _, err := w.Write(payload); err != nil {
					t.Errorf("writing /info: %v", err)
				}
			}))
			t.Cleanup(srv.Close)

			err := relayRemoval(t.Context(), srv.Client(), netprovider.JoinRequest{URL: srv.URL, Agent: "coder", Token: tt.token}, "12D3KooWMe")

			got := ""
			if err != nil {
				got = err.Error()
			}
			if want := strings.ReplaceAll(tt.wantErr, "{url}", srv.URL); got != want {
				t.Fatalf("relayRemoval() error = %q, want %q", got, want)
			}
		})
	}
}

func TestRelayLoginDecodesControlPlaneInfoProto(t *testing.T) {
	t.Parallel()
	token := fakeIDToken(t, map[string]any{"email": "Ben@example.com", "email_verified": true})
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		payload, err := proto.Marshal(&api.ControlPlaneInfoResponse{
			OidcIssuer:      srv.URL + "/idp",
			ClientId:        "mesh-client",
			Audience:        "mesh-aud",
			RouterAddresses: []string{"/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWRouter"},
			BannedPeerIds:   []string{"12D3KooWBanned"},
			Routers:         []*api.RouterInfo{{PeerId: "12D3KooWRouter", Connections: 2, ConnectionLimit: 100}},
			Directory:       api.NewDirectoryParameters(3, 5*time.Minute),
		})
		if err != nil {
			t.Errorf("proto.Marshal() error = %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		if _, err := w.Write(payload); err != nil {
			t.Errorf("Write() error = %v", err)
		}
	})
	mux.HandleFunc("GET /idp/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{
			"device_authorization_endpoint": srv.URL + "/idp/device",
			"token_endpoint":                srv.URL + "/idp/token",
		}); err != nil {
			t.Errorf("Encode() error = %v", err)
		}
	})
	mux.HandleFunc("POST /idp/device", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("client_id") != "mesh-client" || r.FormValue("audience") != "mesh-aud" {
			http.Error(w, "bad client or audience", http.StatusBadRequest)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dc-proto",
			"user_code":                 "WXYZ",
			"verification_uri_complete": srv.URL + "/idp/activate?code=WXYZ",
			"interval":                  0,
			"expires_in":                60,
		}); err != nil {
			t.Errorf("Encode() error = %v", err)
		}
	})
	mux.HandleFunc("POST /idp/token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("device_code") != "dc-proto" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]string{"id_token": token}); err != nil {
			t.Errorf("Encode() error = %v", err)
		}
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	l := &relayLogin{RelayURL: srv.URL, StateDir: dir, HTTP: srv.Client(), Poll: time.Millisecond}
	got, err := l.Token(context.Background())
	if err != nil {
		t.Fatalf("relayLogin.Token() error = %v, want nil", err)
	}
	if got != token {
		t.Fatalf("relayLogin.Token() = %q, want %q", got, token)
	}
	if owner := savedOwner(dir); owner != "ben@example.com" {
		t.Fatalf("savedOwner() = %q, want %q", owner, "ben@example.com")
	}
}
