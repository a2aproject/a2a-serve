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

package netprovider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

func TestTunnelStreamsWhileProxyFinishesTheRequest(t *testing.T) {
	t.Parallel()
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("reading the request body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		if _, err := io.WriteString(w, "data: first\n\n"); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		if _, err := io.WriteString(w, "data: last\n\n"); err != nil {
			t.Errorf("writing the last event: %v", err)
		}
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(finish) })
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	// The proxy's connection holds the write that ends the request body
	// until the response reached the caller, which is when a slow writer
	// would get there.
	release := make(chan struct{})
	wrote := make(chan error, 1)
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out = r.Out.WithContext(httptrace.WithClientTrace(r.Out.Context(), &httptrace.ClientTrace{
				WroteRequest: func(i httptrace.WroteRequestInfo) { wrote <- i.Err },
			}))
		},
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &heldConn{Conn: c, release: release}, nil
		}},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: connectHandler("token", proxy)}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Server.Serve() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Server.Close() error = %v", err)
		}
	})
	tr := &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		return dialTunnel(ctx, ln.Addr().String(), "token", addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)

	resp, err := (&http.Client{Transport: tr}).Post("http://peer.example/", "application/json", strings.NewReader(`{"jsonrpc":"2.0"}`))
	if err != nil {
		t.Fatalf("Client.Post() error = %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing the response: %v", err)
		}
	}()
	close(release)
	if err := <-wrote; err != nil {
		t.Fatalf("proxy writing the request error = %v, want nil", err)
	}
	finish <- struct{}{}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "data: first\n\ndata: last\n\n" {
		t.Fatalf("reading the response = %q, %v, want both events", body, err)
	}
}

// heldConn holds every write after the first, the request head, until
// release is closed.
type heldConn struct {
	net.Conn
	release chan struct{}
	writes  int
}

func (c *heldConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if c.writes++; c.writes > 1 {
		<-c.release
	}
	return n, err
}

// dialTunnel opens a CONNECT tunnel to addr through the connect socket at
// proxyAddr.
func dialTunnel(ctx context.Context, proxyAddr, token, addr string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Bearer %s\r\n\r\n", addr, addr, token); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Join(fmt.Errorf("CONNECT: %s", resp.Status), c.Close())
	}
	return c, nil
}
