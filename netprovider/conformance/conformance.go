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

// Package conformance tests that a network provider binary, in any
// language, speaks the protocol a2a-serve expects. Run it with
//
//	go test ./netprovider/conformance -provider /path/to/my-provider -url <network-url>
//
// or call [Run] from a test of your own.
package conformance

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/netprovider"
	"github.com/a2aproject/a2a-serve/netprovider/host"
)

// Config says which provider to test.
type Config struct {
	// Binary is the provider.
	Binary string
	// URL is a network to join, with the provider's settings. Without it,
	// the suite skips what needs a network.
	URL string
	// Token is the network token to join with.
	Token string
	// Agent is the name to join as (default conformance).
	Agent string
}

// timeout bounds each step.
const timeout = 30 * time.Second

var providerName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Run runs the suite.
func Run(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.Agent == "" {
		cfg.Agent = "conformance"
	}
	t.Run("describe", func(t *testing.T) { testDescribe(t, cfg) })
	t.Run("errors", func(t *testing.T) { testErrors(t, cfg) })
	t.Run("malformed input", func(t *testing.T) { testMalformed(t, cfg) })
	t.Run("stdin EOF stops", func(t *testing.T) { testEOF(t, cfg) })
	t.Run("join and listen", func(t *testing.T) {
		if cfg.URL == "" {
			t.Skip("no network URL")
		}
		testJoin(t, cfg)
	})
}

func runDir(t *testing.T) string {
	t.Helper()
	// Socket paths have to stay short, which t.TempDir doesn't promise.
	dir, err := os.MkdirTemp("", "npc")
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

func start(t *testing.T, cfg Config) (*host.Process, string) {
	t.Helper()
	dir := runDir(t)
	p, err := host.Start(host.Config{Path: cfg.Binary, RunDir: dir, Env: host.Environ(os.Environ()), Stderr: testWriter{t}})
	if err != nil {
		t.Fatalf("host.Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("closing the provider: %v", err)
		}
	})
	return p, dir
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Logf("provider: %s", strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(t.Context(), timeout)
	t.Cleanup(cancel)
	return c
}

func testDescribe(t *testing.T, cfg Config) {
	p, _ := start(t, cfg)
	d, err := p.Describe(ctx(t))
	if err != nil {
		t.Fatalf("describe error = %v", err)
	}
	if !providerName.MatchString(d.Name) {
		t.Errorf("describe name = %q, want lower-case letters, digits and dashes", d.Name)
	}
	if d.Version == "" {
		t.Errorf("describe version is empty")
	}
	for _, h := range d.Hosts {
		if h == "" || strings.ContainsAny(h, ":/ ") {
			t.Errorf("describe host %q isn't a host name", h)
		}
	}
}

func testErrors(t *testing.T, cfg Config) {
	p, _ := start(t, cfg)
	err := p.Call(ctx(t), "conformance/unknown", nil, nil)
	var rpcErr *netprovider.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != netprovider.CodeMethodNotFound {
		t.Errorf("unknown method error = %v, want code %d", err, netprovider.CodeMethodNotFound)
	}
	// Before join, member and peers answer, with a result or an error.
	if _, err := p.Member(ctx(t), "conformance-node"); errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("member before join didn't answer in %v", timeout)
	}
	if _, err := p.Peers(ctx(t)); errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("peers before join didn't answer in %v", timeout)
	}
}

// raw runs the provider with its stdio exposed.
type raw struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader
	done  chan error
}

func startRaw(t *testing.T, cfg Config) *raw {
	t.Helper()
	cmd := exec.Command(cfg.Binary, "serve")
	cmd.Env = host.Environ(os.Environ())
	cmd.Dir = runDir(t)
	cmd.Stderr = testWriter{t}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe() error = %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", cfg.Binary, err)
	}
	r := &raw{cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout), done: make(chan error, 1)}
	t.Cleanup(func() {
		if err := stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("closing stdin: %v", err)
		}
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("killing the provider: %v", err)
		}
	})
	return r
}

func (r *raw) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(r.stdin, line+"\n"); err != nil {
		t.Fatalf("writing to the provider: %v", err)
	}
}

// response reads messages until the response with id, skipping
// notifications.
func (r *raw) response(t *testing.T, id string) *netprovider.Message {
	t.Helper()
	got := make(chan *netprovider.Message, 1)
	errc := make(chan error, 1)
	go func() {
		for {
			m, err := netprovider.ReadMessage(r.out)
			if err != nil {
				errc <- err
				return
			}
			if m.Method == "" && string(m.ID) == id {
				got <- m
				return
			}
		}
	}()
	select {
	case m := <-got:
		return m
	case err := <-errc:
		t.Fatalf("reading the response %s: %v", id, err)
	case <-time.After(timeout):
		t.Fatalf("no response %s in %v", id, timeout)
	}
	return nil
}

func testMalformed(t *testing.T, cfg Config) {
	r := startRaw(t, cfg)
	r.send(t, "{not json")
	if m := r.response(t, "null"); m.Error == nil || m.Error.Code != netprovider.CodeParseError {
		t.Errorf("malformed JSON answer = %+v, want a parse error with id null", m)
	}
	r.send(t, `{"jsonrpc":"2.0","id":7,"method":"describe","params":{"unexpected":true}}`)
	if m := r.response(t, "7"); m.Error != nil {
		t.Errorf("describe after malformed input error = %v, want a result", m.Error)
	}
	r.send(t, `{"jsonrpc":"2.0","id":8,"method":"member","params":"wrong"}`)
	if m := r.response(t, "8"); m.Error == nil {
		t.Errorf("member with wrong params = %s, want an error", m.Result)
	}
}

func testEOF(t *testing.T, cfg Config) {
	r := startRaw(t, cfg)
	go func() { r.done <- r.cmd.Wait() }()
	if err := r.stdin.Close(); err != nil {
		t.Fatalf("closing stdin: %v", err)
	}
	select {
	case err := <-r.done:
		t.Logf("the provider exited: %v", err)
	case <-time.After(timeout):
		t.Fatalf("the provider still runs %v after stdin closed", timeout)
	}
}

func testJoin(t *testing.T, cfg Config) {
	p, dir := start(t, cfg)
	d, err := p.Describe(ctx(t))
	if err != nil {
		t.Fatalf("describe error = %v", err)
	}
	res, err := p.Join(ctx(t), netprovider.JoinRequest{Agent: cfg.Agent, URL: cfg.URL, Token: cfg.Token, StateDir: t.TempDir(), Private: true})
	if err != nil {
		t.Fatalf("join error = %v", err)
	}
	if res.Owner != "" && !strings.Contains(res.Owner, "@") {
		t.Errorf("join owner = %q, want an email or empty", res.Owner)
	}
	ep, err := p.Listen(ctx(t), true, false)
	if err != nil {
		t.Fatalf("listen error = %v", err)
	}
	u, err := url.Parse(ep.PrivateURL)
	if err != nil || u.Host == "" {
		t.Fatalf("listen private URL = %q, want a URL", ep.PrivateURL)
	}
	t.Run("CONNECT needs the token", func(t *testing.T) {
		var dl net.Dialer
		conn, err := dl.DialContext(ctx(t), "unix", filepath.Join(dir, "connect.sock"))
		if err != nil {
			t.Fatalf("the provider doesn't listen on connect.sock: %v", err)
		}
		defer func() {
			if err := conn.Close(); err != nil {
				t.Errorf("closing: %v", err)
			}
		}()
		if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			t.Fatalf("SetDeadline() error = %v", err)
		}
		if _, err := fmt.Fprintf(conn, "CONNECT %s:80 HTTP/1.1\r\nHost: %[1]s:80\r\nProxy-Authorization: Bearer wrong\r\n\r\n", u.Hostname()); err != nil {
			t.Fatalf("writing CONNECT: %v", err)
		}
		status, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			t.Fatalf("reading the CONNECT response: %v", err)
		}
		if !strings.Contains(status, " 407 ") {
			t.Errorf("CONNECT with a wrong token = %q, want 407", strings.TrimSpace(status))
		}
	})
	t.Run("peers on described hosts", func(t *testing.T) {
		peers, err := p.Peers(ctx(t))
		if errors.Is(err, netprovider.ErrUnsupported) {
			t.Skip("peers unsupported")
		}
		if err != nil {
			t.Fatalf("peers error = %v", err)
		}
		for _, peer := range peers {
			pu, err := url.Parse(peer.URL)
			if err != nil || !contains(d.Hosts, pu.Hostname()) {
				t.Errorf("peer %q URL %q isn't on the described hosts %v", peer.Name, peer.URL, d.Hosts)
			}
		}
	})
}

func contains(hosts []string, h string) bool {
	for _, x := range hosts {
		if strings.EqualFold(x, h) {
			return true
		}
	}
	return false
}
