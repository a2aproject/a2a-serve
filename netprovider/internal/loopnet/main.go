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

// Command loopnet is a network provider of agents on one machine, for
// tests and as a reference for provider authors. Agents join the network
// at loopnet:///<dir>: each listens on <dir>/<agent>.sock, where its peers
// connect with a first line that names them. Query settings: owner, the
// email the agent signs in as; token, the network token the agent needs;
// health, a health state to report while joining.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-serve/netprovider"
)

// Host is the host of private URLs on the network.
const Host = "loopnet.local"

func main() {
	netprovider.Main(func(h *netprovider.Host) netprovider.Provider { return &loopnet{host: h} })
}

type loopnet struct {
	host *netprovider.Host

	mu        sync.Mutex
	dir       string
	agent     string
	owner     string
	listeners []net.Listener
}

func (l *loopnet) Describe() netprovider.Description {
	return netprovider.Description{Name: "loopnet", Version: "v0.0.0", Hosts: []string{Host}}
}

func (l *loopnet) Join(_ context.Context, req netprovider.JoinRequest) (netprovider.JoinResult, error) {
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme != "loopnet" || u.Path == "" {
		return netprovider.JoinResult{}, fmt.Errorf("want loopnet:///<dir>, got %q", req.URL)
	}
	q := u.Query()
	if want := q.Get("token"); want != "" && req.Token != want {
		return netprovider.JoinResult{}, netprovider.TokenError("the network doesn't know this token", "ask for a new token")
	}
	if err := os.MkdirAll(u.Path, 0o700); err != nil {
		return netprovider.JoinResult{}, err
	}
	if err := os.WriteFile(filepath.Join(req.StateDir, "joined"), []byte(req.Agent), 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("recording the join", "error", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dir, l.agent, l.owner = u.Path, req.Agent, q.Get("owner")
	if l.owner != "" {
		l.host.Notice("signed in to loopnet as " + l.owner)
	}
	if state := q.Get("health"); state != "" {
		l.host.Health(netprovider.Health{State: state, Problem: "loopnet reports " + state})
	}
	return netprovider.JoinResult{Node: node(req.Agent), Owner: l.owner}, nil
}

func node(agent string) string { return "loop:" + agent }

func (l *loopnet) socket(agent string) string { return filepath.Join(l.dir, agent+".sock") }

func (l *loopnet) Listen(ctx context.Context, in *netprovider.Inbound) (netprovider.ListenResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.agent == "" {
		return netprovider.ListenResult{}, errors.New("listen before join")
	}
	var res netprovider.ListenResult
	var lc net.ListenConfig
	if in.Private() {
		path := l.socket(l.agent)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return res, err
		}
		ln, err := lc.Listen(ctx, "unix", path)
		if err != nil {
			return res, err
		}
		l.listeners = append(l.listeners, ln)
		go l.servePrivate(in, ln)
		res.PrivateURL = "http://" + Host + "/" + l.agent
		res.PrivateNote = "works only from other agents on loopnet"
	}
	if in.Public() {
		ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			return res, err
		}
		l.listeners = append(l.listeners, ln)
		go in.Serve(netprovider.Public, ln, func(c net.Conn) netprovider.Caller {
			host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
			return netprovider.Caller{ClientKey: "ip:" + host}
		})
		res.PublicURL = "http://" + ln.Addr().String()
	}
	return res, nil
}

// servePrivate forwards the connections of peers, which start with a line
// "FROM <agent> [<email>]".
func (l *loopnet) servePrivate(in *netprovider.Inbound, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			br := bufio.NewReader(c)
			line, err := br.ReadString('\n')
			fields := strings.Fields(strings.TrimPrefix(line, "FROM "))
			if err != nil || !strings.HasPrefix(line, "FROM ") || len(fields) == 0 {
				if err := c.Close(); err != nil {
					slog.Debug("closing a connection", "error", err)
				}
				return
			}
			caller := netprovider.Caller{Node: node(fields[0]), Machine: fields[0], ClientKey: node(fields[0])}
			if len(fields) > 1 {
				caller.Email = fields[1]
			}
			in.Forward(netprovider.Private, &readerConn{Conn: c, r: br}, caller)
		}()
	}
}

type readerConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *readerConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

func (l *loopnet) Member(_ context.Context, n string) (netprovider.Caller, error) {
	agent, ok := strings.CutPrefix(n, "loop:")
	if !ok {
		return netprovider.Caller{}, netprovider.ErrNotMember
	}
	l.mu.Lock()
	path := l.socket(agent)
	l.mu.Unlock()
	if _, err := os.Stat(path); err != nil {
		return netprovider.Caller{}, netprovider.ErrNotMember
	}
	return netprovider.Caller{Node: n, Machine: agent}, nil
}

func (l *loopnet) Peers(context.Context) ([]netprovider.PeerInfo, error) {
	l.mu.Lock()
	dir, self := l.dir, l.agent
	l.mu.Unlock()
	matches, err := filepath.Glob(filepath.Join(dir, "*.sock"))
	if err != nil {
		return nil, err
	}
	var peers []netprovider.PeerInfo
	for _, m := range matches {
		name := strings.TrimSuffix(filepath.Base(m), ".sock")
		if name == self {
			continue
		}
		peers = append(peers, netprovider.PeerInfo{Name: name, URL: "http://" + Host + "/" + name, Node: node(name)})
	}
	return peers, nil
}

// Outbound forwards a request for http://loopnet.local/<agent>/<path> to
// the agent, as /<path>.
func (l *loopnet) Outbound() http.Handler {
	return &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			agent, rest, _ := strings.Cut(strings.TrimPrefix(r.In.URL.Path, "/"), "/")
			r.Out.URL.Scheme, r.Out.URL.Host, r.Out.URL.Path, r.Out.URL.RawPath = "http", agent, "/"+rest, ""
			r.Out.Host = r.In.Host
		},
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			agent, _, _ := strings.Cut(addr, ":")
			l.mu.Lock()
			path, self, owner := l.socket(agent), l.agent, l.owner
			l.mu.Unlock()
			var d net.Dialer
			c, err := d.DialContext(ctx, "unix", path)
			if err != nil {
				return nil, err
			}
			if _, err := fmt.Fprintf(c, "FROM %s %s\n", self, owner); err != nil {
				return nil, errors.Join(err, c.Close())
			}
			return c, nil
		}},
	}
}

func (l *loopnet) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var errs []error
	for _, ln := range l.listeners {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	l.listeners = nil
	return errors.Join(errs...)
}
