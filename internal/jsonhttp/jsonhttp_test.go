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

package jsonhttp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestErrorMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "api error", body: `{"error":"no such agent"}`, want: "no such agent"},
		{name: "empty body", body: "", want: "404 Not Found"},
		{name: "text body", body: "page not found\n", want: "404 Not Found: page not found"},
		{name: "long body", body: strings.Repeat("x", 400), want: "404 Not Found: " + strings.Repeat("x", 300) + "…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ErrorMessage("404 Not Found", []byte(tc.body)); got != tc.want {
				t.Fatalf("ErrorMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRequireJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		method string
		header http.Header
		want   int
	}{
		{name: "GET", method: http.MethodGet, want: http.StatusOK},
		{name: "JSON", method: http.MethodPost, header: http.Header{"Content-Type": {"application/json; charset=utf-8"}}, want: http.StatusOK},
		{name: "A2A JSON", method: http.MethodPut, header: http.Header{"Content-Type": {"application/a2a+json"}}, want: http.StatusOK},
		{name: "A2A-Version", method: http.MethodDelete, header: http.Header{"A2a-Version": {"1.0"}}, want: http.StatusOK},
		{name: "form", method: http.MethodPost, header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, want: http.StatusUnsupportedMediaType},
		{name: "text", method: http.MethodPost, header: http.Header{"Content-Type": {"text/plain"}}, want: http.StatusUnsupportedMediaType},
		{name: "no content type", method: http.MethodPost, want: http.StatusUnsupportedMediaType},
	}
	h := RequireJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(tc.method, "/", nil)
			r.Header = tc.header
			if r.Header == nil {
				r.Header = http.Header{}
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("RequireJSON() status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestLimitBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		limit         int64
		size          int64
		contentLength int64
		want          int
	}{
		{name: "at the limit", size: MaxBody, contentLength: MaxBody, want: http.StatusOK},
		{name: "declared over the limit", size: 1, contentLength: MaxBody + 1, want: http.StatusRequestEntityTooLarge},
		{name: "chunked over the limit", size: MaxBody + 1, contentLength: -1, want: http.StatusRequestEntityTooLarge},
		{name: "at a smaller limit", limit: 10, size: 10, contentLength: 10, want: http.StatusOK},
		{name: "declared over a smaller limit", limit: 10, size: 1, contentLength: 11, want: http.StatusRequestEntityTooLarge},
		{name: "chunked over a smaller limit", limit: 10, size: 11, contentLength: -1, want: http.StatusRequestEntityTooLarge},
	}
	read := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			Error(w, http.StatusRequestEntityTooLarge, err.Error())
		}
	})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := LimitBody(read)
			if tc.limit != 0 {
				h = LimitBodyTo(tc.limit, read)
			}
			r := httptest.NewRequest(http.MethodPost, "/", io.LimitReader(zeros{}, tc.size))
			r.ContentLength = tc.contentLength
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("LimitBody() status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestLimitDeadline(t *testing.T) {
	t.Parallel()
	const timeout = 300 * time.Millisecond
	tests := []struct {
		name  string
		limit Limit
		// pauses are the waits before each byte of a 4-byte body.
		pauses  []time.Duration
		wantErr bool
	}{
		{name: "fast body under a total timeout", limit: Limit{Max: 4, Total: timeout}, pauses: []time.Duration{0, 0, 0, 0}},
		{name: "slow body over a total timeout", limit: Limit{Max: 4, Total: timeout}, pauses: []time.Duration{0, timeout / 2, timeout / 2, timeout / 2}, wantErr: true},
		{name: "stalled body over a total timeout", limit: Limit{Max: 4, Total: timeout}, pauses: []time.Duration{0, 10 * timeout}, wantErr: true},
		{name: "slow body making progress under an idle timeout", limit: Limit{Max: 4, Idle: timeout}, pauses: []time.Duration{0, timeout / 2, timeout / 2, timeout / 2}},
		{name: "stalled body over an idle timeout", limit: Limit{Max: 4, Idle: timeout}, pauses: []time.Duration{0, 10 * timeout}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			readErr := make(chan error, 1)
			srv := httptest.NewServer(tc.limit.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.Copy(io.Discard, r.Body)
				readErr <- err
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
			if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 4\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			go func() {
				for _, p := range tc.pauses {
					time.Sleep(p)
					if _, err := conn.Write([]byte{'x'}); err != nil {
						return
					}
				}
			}()

			select {
			case err := <-readErr:
				if gotErr := err != nil; gotErr != tc.wantErr {
					t.Fatalf("Limit.Handler() body read error = %v, want error %t", err, tc.wantErr)
				}
				if tc.wantErr && !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("Limit.Handler() body read error = %v, want %v", err, os.ErrDeadlineExceeded)
				}
			case <-time.After(20 * timeout):
				t.Fatalf("Limit.Handler() body read didn't end in %v", 20*timeout)
			}
		})
	}
}

func TestLimitClosesStalledConnection(t *testing.T) {
	t.Parallel()
	const timeout = 100 * time.Millisecond
	for _, limit := range []Limit{{Max: 1 << 10, Total: timeout}, {Max: 1 << 10, Idle: timeout}} {
		t.Run(fmt.Sprintf("total %v idle %v", limit.Total, limit.Idle), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(limit.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					Error(w, http.StatusBadRequest, err.Error())
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
			if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 100\r\n\r\n{"); err != nil {
				t.Fatal(err)
			}
			if err := conn.SetReadDeadline(time.Now().Add(20 * timeout)); err != nil {
				t.Fatal(err)
			}

			br := bufio.NewReader(conn)
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("reading the response error = %v", err)
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatalf("reading the response body error = %v", err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("Limit.Handler() status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
			if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
				t.Fatalf("reading after the response error = %v, want %v", err, io.EOF)
			}
		})
	}
}

func TestLimitKeepsStreamAfterBody(t *testing.T) {
	t.Parallel()
	const timeout = 100 * time.Millisecond
	for _, limit := range []Limit{{Max: 1 << 10, Total: timeout}, {Max: 1 << 10, Idle: timeout}} {
		t.Run(fmt.Sprintf("total %v idle %v", limit.Total, limit.Idle), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(limit.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					Error(w, http.StatusBadRequest, err.Error())
					return
				}
				rc := http.NewResponseController(w)
				for i := range 5 {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(timeout):
					}
					if _, err := fmt.Fprintf(w, "data: %d\n\n", i); err != nil {
						return
					}
					if err := rc.Flush(); err != nil {
						return
					}
				}
			})))
			t.Cleanup(srv.Close)

			resp, err := srv.Client().Post(srv.URL, "application/json", strings.NewReader(`{"stream":true}`))
			if err != nil {
				t.Fatalf("POST error = %v", err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading the stream error = %v", err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}

			if got := strings.Count(string(body), "data: "); got != 5 {
				t.Fatalf("Limit.Handler() stream events = %d, want 5: %q", got, body)
			}
		})
	}
}
