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
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
