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

package host_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/netprovider"
	"github.com/a2aproject/a2a-serve/netprovider/host"
)

var loopnetBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "loopnet")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	loopnetBin = filepath.Join(dir, "loopnet")
	out, err := exec.Command("go", "build", "-o", loopnetBin, "github.com/a2aproject/a2a-serve/netprovider/internal/loopnet").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "building loopnet: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

type agent struct {
	p        *host.Process
	ep       *host.Endpoints
	notices  chan string
	stateDir string
	runDir   string
}

func runDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "np")
	if err != nil {
		t.Fatalf("os.MkdirTemp() error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("os.RemoveAll() error = %v", err)
		}
	})
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("os.Chmod() error = %v", err)
	}
	return dir
}

func start(t *testing.T) *agent {
	t.Helper()
	a := &agent{notices: make(chan string, 10), stateDir: t.TempDir(), runDir: runDir(t)}
	p, err := host.Start(host.Config{
		Path: loopnetBin, RunDir: a.runDir, Env: host.Environ(os.Environ()), Stderr: io.Discard,
		OnNotice: func(n netprovider.Notice) { a.notices <- n.Message },
	})
	if err != nil {
		t.Fatalf("host.Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("Process.Close() error = %v", err)
		}
	})
	a.p = p
	return a
}

func join(t *testing.T, network, name string, public bool) *agent {
	t.Helper()
	a := start(t)
	ctx := t.Context()
	if _, err := a.p.Join(ctx, netprovider.JoinRequest{Agent: name, URL: network, StateDir: a.stateDir, Private: true, Public: public}); err != nil {
		t.Fatalf("Process.Join() error = %v", err)
	}
	ep, err := a.p.Listen(ctx, true, public)
	if err != nil {
		t.Fatalf("Process.Listen() error = %v", err)
	}
	a.ep = ep
	return a
}

// serveWhoAmI answers each request with who the network says calls.
func serveWhoAmI(t *testing.T, a *agent) {
	t.Helper()
	for _, l := range []*host.Listener{a.ep.Private, a.ep.Public} {
		if l == nil {
			continue
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := l.Caller(r.RemoteAddr)
			if !ok {
				http.Error(w, "no caller", http.StatusInternalServerError)
				return
			}
			if _, err := fmt.Fprintf(w, "%s %s|%s|%s %s", r.URL.Path, c.Node, c.Email, c.ClientKey, r.Host); err != nil {
				t.Errorf("writing: %v", err)
			}
		})}
		go func() {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("Server.Serve() error = %v", err)
			}
		}()
		t.Cleanup(func() {
			if err := srv.Close(); err != nil {
				t.Errorf("Server.Close() error = %v", err)
			}
		})
	}
}

func get(t *testing.T, c *http.Client, u string) string {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", u, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("closing the body: %v", err)
		}
	}()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return string(b)
}

func TestLoopnet(t *testing.T) {
	t.Parallel()
	network := "loopnet://" + runDir(t) + "?owner=anna@example.com"
	coder := join(t, network, "coder", true)
	reviewer := join(t, network, "reviewer", false)
	serveWhoAmI(t, coder)

	if diff := cmp.Diff("http://loopnet.local/coder", coder.ep.PrivateURL); diff != "" {
		t.Fatalf("PrivateURL wrong result (-want +got) diff = %s", diff)
	}
	select {
	case n := <-coder.notices:
		if !strings.Contains(n, "anna@example.com") {
			t.Fatalf("notice = %q, want the owner", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no notice after join")
	}

	client := &http.Client{Transport: reviewer.p.Transport(), Timeout: 10 * time.Second}
	if got, want := get(t, client, "http://loopnet.local/coder/whoami"), "/whoami loop:reviewer|anna@example.com|loop:reviewer loopnet.local"; got != want {
		t.Fatalf("private GET = %q, want %q", got, want)
	}
	public := &http.Client{Timeout: 10 * time.Second}
	if got, want := get(t, public, coder.ep.PublicURL+"/x"), "/x ||ip:127.0.0.1 "+strings.TrimPrefix(coder.ep.PublicURL, "http://"); got != want {
		t.Fatalf("public GET = %q, want %q", got, want)
	}

	peers, err := reviewer.p.Peers(t.Context())
	if err != nil {
		t.Fatalf("Process.Peers() error = %v", err)
	}
	want := []netprovider.PeerInfo{{Name: "coder", URL: "http://loopnet.local/coder", Node: "loop:coder"}}
	if diff := cmp.Diff(want, peers); diff != "" {
		t.Fatalf("Process.Peers() wrong result (-want +got) diff = %s", diff)
	}
	if _, err := reviewer.p.Member(t.Context(), "loop:nobody"); !errors.Is(err, netprovider.ErrNotMember) {
		t.Fatalf("Process.Member(loop:nobody) error = %v, want %v", err, netprovider.ErrNotMember)
	}
}

func TestJoinTokenError(t *testing.T) {
	t.Parallel()
	a := start(t)
	_, err := a.p.Join(t.Context(), netprovider.JoinRequest{Agent: "coder", URL: "loopnet://" + runDir(t) + "?token=good", Token: "bad", StateDir: t.TempDir(), Private: true})
	var e *netprovider.Error
	if !errors.As(err, &e) || e.Kind != netprovider.KindToken || e.Fix == "" {
		t.Fatalf("Process.Join() error = %#v, want a token error with a fix", err)
	}
}

func TestUnknownMethod(t *testing.T) {
	t.Parallel()
	a := start(t)
	err := a.p.Call(t.Context(), "nope", nil, nil)
	var rpcErr *netprovider.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != netprovider.CodeMethodNotFound {
		t.Fatalf("Process.Call(nope) error = %v, want method not found", err)
	}
}

func TestTunnelRefusesWrongToken(t *testing.T) {
	t.Parallel()
	network := "loopnet://" + runDir(t)
	a := join(t, network, "coder", false)
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "unix", filepath.Join(a.runDir, "connect.sock"))
	if err != nil {
		t.Fatalf("dial error = %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("closing: %v", err)
		}
	}()
	if _, err := io.WriteString(conn, "CONNECT loopnet.local:80 HTTP/1.1\r\nHost: loopnet.local:80\r\n\r\n"); err != nil {
		t.Fatalf("write error = %v", err)
	}
	b := make([]byte, 12)
	if _, err := io.ReadFull(conn, b); err != nil {
		t.Fatalf("read error = %v", err)
	}
	if got := string(b); got != "HTTP/1.1 407" {
		t.Fatalf("CONNECT without a token = %q, want HTTP/1.1 407", got)
	}
}

func TestCloseStopsProcess(t *testing.T) {
	t.Parallel()
	a := start(t)
	if _, err := a.p.Describe(t.Context()); err != nil {
		t.Fatalf("Process.Describe() error = %v", err)
	}
	if err := a.p.Close(); err != nil {
		t.Fatalf("Process.Close() error = %v", err)
	}
	select {
	case <-a.p.Done():
	default:
		t.Fatalf("Process.Done() open after Close")
	}
	if _, err := a.p.Describe(context.Background()); err == nil {
		t.Fatalf("Process.Describe() after Close error = nil, want an error")
	}
}

func TestCloseReturnsWhenChildrenHoldTheOutput(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	script := filepath.Join(t.TempDir(), "provider")
	body := `#!/bin/sh
sleep 1000 &
read -r line
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"name":"child","version":"v1","protocol":1,"hosts":[]}}'
printf '\033[31mred\033[0m log\n' >&2
cat >/dev/null
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	p, err := host.Start(host.Config{Path: script, RunDir: runDir(t), Env: host.Environ(os.Environ()), Stderr: io.Discard})
	if err != nil {
		t.Fatalf("host.Start() error = %v", err)
	}
	if _, err := p.Describe(t.Context()); err != nil {
		t.Fatalf("Process.Describe() error = %v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Process.Close() error = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("Process.Close() didn't return while a child holds the provider's output")
	}
	if got := p.Err().Error(); !strings.Contains(got, "red log") || strings.Contains(got, "\x1b") {
		t.Fatalf("Process.Err() = %q, want the log without escapes", got)
	}
}
