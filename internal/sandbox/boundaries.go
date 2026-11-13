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

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/egress"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

// Boundaries opens the egress boundaries of containers: an egress socket
// each, alone in a directory of its own, or a key on the listener that
// egress containers reach. It also keeps the copy of the launcher.
type Boundaries struct {
	dir      string
	nameLen  int
	alphabet string
	blocks   *sandboxnet.Blocks

	mu sync.Mutex
	// relays takes the connections of relay containers, which reach it at
	// relayAddr, mounting relayMount on Linux.
	relays                *sandboxnet.RelayServer
	relayAddr, relayMount string
	// launcherCopy is the copy of the running a2a-serve.
	launcherCopy string
}

// NewBoundaries returns the boundaries that keep their sockets in dir, in
// directories with random names of nameLen characters of alphabet, and
// record the connections they refuse in blocks.
func NewBoundaries(dir string, nameLen int, alphabet string, blocks *sandboxnet.Blocks) *Boundaries {
	return &Boundaries{dir: dir, nameLen: nameLen, alphabet: alphabet, blocks: blocks}
}

// BoundaryRules decide the connections of a container: policy at each
// connection, and the headers of inject, whose hosts get certificates that
// ca signs.
type BoundaryRules struct {
	Policy func() egress.Policy
	Inject []sandboxnet.Injection
	CA     *sandboxnet.CA
}

// Opener returns the function that opens the boundary of the container
// named name with rules.
func (e *Boundaries) Opener(name string, rules BoundaryRules) OpenEgress {
	return func(gateway HostGatewayFunc, relay bool) (EgressEndpoint, error) {
		cfg := sandboxnet.BoundaryConfig{Sandbox: name, Policy: rules.Policy, HostGateway: gateway, Blocks: e.blocks, Inject: rules.Inject, CA: rules.CA}
		if relay {
			return e.openRelayed(cfg)
		}
		if err := fsutil.MkdirPrivate(e.dir); err != nil {
			return EgressEndpoint{}, err
		}
		dir := filepath.Join(e.dir, cryptoutil.RandomString(e.nameLen, e.alphabet))
		if err := os.Mkdir(dir, 0o700); err != nil {
			return EgressEndpoint{}, err
		}
		sock := filepath.Join(dir, sandboxnet.SocketName)
		b, err := sandboxnet.Listen(sock, cfg)
		if err != nil {
			return EgressEndpoint{}, errors.Join(err, os.RemoveAll(dir))
		}
		return EgressEndpoint{Socket: sock, Close: func() error { return errors.Join(b.Close(), os.RemoveAll(dir)) }}, nil
	}
}

// openRelayed opens a boundary that a relay container reaches through the
// server's relay listener, with a key of its own.
func (e *Boundaries) openRelayed(cfg sandboxnet.BoundaryConfig) (EgressEndpoint, error) {
	relays, addr, mount, err := e.relayListener()
	if err != nil {
		return EgressEndpoint{}, err
	}
	b, err := sandboxnet.New(cfg)
	if err != nil {
		return EgressEndpoint{}, err
	}
	key, unregister, err := relays.Register(b)
	if err != nil {
		return EgressEndpoint{}, errors.Join(err, b.Close())
	}
	return EgressEndpoint{RelayAddr: addr, RelayKey: key, RelayMount: mount, Close: func() error {
		unregister()
		return b.Close()
	}}, nil
}

// relayListener returns the server's listener for relay containers,
// listening on the first call. With Docker Desktop it is a TCP port on
// the host's loopback, which containers reach at host.docker.internal. On
// Linux, where containers can't reach the host's loopback, it is a socket
// that relays mount.
func (e *Boundaries) relayListener() (relays *sandboxnet.RelayServer, addr, mount string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.relays != nil {
		return e.relays, e.relayAddr, e.relayMount, nil
	}
	var ln net.Listener
	if bind, dial, tcp := DockerTCPBridge(); tcp {
		if ln, err = net.Listen("tcp", net.JoinHostPort(bind, "0")); err != nil {
			return nil, "", "", fmt.Errorf("listening for egress containers: %w", err)
		}
		_, port, err := net.SplitHostPort(ln.Addr().String())
		if err != nil {
			return nil, "", "", errors.Join(err, ln.Close())
		}
		addr = "tcp://" + net.JoinHostPort(dial, port)
	} else {
		mount = filepath.Join(e.dir, "relay")
		if err := fsutil.MkdirPrivate(e.dir); err != nil {
			return nil, "", "", err
		}
		if err := fsutil.MkdirPrivate(mount); err != nil {
			return nil, "", "", err
		}
		if ln, err = net.Listen("unix", filepath.Join(mount, relaySocketName)); err != nil {
			return nil, "", "", fmt.Errorf("listening for egress containers: %w", err)
		}
		addr = "unix://" + RelayEgressDir + "/" + relaySocketName
	}
	e.relays, e.relayAddr, e.relayMount = sandboxnet.ServeRelays(ln), addr, mount
	return e.relays, addr, mount, nil
}

// relaySocketName is the name of the socket of the relay listener on Linux.
const relaySocketName = "relay.sock"

// Close stops the relay listener.
func (e *Boundaries) Close(context.Context) error {
	e.mu.Lock()
	relays := e.relays
	e.relays = nil
	e.mu.Unlock()
	if relays == nil {
		return nil
	}
	return relays.Close()
}

// launcherDir and launcherFile are the copy of the running a2a-serve, next
// to the egress sockets, that containers run as their launcher, so that a
// binary upgraded in place doesn't change under a running server. The
// caller empties the directory of the boundaries at each start. The copy is
// executable by everyone: with Docker, container root runs it without
// CAP_DAC_OVERRIDE, as another user than its owner. It holds no secret.
const (
	launcherDir  = "launcher"
	launcherFile = "a2a-init"
)

// Launcher copies the running a2a-serve once, and returns the copy, or ""
// to use the binary itself where there is no copy.
func (e *Boundaries) Launcher() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.launcherCopy != "" || runtime.GOOS != "linux" {
		return e.launcherCopy
	}
	if err := fsutil.MkdirPrivate(e.dir); err != nil {
		slog.Warn("copying a2a-serve for the containers; they run the installed binary", "error", err)
		return ""
	}
	dir := filepath.Join(e.dir, launcherDir)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		slog.Warn("copying a2a-serve for the containers; they run the installed binary", "error", err)
		return ""
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		slog.Warn("copying a2a-serve for the containers; they run the installed binary", "error", err)
		return ""
	}
	dst := filepath.Join(dir, launcherFile)
	// /proc/self/exe is the binary this process runs, even after an upgrade
	// replaced its file.
	if err := copyExecutable("/proc/self/exe", dst); err != nil {
		slog.Warn("copying a2a-serve for the containers; they run the installed binary", "error", err)
		return ""
	}
	e.launcherCopy = dst
	return dst
}

// copyExecutable copies src to dst atomically, executable by everyone.
func copyExecutable(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+"-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Chmod(0o755); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}
