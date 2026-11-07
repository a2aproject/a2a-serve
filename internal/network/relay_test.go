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

package network

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/encoding/protowire"
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
		if withOIDC {
			payload = encodeControlPlaneInfo(f.URL+"/idp", "mesh-client", "mesh", "/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWRouter")
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
	l := &RelayLogin{RelayURL: relay.URL, StateDir: dir, HTTP: relay.Client(), Poll: time.Millisecond,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}

	got, err := l.Token(context.Background())
	if err != nil {
		t.Fatalf("RelayLogin.Token() error = %v", err)
	}
	if got != token {
		t.Fatalf("RelayLogin.Token() = %q, want the ID token", got)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], relay.URL+"/idp/activate?code=ABCD") {
		t.Fatalf("RelayLogin printed %q, want the sign-in link", logs)
	}
	if owner := SavedOwner(dir); owner != "ben@example.com" {
		t.Fatalf("SavedOwner() = %q, want the email signed in with", owner)
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

	tests := []struct {
		name      string
		relay     *httptest.Server
		wantErr   bool
		wantOwner string
	}{
		{name: "no oidc on relay", relay: fakeRelay(t, unverified, 0, false, false).Server, wantErr: true},
		{name: "malformed protobuf info", relay: badWire, wantErr: true},
		{name: "no device sign-in", relay: fakeRelay(t, unverified, 0, true, false).Server, wantErr: true},
		{name: "unverified email", relay: fakeRelay(t, unverified, 0, true, true).Server},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			l := &RelayLogin{RelayURL: tc.relay.URL, StateDir: dir, HTTP: tc.relay.Client(), Poll: time.Millisecond}
			if _, err := l.Token(context.Background()); (err != nil) != tc.wantErr {
				t.Fatalf("RelayLogin.Token() error = %v, want error %v", err, tc.wantErr)
			}
			if owner := SavedOwner(dir); owner != tc.wantOwner {
				t.Fatalf("SavedOwner() = %q, want %q", owner, tc.wantOwner)
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
	l := &RelayLogin{RelayURL: relay.URL, StateDir: dir, HTTP: relay.Client(), Poll: time.Millisecond,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	if _, err := l.Token(context.Background()); err != nil {
		t.Fatalf("RelayLogin.Token() error = %v", err)
	}
	logs = nil

	got, err := l.Token(context.Background())

	if err != nil {
		t.Fatalf("RelayLogin.Token() renewal error = %v", err)
	}
	if got != token {
		t.Fatalf("RelayLogin.Token() renewal = %q, want the ID token", got)
	}
	if relay.refreshes.Load() != 1 || relay.polls.Load() != 1 {
		t.Fatalf("renewal used %d refresh grants and %d device polls in all, want 1 and 1", relay.refreshes.Load(), relay.polls.Load())
	}
	if len(logs) != 0 {
		t.Fatalf("RelayLogin printed %q on renewal, want nothing", logs)
	}
}

func TestRelayLoginSignsInAgainWhenTheRelayRefusesTheRefreshToken(t *testing.T) {
	t.Parallel()
	token := fakeIDToken(t, map[string]any{"email": "ben@example.com", "email_verified": true})
	relay := fakeRelay(t, token, 0, true, true)
	dir := t.TempDir()
	var logs []string
	l := &RelayLogin{RelayURL: relay.URL, StateDir: dir, HTTP: relay.Client(), Poll: time.Millisecond,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	if _, err := l.Token(context.Background()); err != nil {
		t.Fatalf("RelayLogin.Token() error = %v", err)
	}
	relay.refused.Store(true)
	logs = nil

	if _, err := l.Token(context.Background()); err != nil {
		t.Fatalf("RelayLogin.Token() error = %v", err)
	}

	want := []string{
		"! relay: ben@example.com may no longer join this relay[2J",
		"→ sign in to the relay " + relay.URL + ": " + relay.URL + "/idp/activate?code=ABCD (code ABCD)",
		"✓ signed in to the relay as ben@example.com",
	}
	if diff := cmp.Diff(want, logs); diff != "" {
		t.Fatalf("RelayLogin printed wrong result (-want +got) diff = %s", diff)
	}
}

func TestRelayLoginSignsInAgainAtANewRelay(t *testing.T) {
	t.Parallel()
	token := fakeIDToken(t, map[string]any{"email": "ben@example.com", "email_verified": true})
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{name: "signed in at another relay", setup: func(t *testing.T, dir string) {
			old := fakeRelay(t, token, 0, true, true)
			l := &RelayLogin{RelayURL: old.URL, StateDir: dir, HTTP: old.Client(), Poll: time.Millisecond}
			if _, err := l.Token(context.Background()); err != nil {
				t.Fatalf("RelayLogin.Token() at the old relay error = %v", err)
			}
		}},
		{name: "token saved without its relay", setup: func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "refresh-token"), []byte("rt"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tc.setup(t, dir)
			relay := fakeRelay(t, token, 0, true, true)
			l := &RelayLogin{RelayURL: relay.URL, StateDir: dir, HTTP: relay.Client(), Poll: time.Millisecond}

			if _, err := l.Token(context.Background()); err != nil {
				t.Fatalf("RelayLogin.Token() error = %v", err)
			}

			if got := relay.refreshes.Load(); got != 0 {
				t.Fatalf("refresh grants at the new relay = %d, want 0: the token belongs to another relay", got)
			}
			if got := relay.polls.Load(); got != 1 {
				t.Fatalf("device polls at the new relay = %d, want 1", got)
			}
			if _, err := l.Token(context.Background()); err != nil {
				t.Fatalf("RelayLogin.Token() renewal error = %v", err)
			}
			if got := relay.refreshes.Load(); got != 1 {
				t.Fatalf("refresh grants after signing in at the new relay = %d, want 1", got)
			}
		})
	}
}

func TestRelayLoginKeepsTheRefreshTokenWhileTheRelayIsDown(t *testing.T) {
	t.Parallel()
	token := fakeIDToken(t, map[string]any{"email": "ben@example.com", "email_verified": true})
	relay := fakeRelay(t, token, 0, true, true)
	dir := t.TempDir()
	l := &RelayLogin{RelayURL: relay.URL, StateDir: dir, HTTP: relay.Client(), Poll: time.Millisecond}
	if _, err := l.Token(context.Background()); err != nil {
		t.Fatalf("RelayLogin.Token() error = %v", err)
	}
	relay.down.Store(true)

	if _, err := l.Token(context.Background()); err == nil {
		t.Fatalf("RelayLogin.Token() while the relay is down error = nil, want error")
	}
	relay.down.Store(false)
	if _, err := l.Token(context.Background()); err != nil {
		t.Fatalf("RelayLogin.Token() after the relay is back error = %v", err)
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
	l := &RelayLogin{RelayURL: relay.URL, StateDir: t.TempDir(), HTTP: relay.Client(), Poll: time.Millisecond}

	got, err := l.Token(context.Background())

	if err != nil {
		t.Fatalf("RelayLogin.Token() error = %v", err)
	}
	if got != token {
		t.Fatalf("RelayLogin.Token() = %q, want the ID token", got)
	}
}

func TestIsRelayURL(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{"https://relay.example.com": true, "http://localhost:8080": true, "tailscale": false, "lan": false, "": false} {
		if got := IsRelayURL(v); got != want {
			t.Fatalf("IsRelayURL(%q) = %v, want %v", v, got, want)
		}
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

			err := relayRemoval(t.Context(), srv.Client(), RelayConfig{URL: srv.URL, AgentName: "coder", Token: tt.token}, "12D3KooWMe")

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
