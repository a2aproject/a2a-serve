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

package agentsrv

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
	"github.com/a2aproject/a2a-serve/internal/network"
)

var guestPrincipal = auth.Principal{ID: auth.GuestID, Role: auth.RoleGuest, Identity: auth.Identity{Network: network.ListenerPublic}}

// guestBodyRequest returns a request from p with a body of n bytes, which
// declares its length unless chunked is set.
func guestBodyRequest(p auth.Principal, path string, n int, chunked bool) *http.Request {
	var body io.Reader = bytes.NewReader(make([]byte, n))
	if chunked {
		body = io.MultiReader(body)
	}
	r := httptest.NewRequest(http.MethodPost, path, body)
	if chunked {
		r.ContentLength = -1
	}
	return r.WithContext(auth.WithPrincipal(r.Context(), p))
}

// holdGuestBody serves a request with a body of n bytes through bodies,
// and keeps its handler running until the returned function is called.
func holdGuestBody(t *testing.T, bodies *guestBodies, n int) func() {
	t.Helper()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := bodies.handler(protocolREST, protocolREST.readBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	})))
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), guestBodyRequest(guestPrincipal, "/message:send", n, false))
	}()
	<-entered
	return sync.OnceFunc(func() {
		close(release)
		<-done
	})
}

func TestGuestBodies(t *testing.T) {
	t.Parallel()
	const held = 3 << 20
	tests := []struct {
		name       string
		caller     auth.Principal
		size       int
		chunked    bool
		wantStatus int
	}{
		{name: "guest body that fits", caller: guestPrincipal, size: 1 << 20, wantStatus: http.StatusOK},
		{name: "guest body over the budget", caller: guestPrincipal, size: 2 << 20, wantStatus: http.StatusServiceUnavailable},
		{name: "chunked guest body that fits", caller: guestPrincipal, size: 1 << 20, chunked: true, wantStatus: http.StatusOK},
		{name: "chunked guest body over the budget", caller: guestPrincipal, size: 1<<20 + 1, chunked: true, wantStatus: http.StatusServiceUnavailable},
		{name: "member body over the budget", caller: auth.Principal{ID: "pair:bob", Role: auth.RoleMember}, size: 8 << 20, wantStatus: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bodies := newGuestBodies(4<<20, 50*time.Millisecond)
			release := holdGuestBody(t, bodies, held)
			defer release()
			var got []byte
			h := bodies.handler(protocolREST, protocolREST.readBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("reading the body error = %v", err)
				}
				got = body
			})))
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, guestBodyRequest(tc.caller, "/message:send", tc.size, tc.chunked))

			if rec.Code != tc.wantStatus {
				t.Fatalf("POST status = %d (%s), want %d", rec.Code, rec.Body, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK && len(got) != tc.size {
				t.Fatalf("handler read %d bytes, want %d", len(got), tc.size)
			}
			if tc.wantStatus != http.StatusServiceUnavailable {
				return
			}
			if got := rec.Header().Get("Retry-After"); got != "1" {
				t.Fatalf("Retry-After = %q, want %q", got, "1")
			}
			if !strings.Contains(rec.Body.String(), errGuestsBusy.Error()) {
				t.Fatalf("POST body = %s, want it to contain %q", rec.Body, errGuestsBusy)
			}
		})
	}
}

func TestGuestBodiesWaitForRelease(t *testing.T) {
	t.Parallel()
	bodies := newGuestBodies(1<<20, 10*time.Second)
	release := holdGuestBody(t, bodies, 1<<20)
	h := bodies.handler(protocolREST, protocolREST.readBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	time.AfterFunc(50*time.Millisecond, release)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, guestBodyRequest(guestPrincipal, "/message:send", 1<<20, false))

	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d (%s), want %d", rec.Code, rec.Body, http.StatusOK)
	}
}

func TestGuestBodiesPassEmptyBodiesPastWaitingOnes(t *testing.T) {
	t.Parallel()
	bodies := newGuestBodies(1<<20, 10*time.Second)
	release := holdGuestBody(t, bodies, 1<<20)
	defer release()
	waiting := make(chan struct{})
	go func() {
		defer close(waiting)
		bodies.handler(protocolREST, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(httptest.NewRecorder(), guestBodyRequest(guestPrincipal, "/message:send", 1<<20, false))
	}()
	for bodies.sem.TryAcquire(0) {
		time.Sleep(time.Millisecond)
	}
	h := jsonhttp.LimitBody(bodies.handler(protocolREST, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	rec := httptest.NewRecorder()
	start := time.Now()

	h.ServeHTTP(rec, guestBodyRequest(guestPrincipal, "/tasks/t1:cancel", 0, false))

	if rec.Code != http.StatusOK || time.Since(start) > 5*time.Second {
		t.Fatalf("POST without a body = %d after %s, want %d at once", rec.Code, time.Since(start), http.StatusOK)
	}
	release()
	<-waiting
}
