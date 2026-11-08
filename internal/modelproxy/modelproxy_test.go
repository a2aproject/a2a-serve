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

package modelproxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// upstreamRequest is what a provider got.
type upstreamRequest struct {
	URL     string
	Headers map[string]string
	Body    string
}

// fakeProviders serves every provider: a transport sends the requests of
// the proxy to handler and records them.
type fakeProviders struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	got     []upstreamRequest
	handler http.HandlerFunc
}

func newFakeProviders(t *testing.T, handler http.HandlerFunc) *fakeProviders {
	t.Helper()
	f := &fakeProviders{t: t, handler: handler}
	if f.handler == nil {
		f.handler = func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.WriteString(w, `{"ok":true}`); err != nil {
				t.Errorf("writing the answer: %v", err)
			}
		}
	}
	f.srv = httptest.NewServer(f.handler)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeProviders) RoundTrip(r *http.Request) (*http.Response, error) {
	body := ""
	if r.Body != nil {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		body = string(data)
		r.Body = io.NopCloser(strings.NewReader(body))
	}
	headers := map[string]string{}
	for _, h := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie", "Anthropic-Version"} {
		if v := r.Header.Get(h); v != "" {
			headers[h] = v
		}
	}
	f.mu.Lock()
	f.got = append(f.got, upstreamRequest{URL: r.URL.String(), Headers: headers, Body: body})
	f.mu.Unlock()
	target, err := url.Parse(f.srv.URL)
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
	return f.srv.Client().Transport.RoundTrip(r)
}

func (f *fakeProviders) requests() []upstreamRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]upstreamRequest(nil), f.got...)
}

// newTestProxy returns a proxy whose process "agent" has the keys of
// openai, anthropic, google and openrouter, and whose other requests come
// from no process.
func newTestProxy(t *testing.T, f *fakeProviders) http.Handler {
	t.Helper()
	keys := map[string]string{"openai": "sk-openai", "anthropic": "sk-ant", "google": "goog-key", "openrouter": "sk-or"}
	return New(Options{
		Key: func(process string, p Provider) (string, bool) {
			k, ok := keys[p.Name]
			return k, ok && process == "agent"
		},
		Process: func(r *http.Request) (string, bool) {
			p := r.Header.Get("Test-Process")
			return p, p != ""
		},
		Transport: f,
	})
}

func TestProxyForwardsWithKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		path    string
		headers map[string]string
		want    upstreamRequest
	}{
		{
			name:    "openai",
			path:    "/_model/openai/v1/responses",
			headers: map[string]string{"Authorization": "Bearer placeholder", "Cookie": "a=b"},
			want:    upstreamRequest{URL: "https://api.openai.com/v1/responses", Headers: map[string]string{"Authorization": "Bearer sk-openai"}, Body: `{"m":1}`},
		},
		{
			name:    "anthropic",
			path:    "/_model/anthropic/v1/messages",
			headers: map[string]string{"X-Api-Key": "placeholder", "Anthropic-Version": "2023-06-01"},
			want:    upstreamRequest{URL: "https://api.anthropic.com/v1/messages", Headers: map[string]string{"X-Api-Key": "sk-ant", "Anthropic-Version": "2023-06-01"}, Body: `{"m":1}`},
		},
		{
			name:    "google with the key in the query",
			path:    "/_model/google/v1beta/models/gemini:streamGenerateContent?alt=sse&key=placeholder",
			headers: map[string]string{"X-Goog-Api-Key": "placeholder", "Authorization": "Bearer placeholder"},
			want:    upstreamRequest{URL: "https://generativelanguage.googleapis.com/v1beta/models/gemini:streamGenerateContent?alt=sse", Headers: map[string]string{"X-Goog-Api-Key": "goog-key"}, Body: `{"m":1}`},
		},
		{
			name:    "openrouter",
			path:    "/_model/openrouter/api/v1/chat/completions",
			headers: map[string]string{"Authorization": "Bearer placeholder"},
			want:    upstreamRequest{URL: "https://openrouter.ai/api/v1/chat/completions", Headers: map[string]string{"Authorization": "Bearer sk-or"}, Body: `{"m":1}`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeProviders(t, nil)
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"m":1}`))
			r.Header.Set("Test-Process", "agent")
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			newTestProxy(t, f).ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
				t.Fatalf("ServeHTTP() = %d %q, want 200 with the answer of the provider", w.Code, w.Body)
			}
			if diff := cmp.Diff([]upstreamRequest{tc.want}, f.requests()); diff != "" {
				t.Fatalf("requests to the provider wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestProxyRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		method   string
		path     string
		process  string
		wantCode int
	}{
		{name: "no process", path: "/_model/openai/v1/responses", wantCode: http.StatusUnauthorized},
		{name: "unknown provider", path: "/_model/evil/v1/x", process: "agent", wantCode: http.StatusNotFound},
		{name: "path outside the API", path: "/_model/openai/dashboard", process: "agent", wantCode: http.StatusNotFound},
		{name: "path that leaves the API", path: "/_model/openai/v1/models/../files", method: http.MethodGet, process: "agent", wantCode: http.StatusNotFound},
		{name: "encoded path that leaves the API", path: "/_model/google/v1beta/models/x%2F..%2F..%2FcachedContents", process: "agent", wantCode: http.StatusNotFound},
		{name: "files of the account", method: http.MethodGet, path: "/_model/openai/v1/files", process: "agent", wantCode: http.StatusNotFound},
		{name: "stored chat completions", method: http.MethodGet, path: "/_model/openai/v1/chat/completions", process: "agent", wantCode: http.StatusNotFound},
		{name: "batches", path: "/_model/anthropic/v1/messages/batches", process: "agent", wantCode: http.StatusNotFound},
		{name: "method", method: http.MethodDelete, path: "/_model/openai/v1/models/m", process: "agent", wantCode: http.StatusNotFound},
		{name: "process without the key", path: "/_model/openai/v1/responses", process: "other", wantCode: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeProviders(t, nil)
			method := tc.method
			if method == "" {
				method = http.MethodPost
			}
			r := httptest.NewRequest(method, tc.path, strings.NewReader("{}"))
			if tc.process != "" {
				r.Header.Set("Test-Process", tc.process)
			}
			w := httptest.NewRecorder()
			newTestProxy(t, f).ServeHTTP(w, r)
			if w.Code != tc.wantCode {
				t.Fatalf("ServeHTTP() = %d %q, want %d", w.Code, w.Body, tc.wantCode)
			}
			if got := f.requests(); len(got) > 0 {
				t.Fatalf("requests to the provider = %v, want none", got)
			}
		})
	}
}

func TestProxyRefusesLargeBody(t *testing.T) {
	t.Parallel()
	f := newFakeProviders(t, nil)
	r := httptest.NewRequest(http.MethodPost, "/_model/openai/v1/responses", strings.NewReader(strings.Repeat("a", 32<<20+1)))
	r.Header.Set("Test-Process", "agent")
	w := httptest.NewRecorder()
	newTestProxy(t, f).ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("ServeHTTP() = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestProxyStreams(t *testing.T) {
	t.Parallel()
	next := make(chan struct{})
	f := newFakeProviders(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{"one", "two"} {
			if _, err := io.WriteString(w, "data: "+ev+"\n\n"); err != nil {
				t.Errorf("writing an event: %v", err)
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-next:
			case <-r.Context().Done():
				return
			}
		}
	})
	srv := httptest.NewServer(newTestProxy(t, f))
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/_model/anthropic/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Test-Process", "agent")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	br := bufio.NewReader(resp.Body)
	for _, want := range []string{"data: one\n", "\n", "data: two\n"} {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString() error = %v", err)
		}
		if line != want {
			t.Fatalf("streamed line = %q, want %q", line, want)
		}
		if want == "\n" {
			next <- struct{}{}
		}
	}
	close(next)
}

func TestProxyLimitsRequestsOfAProcess(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{}, maxPerProcess)
	f := newFakeProviders(t, func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	h := newTestProxy(t, f)
	var wg sync.WaitGroup
	for range maxPerProcess {
		wg.Go(func() {
			r := httptest.NewRequest(http.MethodPost, "/_model/openai/v1/responses", strings.NewReader("{}"))
			r.Header.Set("Test-Process", "agent")
			h.ServeHTTP(httptest.NewRecorder(), r)
		})
	}
	for range maxPerProcess {
		<-started
	}
	r := httptest.NewRequest(http.MethodPost, "/_model/openai/v1/responses", strings.NewReader("{}"))
	r.Header.Set("Test-Process", "agent")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	close(release)
	wg.Wait()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("ServeHTTP() = %d, want %d", w.Code, http.StatusTooManyRequests)
	}
}

func TestCredential(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		target  string
		headers map[string]string
		want    string
	}{
		{name: "bearer", headers: map[string]string{"Authorization": "Bearer k1"}, want: "k1"},
		{name: "x-api-key", headers: map[string]string{"X-Api-Key": "k2"}, want: "k2"},
		{name: "x-goog-api-key", headers: map[string]string{"X-Goog-Api-Key": "k3"}, want: "k3"},
		{name: "query", target: "/?key=k4", want: "k4"},
		{name: "basic", headers: map[string]string{"Authorization": "Basic a2V5"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := tc.target
			if target == "" {
				target = "/"
			}
			r := httptest.NewRequest(http.MethodGet, target, nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := Credential(r); got != tc.want {
				t.Fatalf("Credential() = %q, want %q", got, tc.want)
			}
		})
	}
}
