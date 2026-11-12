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

package host

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/a2aproject/a2a-serve/netprovider"
)

const (
	// headerTimeout bounds how long a connection may take to send its
	// header.
	headerTimeout = 10 * time.Second
	// maxHandshakes bounds the connections whose header is being read.
	maxHandshakes = 256
)

// Listener accepts the connections a provider forwards to one socket, and
// knows who each comes from.
type Listener struct {
	ln    net.Listener
	path  string
	kind  netprovider.Kind
	token string

	conns  chan *Conn
	done   chan struct{}
	once   sync.Once
	slots  chan struct{}
	nextID atomic.Uint64

	mu      sync.Mutex
	callers map[string]netprovider.Caller
}

func listen(ctx context.Context, path string, kind netprovider.Kind, token string) (*Listener, error) {
	if err := removeSocket(path); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening for the network provider: %w", err)
	}
	l := &Listener{
		ln: ln, path: path, kind: kind, token: token,
		conns: make(chan *Conn), done: make(chan struct{}), slots: make(chan struct{}, maxHandshakes),
		callers: map[string]netprovider.Caller{},
	}
	go l.acceptLoop()
	return l, nil
}

func (l *Listener) acceptLoop() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("network provider socket stopped", "kind", l.kind, "error", err)
			}
			return
		}
		select {
		case l.slots <- struct{}{}:
		default:
			slog.Warn("too many network provider connections in their handshake; refusing one", "kind", l.kind)
			closeQuietly(c)
			continue
		}
		go l.handshake(c)
	}
}

func (l *Listener) handshake(c net.Conn) {
	defer func() { <-l.slots }()
	caller, err := l.readHeader(c)
	if err != nil {
		slog.Warn("refusing a network provider connection", "kind", l.kind, "error", err)
		closeQuietly(c)
		return
	}
	addr := addr("netprovider-" + l.kind.String() + "-" + strconv.FormatUint(l.nextID.Add(1), 10))
	conn := &Conn{Conn: c, caller: caller, remote: addr, l: l}
	l.mu.Lock()
	l.callers[addr.String()] = caller
	l.mu.Unlock()
	select {
	case l.conns <- conn:
	case <-l.done:
		closeQuietly(conn)
	}
}

func (l *Listener) readHeader(c net.Conn) (netprovider.Caller, error) {
	if err := c.SetReadDeadline(time.Now().Add(headerTimeout)); err != nil {
		return netprovider.Caller{}, err
	}
	h, err := netprovider.ReadHeader(c)
	if err != nil {
		return netprovider.Caller{}, err
	}
	if subtle.ConstantTimeCompare([]byte(h.Token), []byte(l.token)) != 1 {
		return netprovider.Caller{}, errors.New("wrong or missing token")
	}
	if l.kind == netprovider.Public && h.Caller.Identified() {
		return netprovider.Caller{}, errors.New("a connection to the public URL claims an identity")
	}
	for name, v := range map[string]string{"node": h.Caller.Node, "email": h.Caller.Email, "machine": h.Caller.Machine, "client key": h.Caller.ClientKey} {
		if !printable(v) {
			return netprovider.Caller{}, fmt.Errorf("the %s isn't printable UTF-8", name)
		}
	}
	return h.Caller, c.SetReadDeadline(time.Time{})
}

func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// Accept returns the next connection, a [*Conn].
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close closes the socket and removes it.
func (l *Listener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.done)
		err = l.ln.Close()
		if rerr := removeSocket(l.path); rerr != nil {
			err = errors.Join(err, rerr)
		}
	})
	return err
}

// Addr returns the socket's address.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Caller returns who the open connection with the remote address
// remoteAddr comes from.
func (l *Listener) Caller(remoteAddr string) (netprovider.Caller, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.callers[remoteAddr]
	return c, ok
}

// Conn is a connection a provider forwarded, after its header.
type Conn struct {
	net.Conn
	caller netprovider.Caller
	remote addr
	l      *Listener
	once   sync.Once
}

// Caller returns who the connection comes from.
func (c *Conn) Caller() netprovider.Caller { return c.caller }

// RemoteAddr returns an address unique to the connection, by which
// [Listener.Caller] finds its caller.
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

// Close closes the connection and forgets its caller.
func (c *Conn) Close() error {
	c.once.Do(func() {
		c.l.mu.Lock()
		delete(c.l.callers, c.remote.String())
		c.l.mu.Unlock()
	})
	return c.Conn.Close()
}

// CloseWrite half-closes the connection.
func (c *Conn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

type addr string

func (a addr) Network() string { return "netprovider" }
func (a addr) String() string  { return string(a) }

func closeQuietly(c net.Conn) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Debug("closing a network provider connection", "error", err)
	}
}
