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

package runner

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/agentprofile"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/httpserver"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

// listeners give each agent process its own way to reach the caller,
// bound to the process's key: a Unix socket alone in a
// directory, which the process's container mounts read-only, or on Docker
// Desktop a key for the shared TCP listener. A request tells which process
// it came from, so a process can't use the handles of another's
// conversations, nor replace another's socket.
type listeners struct {
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
	fatal   func(error)
	pending []net.Listener
	// keys has the agent key of each TCP key.
	keys map[string]string
}

// endpoint is how one agent process reaches the caller.
type endpoint struct {
	// socket is the host path of its Unix socket, empty over TCP.
	socket string
	// key is what it sends to the TCP listener, empty with sockets.
	key   string
	close func() error
}

// listenSockets prepares dir for the sockets of agent processes, removing
// those a previous run left behind.
func listenSockets(dir string) (*listeners, error) {
	if err := fsutil.MkdirPrivate(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("removing old agent sockets: %w", err)
	}
	if err := fsutil.MkdirPrivate(dir); err != nil {
		return nil, err
	}
	return &listeners{dir: dir}, nil
}

// listenTCP listens on bind for agent processes that reach it on host
// dial.
func listenTCP(bind, dial string) (*listeners, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, "0"))
	if err != nil {
		return nil, fmt.Errorf("listening for the agent on %s: %w", bind, err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return nil, errors.Join(err, ln.Close())
	}
	return &listeners{tcp: ln, url: "http://" + net.JoinHostPort(dial, port), dial: dial, keys: map[string]string{}}, nil
}

// open opens the endpoint of the agent process of agentKey. Closing it
// stops the server from accepting requests through it.
func (hl *listeners) open(agentKey string) (*endpoint, error) {
	if hl.tcp != nil {
		return hl.openKey(agentKey), nil
	}
	return hl.openSocket(agentKey)
}

func (hl *listeners) openKey(agentKey string) *endpoint {
	key := cryptoutil.RandomString(40, cryptoutil.Alphanumeric)
	hl.mu.Lock()
	hl.keys[key] = agentKey
	hl.mu.Unlock()
	return &endpoint{key: key, close: func() error {
		hl.mu.Lock()
		delete(hl.keys, key)
		hl.mu.Unlock()
		return nil
	}}
}

func (hl *listeners) openSocket(agentKey string) (*endpoint, error) {
	dir := filepath.Join(hl.dir, cryptoutil.RandomString(sandboxrt.SocketIDLen, idAlphabet))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, agentSocketName)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("listening for the agent on %s: %w", sock, err), os.RemoveAll(dir))
	}
	kl := &keyedListener{Listener: ln, agentKey: agentKey}
	hl.mu.Lock()
	if hl.srv != nil {
		go serveAgents(hl.srv, kl, hl.fatal)
	} else {
		hl.pending = append(hl.pending, kl)
	}
	hl.mu.Unlock()
	return &endpoint{socket: sock, close: func() error {
		hl.mu.Lock()
		hl.pending = slices.DeleteFunc(hl.pending, func(l net.Listener) bool { return l == kl })
		hl.mu.Unlock()
		return errors.Join(httpserver.CloseListener(kl), os.RemoveAll(dir))
	}}, nil
}

// serve serves handler on every endpoint, those opened later included,
// and returns the function that shuts them down.
func (hl *listeners) serve(ctx context.Context, handler http.Handler, fatal func(error)) func(context.Context) error {
	srv := httpserver.New(ctx, handler, httpserver.WithConnContext(func(ctx context.Context, c net.Conn) context.Context {
		if kc, ok := c.(*keyedConn); ok {
			return context.WithValue(ctx, processKey{}, kc.agentKey)
		}
		return ctx
	}))
	hl.mu.Lock()
	hl.srv, hl.fatal = srv, fatal
	pending := hl.pending
	hl.pending = nil
	hl.mu.Unlock()
	if hl.tcp != nil {
		pending = append(pending, hl.tcp)
	}
	for _, ln := range pending {
		go serveAgents(srv, ln, fatal)
	}
	return func(ctx context.Context) error {
		err := srv.Shutdown(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			return srv.Close()
		}
		return err
	}
}

// serveAgents serves ln until it or srv closes.
func serveAgents(srv *http.Server, ln net.Listener, fatal func(error)) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		fatal(fmt.Errorf("agent listener: %w", err))
	}
}

// close closes the TCP listener. Sockets close with their endpoints.
func (hl *listeners) close() error {
	if hl.tcp == nil {
		return nil
	}
	return httpserver.CloseListener(hl.tcp)
}

type processKey struct{}

// processOf returns the key of the process a request on the listeners
// came from, or false when it proves none.
func (hl *listeners) processOf(r *http.Request) (string, bool) {
	if key, ok := r.Context().Value(processKey{}).(string); ok {
		return key, true
	}
	tok, ok := httpserver.BearerToken(r.Header.Get("Authorization"))
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

// credentialHelperPath is where the agent's credential helper is written,
// relative to its home.
const credentialHelperPath = ".local/share/a2a-serve/credential-helper"

// writeCredentialHelper writes a credential helper into the agent home that
// gives the agent's a2a CLI the key for the TCP bridge, and returns the
// environment variable that has the CLI use it for requests to host only.
func writeCredentialHelper(home, host, key string) (env string, err error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	p := filepath.FromSlash(credentialHelperPath)
	if err := root.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"headers\":{\"Authorization\":[\"Bearer " + key + "\"]}}'\n"
	if err := fsutil.WriteAtomicIn(root, p, []byte(script), 0o700); err != nil {
		return "", err
	}
	return agentprofile.EnvCredentialHelper + "=" + host + "=" + path.Join(agentprofile.ContainerHome, credentialHelperPath), nil
}
