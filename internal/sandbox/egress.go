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
	"debug/buildinfo"
	"debug/elf"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// OpenEgress opens the boundary of a container, which decides its
// connections by destination. Connections to host.docker.internal reach
// what hostGateway returns. With relay, a relay container reaches the
// boundary on the container's behalf.
type OpenEgress func(hostGateway HostGatewayFunc, relay bool) (EgressEndpoint, error)

// EgressEndpoint is where a container's connections reach its boundary.
type EgressEndpoint struct {
	// Socket is the host path of the egress socket, alone in its
	// directory, which the container mounts. It is empty with a relay.
	Socket string
	// RelayAddr is the tcp:// or unix:// address a relay connects to, and
	// RelayKey the key it sends. RelayMount is the host directory of a
	// unix:// address, which the relay mounts at /run/a2a-relay.
	RelayAddr, RelayKey, RelayMount string
	// Close stops the boundary and closes its connections.
	Close func() error
}

// Launcher returns the host path of the binary that starts a container
// with the server as its only network path, or why there is none.
func Launcher() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding this a2a-serve: %w", err)
	}
	return CheckLauncher(exe)
}

// CheckLauncher returns the path of the a2a-serve binary exe with symlinks
// resolved when Docker can run it in a container as the launcher: on a
// Linux host, statically linked and built without cgo.
func CheckLauncher(exe string) (string, error) {
	exe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("finding this a2a-serve: %w", err)
	}
	if err := checkStatic(exe); err != nil {
		return "", err
	}
	return exe, nil
}

// checkStatic refuses a binary that needs a dynamic loader, which the
// container's image may lack, or that was built with cgo, which can't
// drop capabilities from all of its threads.
func checkStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("closing "+path, "error", err)
		}
	}()
	cgo := fmt.Errorf("%s was built with cgo; install a release build, or build it with CGO_ENABLED=0", path)
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return cgo
		}
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading the build of %s: %w", path, err)
	}
	for _, s := range info.Settings {
		if s.Key == "CGO_ENABLED" && s.Value == "1" {
			return cgo
		}
	}
	return nil
}

// EgressEnv is how to check this machine for the egress boundary.
type EgressEnv struct {
	// GOOS is the system the server runs on.
	GOOS string
	// Launcher checks the binary that would run as the containers' PID 1,
	// as [Launcher] does.
	Launcher func() (string, error)
	// TUN checks the host's TUN device, as [CheckTUN] does.
	TUN func() error
	// ImageArch is the CPU architecture of the agent's image, or "" when
	// it isn't known.
	ImageArch string
	// ForceRelay gives Linux containers a relay as with Docker Desktop.
	ForceRelay bool
	// RelayImage checks the image of the relay containers, as
	// [CheckRelayImage] does; nil skips the check. A missing image is a
	// warning, since the next start pulls or builds it.
	RelayImage func() error
}

// HostEgressEnv returns the environment of this process, with an image
// of unknown architecture.
func HostEgressEnv() EgressEnv {
	return EgressEnv{GOOS: runtime.GOOS, Launcher: Launcher, TUN: CheckTUN, ForceRelay: os.Getenv(EnvForceRelay) == "1", RelayImage: func() error {
		ctx, cancel := context.WithTimeout(context.Background(), DockerTimeout)
		defer cancel()
		return CheckRelayImage(ctx, func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}, false)
	}}
}

// Capabilities are facts about this machine, which the caller combines
// with its config to decide about the egress boundary.
type Capabilities struct {
	// Boundary is nil when the egress boundary can work, and otherwise
	// wraps [ErrNoBoundary] with the reason.
	Boundary error
	// EgressRelay is set when an egress container next to each container
	// carries its sockets, as Docker Desktop can't mount host sockets.
	EgressRelay bool
	// RelayImage is the problem of the egress container's image, or nil. A
	// [*MissingRelayImage] is one the next start pulls or builds.
	RelayImage error
}

// Probe runs the quick checks of this machine in env, without starting a
// container.
func (env EgressEnv) Probe() Capabilities {
	if env.GOOS != "linux" || env.ForceRelay {
		c := Capabilities{EgressRelay: true}
		if env.RelayImage != nil {
			c.RelayImage = env.RelayImage()
		}
		return c
	}
	if why := egressProblem(env); why != "" {
		return Capabilities{Boundary: &boundaryError{why: why}}
	}
	return Capabilities{}
}

// boundaryError says why the egress boundary can't work on this machine.
type boundaryError struct{ why string }

func (e *boundaryError) Error() string { return e.why }

func (e *boundaryError) Unwrap() error { return ErrNoBoundary }

// egressProblem returns why the socket of the boundary can't be mounted
// into the containers in env, or "".
func egressProblem(env EgressEnv) string {
	if _, err := env.Launcher(); err != nil {
		return err.Error()
	}
	if env.ImageArch != "" && env.ImageArch != runtime.GOARCH {
		return fmt.Sprintf("the agent's image is for %s and this a2a-serve for %s", env.ImageArch, runtime.GOARCH)
	}
	if err := env.TUN(); err != nil {
		return err.Error()
	}
	return ""
}

// tunDevice is the host's TUN device, which the container gets.
const tunDevice = "/dev/net/tun"

// CheckTUN checks that the host has a TUN device for the containers.
func CheckTUN() error {
	if _, err := os.Stat(tunDevice); err != nil {
		return fmt.Errorf("this machine has no %s; load the tun kernel module", tunDevice)
	}
	return nil
}

// ImageConfig returns the architecture and the entrypoint of image.
func ImageConfig(ctx context.Context, image string) (arch string, entrypoint []string, err error) {
	out, err := DockerQuery(ctx, "image", "inspect", "--format", "{{.Architecture}} {{json .Config.Entrypoint}}", image)
	if err != nil {
		return "", nil, fmt.Errorf("docker image inspect %s: %w", image, err)
	}
	arch, ep, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if err := json.Unmarshal([]byte(ep), &entrypoint); err != nil {
		return "", nil, fmt.Errorf("reading the entrypoint of %s: %w", image, err)
	}
	return arch, entrypoint, nil
}

// HostGatewayFunc returns the address that a connection from a container
// to host.docker.internal on port reaches, or false when there is none.
type HostGatewayFunc func(ctx context.Context, port uint16) (netip.AddrPort, bool)

// HostGateway returns what host.docker.internal reaches from a container
// on goos without a rule that names it: the ports that containers publish
// on this host, which are on its loopback. On Linux, these are the ports
// published on every address, which Docker's own network reaches too; a
// port published on loopback keeps a service private to the machine. With
// Docker Desktop, host.docker.internal reaches loopback ports anyway.
func HostGateway(goos string) HostGatewayFunc {
	return (&publishedPorts{loopback: goos != "linux"}).lookup
}

// publishedPorts finds the ports that containers publish on this host,
// asking docker at most once per publishedPortsTTL, failures included.
// Concurrent lookups share one docker call. With loopback, ports published
// only on loopback count too.
type publishedPorts struct {
	loopback bool
	calls    singleflight.Group

	mu  sync.Mutex
	at  time.Time
	out string
	err error
}

// publishedPortsTTL is how long the ports containers publish are reused.
const publishedPortsTTL = 5 * time.Second

// lookup returns the loopback address of port when a container publishes
// it on this host.
func (p *publishedPorts) lookup(ctx context.Context, port uint16) (netip.AddrPort, bool) {
	out, err := p.ports(ctx)
	if err != nil || !publishes(out, port, p.loopback) {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), port), true
}

func (p *publishedPorts) ports(ctx context.Context) (string, error) {
	p.mu.Lock()
	if !p.at.IsZero() && time.Since(p.at) < publishedPortsTTL {
		out, err := p.out, p.err
		p.mu.Unlock()
		return out, err
	}
	p.mu.Unlock()
	ch := p.calls.DoChan("ps", func() (any, error) {
		qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DockerTimeout)
		defer cancel()
		out, err := DockerQuery(qctx, "ps", "--format", "{{.Ports}}")
		if err != nil {
			slog.Warn("listing the ports containers publish", "error", err)
		}
		p.mu.Lock()
		p.at, p.out, p.err = time.Now(), string(out), err
		p.mu.Unlock()
		return string(out), err
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			return "", r.Err
		}
		return r.Val.(string), nil
	}
}

// publishes reports whether the Ports column of docker ps, such as
// "0.0.0.0:3000->3000/tcp, [::]:3000->3000/tcp", publishes port over TCP
// on every address, or with loopback on loopback.
func publishes(ports string, port uint16, loopback bool) bool {
	for entry := range strings.FieldsFuncSeq(ports, func(r rune) bool { return r == ',' || r == '\n' }) {
		host, target, ok := strings.Cut(strings.TrimSpace(entry), "->")
		if !ok || !strings.HasSuffix(target, "/tcp") {
			continue
		}
		ap, err := netip.ParseAddrPort(host)
		if err != nil || ap.Port() != port {
			continue
		}
		if a := ap.Addr(); a.IsUnspecified() || loopback && a.IsLoopback() {
			return true
		}
	}
	return false
}
