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

package agentsrv

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
)

// harnessListeners gives each agent process its own way to reach the
// server, bound to the process's agent key: a Unix socket alone in a
// directory, which the process's container mounts read-only, or on Docker
// Desktop a key for the shared TCP listener. A request tells which process
// it came from, so a process can't use the handles of another's
// conversations, nor replace another's socket.
type harnessListeners struct {
	// dir holds the directory of each socket; empty over TCP.
	dir string
	// tcp is the TCP listener, reached at url on host dial; nil with sockets.
	tcp  net.Listener
	url  string
	dial string

	mu sync.Mutex
	// srv serves the listeners once the server serves; until then new
	// sockets wait in pending.
	srv     *http.Server
	errc    chan<- error
	pending []net.Listener
	// keys has the agent key of each TCP key.
	keys map[string]string
}

// harnessEndpoint is how one agent process reaches the server.
type harnessEndpoint struct {
	// socket is the host path of its Unix socket, empty over TCP.
	socket string
	// key is what it sends to the TCP listener, empty with sockets.
	key   string
	close func() error
}

// listenHarnessSockets prepares dir for the sockets of agent processes,
// removing those a previous run left behind.
func listenHarnessSockets(dir string) (*harnessListeners, error) {
	if err := fsutil.MkdirPrivate(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("removing old harness sockets: %w", err)
	}
	if err := fsutil.MkdirPrivate(dir); err != nil {
		return nil, err
	}
	return &harnessListeners{dir: dir}, nil
}

// listenHarnessTCP listens on bind for agent processes that reach it on
// host dial.
func listenHarnessTCP(bind, dial string) (*harnessListeners, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, "0"))
	if err != nil {
		return nil, fmt.Errorf("listening for the harness on %s: %w", bind, err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return nil, errors.Join(err, ln.Close())
	}
	return &harnessListeners{tcp: ln, url: "http://" + net.JoinHostPort(dial, port), dial: dial, keys: map[string]string{}}, nil
}

// open opens the endpoint of the agent process of agentKey. Closing it
// stops the server from accepting requests through it.
func (hl *harnessListeners) open(agentKey string) (*harnessEndpoint, error) {
	if hl.tcp != nil {
		return hl.openKey(agentKey), nil
	}
	return hl.openSocket(agentKey)
}

func (hl *harnessListeners) openKey(agentKey string) *harnessEndpoint {
	key := cryptoutil.RandomString(40, cryptoutil.Alphanumeric)
	hl.mu.Lock()
	hl.keys[key] = agentKey
	hl.mu.Unlock()
	return &harnessEndpoint{key: key, close: func() error {
		hl.mu.Lock()
		delete(hl.keys, key)
		hl.mu.Unlock()
		return nil
	}}
}

func (hl *harnessListeners) openSocket(agentKey string) (*harnessEndpoint, error) {
	dir := filepath.Join(hl.dir, cryptoutil.RandomString(agentdir.HarnessSocketIDLen, handleAlphabet))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, harnessSocketName)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("listening for the harness on %s: %w", sock, err), os.RemoveAll(dir))
	}
	kl := &keyedListener{Listener: ln, agentKey: agentKey}
	hl.mu.Lock()
	if hl.srv != nil {
		go serveHarness(hl.srv, kl, hl.errc)
	} else {
		hl.pending = append(hl.pending, kl)
	}
	hl.mu.Unlock()
	return &harnessEndpoint{socket: sock, close: func() error {
		hl.mu.Lock()
		hl.pending = slices.DeleteFunc(hl.pending, func(l net.Listener) bool { return l == kl })
		hl.mu.Unlock()
		return errors.Join(closeListener(kl), os.RemoveAll(dir))
	}}, nil
}

// serve serves handler on every endpoint, those opened later included,
// and returns the function that shuts them down.
func (hl *harnessListeners) serve(ctx context.Context, handler http.Handler, errc chan<- error) func(context.Context) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if kc, ok := c.(*keyedConn); ok {
				return context.WithValue(ctx, harnessProcessKey{}, kc.agentKey)
			}
			return ctx
		},
	}
	hl.mu.Lock()
	hl.srv, hl.errc = srv, errc
	listeners := hl.pending
	hl.pending = nil
	hl.mu.Unlock()
	if hl.tcp != nil {
		listeners = append(listeners, hl.tcp)
	}
	for _, ln := range listeners {
		go serveHarness(srv, ln, errc)
	}
	return func(ctx context.Context) error {
		err := srv.Shutdown(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			return srv.Close()
		}
		return err
	}
}

// serveHarness serves ln until it or srv closes.
func serveHarness(srv *http.Server, ln net.Listener, errc chan<- error) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		reportFatal(errc, fmt.Errorf("harness listener: %w", err))
	}
}

// close closes the TCP listener. Sockets close with their endpoints.
func (hl *harnessListeners) close(context.Context) error {
	if hl.tcp == nil {
		return nil
	}
	return closeListener(hl.tcp)
}

// bridge is how agents URLs reach the server.
func (hl *harnessListeners) bridge(main *harnessEndpoint) harnessBridge {
	return harnessBridge{socket: main.socket, url: hl.url}
}

type harnessProcessKey struct{}

// processOf returns the agent key of the process a request on the harness
// listener came from, or false when it proves none.
func (hl *harnessListeners) processOf(r *http.Request) (string, bool) {
	if key, ok := r.Context().Value(harnessProcessKey{}).(string); ok {
		return key, true
	}
	tok, ok := auth.BearerToken(r.Header.Get("Authorization"))
	if !ok {
		return "", false
	}
	hl.mu.Lock()
	defer hl.mu.Unlock()
	for key, agentKey := range hl.keys {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(key)) == 1 {
			return agentKey, true
		}
	}
	return "", false
}

// keyedListener tags its connections with the agent key of the process
// whose socket it is.
type keyedListener struct {
	net.Listener
	agentKey string
}

func (l *keyedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &keyedConn{Conn: c, agentKey: l.agentKey}, nil
}

type keyedConn struct {
	net.Conn
	agentKey string
}
