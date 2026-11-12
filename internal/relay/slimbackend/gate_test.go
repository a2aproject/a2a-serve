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
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/relay/control"
)

// echoNode accepts WebSocket upgrades and echoes what it reads, as a
// stand-in for the SLIM node.
func echoNode(t *testing.T) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("Hijack() error = %v", err)
			return
		}
		defer func() {
			if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("closing the node's connection: %v", err)
			}
		}()
		if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}
		if _, err := io.Copy(c, rw); err != nil {
			return
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func newTestGate(t *testing.T) (*service, *httptest.Server) {
	t.Helper()
	s := newTestService(t, &clock{testNow})
	srv := httptest.NewServer(newGate(s, testAud, echoNode(t), func() time.Time { return testNow }))
	t.Cleanup(srv.Close)
	return s, srv
}

// dialNode opens a connection to the node through the gate at addr with
// token, and returns it after the upgrade, or the status the gate answered.
func dialNode(t *testing.T, addr, token string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+NodePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Authorization", "Bearer "+token)
	if err := req.Write(c); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("reading the gate's answer: %v", err)
	}
	return c, br, resp.StatusCode
}

func TestGateLetsMembersReachTheNode(t *testing.T) {
	t.Parallel()
	s, srv := newTestGate(t)
	priv := join(t, s, "coder")
	c, br, status := dialNode(t, srv.Listener.Addr().String(), memberToken(t, priv, "coder"))
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("connecting to the node got status %d, want %d", status, http.StatusSwitchingProtocols)
	}

	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		t.Fatalf("reading through the gate = %q, %v, want the node's echo", got, err)
	}
	if _, err := s.Remove(t.Context(), control.RemoveRequest{Target: "coder"}); err != nil {
		t.Fatal(err)
	}

	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("reading after the member was removed error = %v, want %v", err, io.EOF)
	}
}

func TestGateRefusesTokensThatProveNoMembership(t *testing.T) {
	t.Parallel()
	s, srv := newTestGate(t)
	priv := join(t, s, "coder")
	_, stranger := newKey(t)
	removed := join(t, s, "reviewer")
	if _, err := s.Remove(t.Context(), control.RemoveRequest{Target: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"no token":       "",
		"stranger's key": memberToken(t, stranger, "coder"),
		"removed member": memberToken(t, removed, "reviewer"),
		"another's name": memberToken(t, priv, "reviewer"),
		"garbage":        "garbage",
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, status := dialNode(t, srv.Listener.Addr().String(), token); status != http.StatusUnauthorized {
				t.Fatalf("connecting to the node got status %d, want %d", status, http.StatusUnauthorized)
			}
		})
	}
}

func TestGateEnrollsAndListsMembers(t *testing.T) {
	t.Parallel()
	s, srv := newTestGate(t)
	inv, err := s.Invite(t.Context(), control.InviteRequest{Name: "coder"})
	if err != nil {
		t.Fatal(err)
	}
	pub, priv := newKey(t)
	body, err := json.Marshal(EnrollRequest{Token: inv.Token, Key: pub})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(srv.URL+EnrollPath, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var enrolled EnrollResponse
	if err := errors.Join(json.NewDecoder(resp.Body).Decode(&enrolled), resp.Body.Close()); err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(EnrollResponse{Agent: "coder", SLIMName: "a2a/relay/coder"}, enrolled); diff != "" {
		t.Fatalf("POST %s wrong result (-want +got) diff = %s", EnrollPath, diff)
	}
	req, err := http.NewRequest(http.MethodGet, srv.URL+MembersPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+memberToken(t, priv, "coder"))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var members MembersResponse
	if err := errors.Join(json.NewDecoder(resp.Body).Decode(&members), resp.Body.Close()); err != nil {
		t.Fatal(err)
	}
	want := MembersResponse{Members: []MemberInfo{{Agent: "coder", SLIMName: "a2a/relay/coder", Key: ed25519.PublicKey(pub)}}}
	if diff := cmp.Diff(want, members); diff != "" {
		t.Fatalf("GET %s wrong result (-want +got) diff = %s", MembersPath, diff)
	}
}

func TestGateRefusesMembersListWithoutAToken(t *testing.T) {
	t.Parallel()
	_, srv := newTestGate(t)

	resp, err := http.Get(srv.URL + MembersPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET %s without a token got status %d, want %d", MembersPath, resp.StatusCode, http.StatusUnauthorized)
	}
}
