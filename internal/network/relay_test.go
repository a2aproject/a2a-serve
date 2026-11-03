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
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// fakeRelay serves a relay's /info and its identity provider's device flow,
// which grants the token after pending polls.
func fakeRelay(t *testing.T, idToken string, pending int, withOIDC, withDevice bool) *httptest.Server {
	t.Helper()
	var polls atomic.Int32
	var srv *httptest.Server
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
			payload = encodeControlPlaneInfo(srv.URL+"/idp", "mesh-client", "mesh", "/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWRouter")
		} else {
			payload = encodeControlPlaneInfo("", "", "", "/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWRouter")
		}
		if _, err := w.Write(payload); err != nil {
			t.Errorf("writing /info: %v", err)
		}
	})
	mux.HandleFunc("GET /idp/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		conf := map[string]any{"token_endpoint": srv.URL + "/idp/token"}
		if withDevice {
			conf["device_authorization_endpoint"] = srv.URL + "/idp/device"
		}
		write(w, conf)
	})
	mux.HandleFunc("POST /idp/device", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("client_id") != "mesh-client" || r.FormValue("audience") != "mesh" {
			http.Error(w, "bad client", http.StatusBadRequest)
			return
		}
		write(w, map[string]any{"device_code": "dc", "user_code": "ABCD", "verification_uri_complete": srv.URL + "/idp/activate?code=ABCD", "interval": 0, "expires_in": 60})
	})
	mux.HandleFunc("POST /idp/token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("device_code") != "dc" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		if int(polls.Add(1)) <= pending {
			w.WriteHeader(http.StatusBadRequest)
			write(w, map[string]string{"error": "authorization_pending"})
			return
		}
		write(w, map[string]string{"id_token": idToken})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
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
		{name: "no oidc on relay", relay: fakeRelay(t, unverified, 0, false, false), wantErr: true},
		{name: "malformed protobuf info", relay: badWire, wantErr: true},
		{name: "no device sign-in", relay: fakeRelay(t, unverified, 0, true, false), wantErr: true},
		{name: "unverified email", relay: fakeRelay(t, unverified, 0, true, true)},
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

func TestIsRelayURL(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{"https://relay.example.com": true, "http://localhost:8080": true, "tailscale": false, "lan": false, "": false} {
		if got := IsRelayURL(v); got != want {
			t.Fatalf("IsRelayURL(%q) = %v, want %v", v, got, want)
		}
	}
}
