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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Kind says on which of the agent's URLs a connection arrived.
type Kind int

// Connection kinds.
const (
	Private Kind = iota
	Public
)

func (k Kind) String() string {
	if k == Public {
		return "public"
	}
	return "private"
}

// Inbound hands the network's connections to the server.
type Inbound struct {
	ctx             context.Context
	private, public string
	token           string
}

// Private reports whether the agent wants a private URL.
func (in *Inbound) Private() bool { return in.private != "" }

// Public reports whether the agent wants a public URL.
func (in *Inbound) Public() bool { return in.public != "" }

// Dial opens a connection to the server for a caller, with its header
// written. On the public URL, only the client key of c reaches the server.
func (in *Inbound) Dial(ctx context.Context, kind Kind, c Caller) (net.Conn, error) {
	path := in.private
	if kind == Public {
		path = in.public
		c = Caller{ClientKey: c.ClientKey}
	}
	if path == "" {
		return nil, fmt.Errorf("the agent has no %s URL", kind)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	if err := WriteHeader(conn, Header{Token: in.token, Caller: c}); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	return conn, nil
}

// Forward copies conn to and from a new connection to the server for c,
// and closes both once either side is done.
func (in *Inbound) Forward(kind Kind, conn net.Conn, c Caller) {
	srv, err := in.Dial(in.ctx, kind, c)
	if err != nil {
		slog.Warn("forwarding a connection to the server", "kind", kind, "error", err)
		closeConn(conn)
		return
	}
	pipe(conn, srv)
}

// Serve forwards the connections of ln, each for the caller callerOf
// returns, until ln is closed.
func (in *Inbound) Serve(kind Kind, ln net.Listener, callerOf func(net.Conn) Caller) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("listener stopped", "kind", kind, "error", err)
			}
			return
		}
		go in.Forward(kind, conn, callerOf(conn))
	}
}

func pipe(a, b net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			closeConn(a)
			closeConn(b)
		})
	}
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		if _, err := io.Copy(dst, src); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Debug("copying a connection", "error", err)
		}
		// Half-close when the connection supports it, so that a request
		// body's end reaches the other side and the response still flows.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			if err := cw.CloseWrite(); err == nil {
				return
			}
		}
		closeBoth()
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	closeBoth()
}
