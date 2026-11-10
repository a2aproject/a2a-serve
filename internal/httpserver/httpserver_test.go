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

package httpserver

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLimitConcurrency(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	h := LimitConcurrency(1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block" {
			entered <- struct{}{}
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}))
	blocked := make(chan int)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/block", nil))
		blocked <- w.Code
	}()
	<-entered

	busy := httptest.NewRecorder()
	h.ServeHTTP(busy, httptest.NewRequest(http.MethodGet, "/", nil))
	close(release)
	if code := <-blocked; code != http.StatusOK {
		t.Fatalf("LimitConcurrency() first request status = %d, want %d", code, http.StatusOK)
	}
	if busy.Code != http.StatusServiceUnavailable {
		t.Fatalf("LimitConcurrency() request over the limit status = %d, want %d", busy.Code, http.StatusServiceUnavailable)
	}
	if busy.Header().Get("Retry-After") == "" {
		t.Fatalf("LimitConcurrency() request over the limit has no Retry-After header")
	}

	after := httptest.NewRecorder()
	h.ServeHTTP(after, httptest.NewRequest(http.MethodGet, "/", nil))
	if after.Code != http.StatusOK {
		t.Fatalf("LimitConcurrency() request after the first ended status = %d, want %d", after.Code, http.StatusOK)
	}
}

func TestKeyedLimiter(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	l := NewKeyedLimiter(2, 1, func(r *http.Request) string { return r.Header.Get("X-Key") })
	h := l.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block" {
			entered <- struct{}{}
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}))
	serve := func(path, key string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Key", key)
		h.ServeHTTP(w, r)
		return w
	}
	blocked := make(chan int, 2)
	for _, key := range []string{"a", "b"} {
		go func() { blocked <- serve("/block", key).Code }()
		<-entered
	}

	sameKey := serve("/", "a")
	full := serve("/", "c")
	close(release)
	for range 2 {
		if code := <-blocked; code != http.StatusOK {
			t.Fatalf("KeyedLimiter.Handler() blocking request status = %d, want %d", code, http.StatusOK)
		}
	}
	after := serve("/", "a")

	if sameKey.Code != http.StatusTooManyRequests || sameKey.Header().Get("Retry-After") == "" {
		t.Fatalf("KeyedLimiter.Handler() request over the key's limit = %d with Retry-After %q, want %d with Retry-After", sameKey.Code, sameKey.Header().Get("Retry-After"), http.StatusTooManyRequests)
	}
	if full.Code != http.StatusServiceUnavailable || full.Header().Get("Retry-After") == "" {
		t.Fatalf("KeyedLimiter.Handler() request over the total limit = %d with Retry-After %q, want %d with Retry-After", full.Code, full.Header().Get("Retry-After"), http.StatusServiceUnavailable)
	}
	for _, w := range []*httptest.ResponseRecorder{sameKey, full} {
		if got := w.Header().Get("Connection"); got != "close" {
			t.Fatalf("KeyedLimiter.Handler() refused request Connection header = %q, want %q", got, "close")
		}
	}
	if after.Code != http.StatusOK {
		t.Fatalf("KeyedLimiter.Handler() request after the key's request ended status = %d, want %d", after.Code, http.StatusOK)
	}
}

func TestSharesHandler(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	shares := NewShares(map[string]int{"member": 1})
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block" {
			entered <- struct{}{}
			<-release
		}
		w.WriteHeader(http.StatusOK)
	})
	serve := func(path, class string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		shares.Handler(class, ok).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	blocked := make(chan int)
	go func() { blocked <- serve("/block", "member").Code }()
	<-entered

	over := serve("/", "member")
	uncapped := serve("/", "owner")
	close(release)
	if code := <-blocked; code != http.StatusOK {
		t.Fatalf("Shares.Handler() blocking request status = %d, want %d", code, http.StatusOK)
	}
	after := serve("/", "member")

	if over.Code != http.StatusServiceUnavailable || over.Header().Get("Retry-After") != "1" || over.Header().Get("Connection") != "close" {
		t.Fatalf("Shares.Handler() request over the share = %d with headers %v, want %d with Retry-After 1 and Connection close", over.Code, over.Header(), http.StatusServiceUnavailable)
	}
	if !strings.Contains(over.Body.String(), BusyMessage) {
		t.Fatalf("Shares.Handler() request over the share body = %s, want it to contain %q", over.Body, BusyMessage)
	}
	if uncapped.Code != http.StatusOK {
		t.Fatalf("Shares.Handler() request of a class without a share status = %d, want %d", uncapped.Code, http.StatusOK)
	}
	if after.Code != http.StatusOK {
		t.Fatalf("Shares.Handler() request after the share was given back status = %d, want %d", after.Code, http.StatusOK)
	}
}

func TestSharesLeaveTheOwnerRequests(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var held sync.WaitGroup
	shares := NewShares(map[string]int{"unauthenticated": MaxUnauthenticated, "member": MaxMemberRequests, "guest": MaxGuestRequests})
	h := LimitConcurrency(MaxConcurrent, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shares.Handler(r.Header.Get("X-Class"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/block" {
				held.Done()
				<-release
			}
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(w, r)
	}))
	serve := func(path, class string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Class", class)
		h.ServeHTTP(w, r)
		return w.Code
	}
	var done sync.WaitGroup
	for class, n := range map[string]int{"unauthenticated": MaxUnauthenticated, "member": MaxMemberRequests, "guest": MaxGuestRequests} {
		held.Add(n)
		for range n {
			done.Go(func() { serve("/block", class) })
		}
	}
	held.Wait()
	defer done.Wait()
	defer close(release)

	for _, class := range []string{"unauthenticated", "member", "guest"} {
		if got := serve("/", class); got != http.StatusServiceUnavailable {
			t.Fatalf("request of %s over its share status = %d, want %d", class, got, http.StatusServiceUnavailable)
		}
	}
	if got := serve("/", "owner"); got != http.StatusOK {
		t.Fatalf("request of the owner while the other shares are in use status = %d, want %d", got, http.StatusOK)
	}
}

func TestNewLimitsHeaders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		padding int
		want    int
	}{
		{name: "small headers", padding: 8 << 10, want: http.StatusOK},
		{name: "headers over the limit", padding: MaxHeaderBytes * 2, want: http.StatusRequestHeaderFieldsTooLarge},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Padding", strings.Repeat("a", tc.padding))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET error = %v", err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want {
				t.Fatalf("GET status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestNewWithConnContext(t *testing.T) {
	t.Parallel()
	type key struct{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, _ := r.Context().Value(key{}).(string); got != "set" {
			w.WriteHeader(http.StatusTeapot)
		}
	}), WithConnContext(func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, key{}, "set")
	}))
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	resp, err := http.Get("http://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want %d: the request context lacks the value of WithConnContext", resp.StatusCode, http.StatusOK)
	}
}

func TestKeyedLimiterLimitsUnkeyedRequestsOnlyInTotal(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	l := NewKeyedLimiter(2, 1, func(r *http.Request) string { return "" })
	h := l.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block" {
			entered <- struct{}{}
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}))
	serve := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	blocked := make(chan int, 2)
	for i := range 2 {
		go func() { blocked <- serve("/block").Code }()
		select {
		case <-entered:
		case code := <-blocked:
			t.Fatalf("KeyedLimiter.Handler() unkeyed request %d status = %d, want it served", i+1, code)
		}
	}

	full := serve("/")

	if full.Code != http.StatusServiceUnavailable {
		t.Fatalf("KeyedLimiter.Handler() unkeyed request over the total limit status = %d, want %d", full.Code, http.StatusServiceUnavailable)
	}
}

// rawGet sends a GET of path on conn.
func rawGet(t *testing.T, conn net.Conn, path string) {
	t.Helper()
	if _, err := io.WriteString(conn, "GET "+path+" HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
}

func TestWriteTimeout(t *testing.T) {
	t.Parallel()
	ended := make(chan error, 1)
	srv := httptest.NewServer(WriteTimeout(50*time.Millisecond, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 64<<10)
		for {
			if _, err := w.Write(chunk); err != nil {
				ended <- err
				return
			}
		}
	})))
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("closing the connection: %v", err)
		}
	})

	rawGet(t, conn, "/")

	select {
	case err := <-ended:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("WriteTimeout() write to a client that doesn't read error = %v, want %v", err, os.ErrDeadlineExceeded)
		}
	case <-time.After(time.Minute):
		t.Fatalf("WriteTimeout() response to a client that doesn't read still written after a minute")
	}
}

func TestWriteTimeoutEndsWithTheResponse(t *testing.T) {
	t.Parallel()
	const timeout = 10 * time.Millisecond
	srv := httptest.NewServer(WriteTimeout(timeout, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "ok"); err != nil {
			t.Errorf("writing: %v", err)
		}
	})))
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("closing the connection: %v", err)
		}
	})
	br := bufio.NewReader(conn)
	read := func() string {
		t.Helper()
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("reading the response: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading the response body: %v", err)
		}
		return string(body)
	}
	rawGet(t, conn, "/")
	read()
	time.Sleep(5 * timeout)

	rawGet(t, conn, "/")
	got := read()

	if got != "ok" {
		t.Fatalf("WriteTimeout() second response on the connection = %q, want %q", got, "ok")
	}
}
