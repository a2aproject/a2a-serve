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

package sandboxnet

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// With Docker Desktop, a relay container next to the agent's container
// serves the agent's sockets in a volume both share, since a socket on
// the host can't be mounted into a container. It forwards each connection
// to the server, which decides it as it decides those of a mounted socket.
const (
	// RelayLauncher is where the relay image has the a2a-serve binary,
	// which the agent's container runs as its launcher.
	RelayLauncher = "/opt/a2a/a2a-serve"
	// ServerSocketName is the name of the socket through which the agent
	// reaches the server, next to the egress socket of a relay.
	ServerSocketName = "a2a.sock"
	// RelayReady is the line a relay prints once its sockets listen.
	RelayReady = "a2a-serve relay ready"
	// relayPreface starts each egress connection of a relay to the server,
	// followed by the key of the container's boundary and a newline.
	relayPreface = "a2a-egress "
	// maxRelayConns bounds the connections a relay forwards at once for
	// each socket.
	maxRelayConns = 1024
)

// RelayPair is a socket a relay serves and where it forwards its
// connections.
type RelayPair struct {
	// Listen is the path of the Unix socket.
	Listen string
	// Target is tcp://host:port or unix:///path.
	Target string
	// Key, when set, starts each forwarded connection with the egress
	// preface and the key.
	Key string
}

// Relay serves pairs until ctx ends, and prints [RelayReady] to ready once
// every socket listens.
func Relay(ctx context.Context, pairs []RelayPair, ready io.Writer) error {
	var lns []net.Listener
	closeAll := func() error {
		var errs []error
		for _, ln := range lns {
			if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	for _, p := range pairs {
		if err := os.Remove(p.Listen); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, closeAll())
		}
		ln, err := net.Listen("unix", p.Listen)
		if err != nil {
			return errors.Join(err, closeAll())
		}
		lns = append(lns, ln)
		// The agent may run as another user than the relay.
		if err := os.Chmod(p.Listen, 0o666); err != nil {
			return errors.Join(err, closeAll())
		}
	}
	if _, err := fmt.Fprintln(ready, RelayReady); err != nil {
		return errors.Join(err, closeAll())
	}
	var wg sync.WaitGroup
	for i, p := range pairs {
		wg.Go(func() { forward(lns[i], p) })
	}
	<-ctx.Done()
	err := closeAll()
	wg.Wait()
	return err
}

// forward forwards the connections of ln to the target of p until ln
// closes.
func forward(ln net.Listener, p RelayPair) {
	slots := make(chan struct{}, maxRelayConns)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			closeConn(c)
			continue
		}
		go func() {
			defer func() { <-slots }()
			defer closeConn(c)
			defer func() { logPanic(recover(), slog.Default(), "") }()
			up, err := dialTarget(p.Target)
			if err != nil {
				slog.Warn("relaying a connection of the sandbox", "target", p.Target, "error", err)
				return
			}
			defer closeConn(up)
			if p.Key != "" {
				if err := up.SetWriteDeadline(time.Now().Add(dialTimeout)); err != nil {
					return
				}
				if _, err := io.WriteString(up, relayPreface+p.Key+"\n"); err != nil {
					return
				}
				if err := up.SetWriteDeadline(time.Time{}); err != nil {
					return
				}
			}
			relay(c, up, DefaultIdleTimeout)
		}()
	}
}

func dialTarget(target string) (net.Conn, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: dialTimeout}
	switch u.Scheme {
	case "tcp":
		return d.Dial("tcp", u.Host)
	case "unix":
		return d.Dial("unix", u.Path)
	}
	return nil, fmt.Errorf("unsupported relay target %q: want tcp://host:port or unix:///path", target)
}

// RelayServer takes the egress connections of relay containers. Each
// connection starts with the key of a container's boundary, which then
// serves it.
type RelayServer struct {
	ln net.Listener

	mu         sync.Mutex
	boundaries map[string]*Boundary
	wg         sync.WaitGroup
}

// ServeRelays serves the relay connections that ln accepts.
func ServeRelays(ln net.Listener) *RelayServer {
	r := &RelayServer{ln: ln, boundaries: map[string]*Boundary{}}
	r.wg.Go(r.serve)
	return r
}

// Register gives b a key, which a relay sends to reach it, until
// unregister.
func (r *RelayServer) Register(b *Boundary) (key string, unregister func(), err error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	key = hex.EncodeToString(buf)
	r.mu.Lock()
	r.boundaries[key] = b
	r.mu.Unlock()
	return key, func() {
		r.mu.Lock()
		delete(r.boundaries, key)
		r.mu.Unlock()
	}, nil
}

// Close stops taking connections. The boundaries close on their own.
func (r *RelayServer) Close() error {
	err := r.ln.Close()
	r.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (r *RelayServer) serve() {
	for {
		c, err := r.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { logPanic(recover(), slog.Default(), "") }()
			r.route(c)
		}()
	}
}

// route reads the preface of c and gives c to the boundary of its key.
func (r *RelayServer) route(c net.Conn) {
	defer func() {
		if p := recover(); p != nil {
			closeConn(c)
			panic(p)
		}
	}()
	key, err := readPreface(c)
	if err != nil {
		slog.Warn("refusing a relay connection", "error", err)
		closeConn(c)
		return
	}
	b := r.boundary(key)
	if b == nil {
		slog.Warn("refusing a relay connection with an unknown key")
		closeConn(c)
		return
	}
	b.ServeConn(c)
}

func (r *RelayServer) boundary(key string) *Boundary {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, b := range r.boundaries {
		if subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1 {
			return b
		}
	}
	return nil
}

// readPreface reads the preface line of a relay connection byte by byte,
// so that the tunnel bytes after it stay unread, and returns its key.
func readPreface(c net.Conn) (string, error) {
	if err := c.SetReadDeadline(time.Now().Add(headTimeout)); err != nil {
		return "", err
	}
	var line []byte
	buf := make([]byte, 1)
	for len(line) < len(relayPreface)+64 {
		if _, err := io.ReadFull(c, buf); err != nil {
			return "", err
		}
		if buf[0] == '\n' {
			key, ok := strings.CutPrefix(string(line), relayPreface)
			if !ok {
				return "", errors.New("malformed preface")
			}
			return key, c.SetReadDeadline(time.Time{})
		}
		line = append(line, buf[0])
	}
	return "", errors.New("preface too long")
}
