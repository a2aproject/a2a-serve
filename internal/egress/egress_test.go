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

package egress

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestPublic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ip   string
		want bool
	}{
		{ip: "8.8.8.8", want: true},
		{ip: "2001:4860:4860::8888", want: true},
		{ip: "127.0.0.1"},
		{ip: "::1"},
		{ip: "10.1.2.3"},
		{ip: "172.16.0.1"},
		{ip: "192.168.1.1"},
		{ip: "169.254.169.254"},
		{ip: "fe80::1"},
		{ip: "100.101.102.103"},
		{ip: "fd7a:115c:a1e0::1"},
		{ip: "0.0.0.0"},
		{ip: "0.1.2.3"},
		{ip: "::"},
		{ip: "224.0.0.1"},
		{ip: "::ffff:127.0.0.1"},
		{ip: "::ffff:8.8.8.8", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.ip, func(t *testing.T) {
			t.Parallel()
			if got := Public(netip.MustParseAddr(tc.ip)); got != tc.want {
				t.Fatalf("Public(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

// servers starts two loopback servers: one that the test treats as public,
// and one that it treats as private.
func servers(t *testing.T, public http.Handler) (pub, priv *httptest.Server, isPublic func(netip.AddrPort) bool) {
	t.Helper()
	pub = httptest.NewServer(public)
	t.Cleanup(pub.Close)
	priv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "private"); err != nil {
			t.Errorf("writing the private response: %v", err)
		}
	}))
	t.Cleanup(priv.Close)
	pubAddr := netip.MustParseAddrPort(pub.Listener.Addr().String())
	return pub, priv, func(ap netip.AddrPort) bool { return ap == pubAddr }
}

func get(t *testing.T, c *http.Client, u string) (string, error) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing the response: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func TestTransportKeepsPublicOriginOutOfPrivateNetworks(t *testing.T) {
	t.Parallel()
	var privURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "public"); err != nil {
			t.Errorf("writing the public response: %v", err)
		}
	})
	mux.HandleFunc("/to-private", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, privURL, http.StatusFound)
	})
	pub, priv, isPublic := servers(t, mux)
	privURL = priv.URL
	privByName := strings.Replace(priv.URL, "127.0.0.1", "localhost", 1)
	tests := []struct {
		name string
		// reachOrigin gets the origin first, which decides whether it is public.
		reachOrigin bool
		origin      string
		get         string
		want        string
		wantErr     error
	}{
		{name: "public origin", origin: pub.URL, get: pub.URL, want: "public"},
		{name: "redirect from a public origin to a private address", reachOrigin: true, origin: pub.URL, get: pub.URL + "/to-private", wantErr: ErrPrivateAddress},
		{name: "private address from a public origin", reachOrigin: true, origin: pub.URL, get: priv.URL, wantErr: ErrPrivateAddress},
		{name: "name that resolves to a private address from a public origin", reachOrigin: true, origin: pub.URL, get: privByName, wantErr: ErrPrivateAddress},
		{name: "private address before the origin was reached", origin: pub.URL, get: priv.URL, wantErr: ErrPrivateAddress},
		{name: "private origin", origin: priv.URL, get: priv.URL, want: "private"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTransport(tc.origin, isPublic)
			t.Cleanup(tr.CloseIdleConnections)
			c := &http.Client{Transport: tr}
			if tc.reachOrigin {
				if _, err := get(t, c, tc.origin); err != nil {
					t.Fatalf("Get(%s) error = %v", tc.origin, err)
				}
			}

			got, err := get(t, c, tc.get)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Get(%s) error = %v, want %v", tc.get, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("Get(%s) = %q, want %q", tc.get, got, tc.want)
			}
		})
	}
}

func TestTransportOfPrivateOriginFollowsRedirectsIntoPrivateNetworks(t *testing.T) {
	t.Parallel()
	_, priv, isPublic := servers(t, http.NotFoundHandler())
	mux := http.NewServeMux()
	origin := httptest.NewServer(mux)
	t.Cleanup(origin.Close)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, priv.URL, http.StatusFound)
	})
	tr := newTransport(origin.URL, isPublic)
	t.Cleanup(tr.CloseIdleConnections)

	got, err := get(t, &http.Client{Transport: tr}, origin.URL)

	if err != nil {
		t.Fatalf("Get(%s) error = %v", origin.URL, err)
	}
	if got != "private" {
		t.Fatalf("Get(%s) = %q, want %q", origin.URL, got, "private")
	}
}

func TestCheckRedirect(t *testing.T) {
	t.Parallel()
	req := func(u string) *http.Request {
		t.Helper()
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatalf("url.Parse(%q) error = %v", u, err)
		}
		return &http.Request{URL: parsed}
	}
	tests := []struct {
		name    string
		via     []string
		to      string
		wantErr bool
	}{
		{name: "https to https", via: []string{"https://a.example/hook"}, to: "https://b.example/hook"},
		{name: "http to https", via: []string{"http://a.example/hook"}, to: "https://a.example/hook"},
		{name: "http to http", via: []string{"http://a.example/hook"}, to: "http://b.example/hook"},
		{name: "https to http", via: []string{"https://a.example/hook"}, to: "http://a.example/hook", wantErr: true},
		{name: "https to http after a hop", via: []string{"http://a.example/hook", "https://a.example/hook"}, to: "http://b.example/hook", wantErr: true},
		{name: "too many redirects", via: strings.Split(strings.Repeat("https://a.example/hook ", 10), " ")[:10], to: "https://a.example/hook", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var via []*http.Request
			for _, u := range tc.via {
				via = append(via, req(u))
			}
			if err := CheckRedirect(req(tc.to), via); (err != nil) != tc.wantErr {
				t.Fatalf("CheckRedirect(%s) error = %v, want error %v", tc.to, err, tc.wantErr)
			}
		})
	}
}

func TestLimitResponses(t *testing.T) {
	t.Parallel()
	const limit = 1 << 10
	tests := []struct {
		name        string
		contentType string
		size        int
		chunked     bool
		wantErr     error
	}{
		{name: "at the limit", contentType: "application/json", size: limit},
		{name: "over the limit", contentType: "application/json", size: limit + 1, wantErr: ErrResponseTooLarge},
		{name: "over the limit without a length", contentType: "application/json", size: limit + 1, chunked: true, wantErr: ErrResponseTooLarge},
		{name: "event stream over the limit", contentType: "text/event-stream; charset=utf-8", size: 4 * limit, chunked: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				body := strings.Repeat("x", tc.size)
				if tc.chunked {
					if _, err := io.WriteString(w, body[:1]); err != nil {
						t.Errorf("writing the response: %v", err)
					}
					w.(http.Flusher).Flush()
					body = body[1:]
				}
				if _, err := io.WriteString(w, body); err != nil {
					t.Errorf("writing the response: %v", err)
				}
			}))
			t.Cleanup(srv.Close)
			c := &http.Client{Transport: LimitResponses(srv.Client().Transport, limit)}

			got, err := get(t, c, srv.URL)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Get() error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && len(got) != tc.size {
				t.Fatalf("Get() read %d bytes, want %d", len(got), tc.size)
			}
		})
	}
}
