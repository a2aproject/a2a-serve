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

package harness

import (
	"context"
	"debug/buildinfo"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/egress"
)

// OpenEgress opens the egress socket of a container: a Unix socket alone
// in its directory, whose connections the server decides by destination.
// close stops it and closes its connections.
// Connections to host.docker.internal reach what hostGateway returns.
type OpenEgress func(hostGateway HostGatewayFunc) (socket string, close func() error, err error)

// UnrestrictedEffect says what a sandbox without the egress boundary reaches.
const UnrestrictedEffect = "the agent reaches every network this machine reaches, the LAN and the tailnet included"

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
	if runtime.GOOS != "linux" {
		return "", errors.New("with Docker Desktop, the sandbox can't connect to the server")
	}
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

// EgressWarning returns why the egress settings of an agent's config don't
// apply as set with the launcher, such as [Launcher], or "".
func EgressWarning(cfg *agentdir.Config, launcher func() (string, error)) string {
	if cfg.Sandbox == SandboxLocal {
		if cfg.Egress.Mode != "" || len(cfg.Egress.Allow) > 0 {
			return "--egress and --egress-allow have no effect with --sandbox local: " + UnrestrictedEffect + "; use --sandbox docker, or remove the rules with a2a-serve config unset egress-allow"
		}
		return ""
	}
	if cfg.Egress.Mode == egress.ModeUnrestricted {
		return ""
	}
	if _, err := launcher(); err != nil {
		if cfg.Egress.Mode == egress.ModeAllowlist {
			return noAllowlist(err.Error())
		}
		return noBoundary(err.Error())
	}
	if cfg.Egress.Mode == egress.ModeAllowlist && len(cfg.Egress.Allow) == 0 {
		return "--egress allowlist with no --egress-allow rules: the agent can't reach its model provider"
	}
	return ""
}

// noBoundary says that a container gets Docker's network, and why.
func noBoundary(why string) string {
	return "egress control is off: " + why + ". " + capitalize(UnrestrictedEffect)
}

// noAllowlist says why a container with --egress allowlist can't start.
func noAllowlist(why string) string {
	return "--egress allowlist can't apply: " + why + "; run the server on Linux with a release build, or use --egress internet"
}

func capitalize(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

// imageConfig returns the architecture and the entrypoint of image.
func imageConfig(ctx context.Context, image string) (arch string, entrypoint []string, err error) {
	out, err := dockerQuery(ctx, dockerTimeout, "image", "inspect", "--format", "{{.Architecture}} {{json .Config.Entrypoint}}", image)
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

// hostGateway returns what host.docker.internal reaches from this
// Docker's containers. Rootful Docker puts the bridge gateway on this
// host, and a container reaches the host's services through it. Rootless
// Docker's bridge is in a namespace of its own, where a container reaches
// only the ports other containers publish, which are on this host's
// loopback.
func hostGateway(ctx context.Context, rootless bool) HostGatewayFunc {
	if rootless {
		return (&publishedPorts{}).lookup
	}
	out, err := dockerQuery(ctx, dockerTimeout, "network", "inspect", "bridge", "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	if err != nil {
		slog.Warn("finding Docker's bridge gateway; containers can't reach host.docker.internal", "error", err)
		return nil
	}
	for f := range strings.FieldsSeq(string(out)) {
		gw, err := netip.ParseAddr(f)
		if err != nil || !isLocalAddr(gw) {
			continue
		}
		return func(_ context.Context, port uint16) (netip.AddrPort, bool) {
			return netip.AddrPortFrom(gw, port), true
		}
	}
	return nil
}

func isLocalAddr(a netip.Addr) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, ia := range addrs {
		if p, err := netip.ParsePrefix(ia.String()); err == nil && p.Addr() == a {
			return true
		}
	}
	return false
}

// publishedPorts finds the ports that containers publish on this host,
// asking docker at most once per publishedPortsTTL.
type publishedPorts struct {
	mu      sync.Mutex
	at      time.Time
	ports   string
	queried bool
}

// publishedPortsTTL is how long the ports containers publish are reused.
const publishedPortsTTL = 5 * time.Second

// lookup returns the loopback address of port when a container publishes
// it on this host.
func (p *publishedPorts) lookup(ctx context.Context, port uint16) (netip.AddrPort, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.queried || time.Since(p.at) > publishedPortsTTL {
		out, err := dockerQuery(ctx, dockerTimeout, "ps", "--format", "{{.Ports}}")
		if err != nil {
			slog.Warn("listing the ports containers publish", "error", err)
			return netip.AddrPort{}, false
		}
		p.ports, p.at, p.queried = string(out), time.Now(), true
	}
	if !publishes(p.ports, port) {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), port), true
}

// publishes reports whether the Ports column of docker ps, such as
// "0.0.0.0:3000->3000/tcp, [::]:3000->3000/tcp", publishes port over TCP
// on every address or on loopback.
func publishes(ports string, port uint16) bool {
	for entry := range strings.FieldsFuncSeq(ports, func(r rune) bool { return r == ',' || r == '\n' }) {
		host, target, ok := strings.Cut(strings.TrimSpace(entry), "->")
		if !ok || !strings.HasSuffix(target, "/tcp") {
			continue
		}
		ap, err := netip.ParseAddrPort(host)
		if err != nil || ap.Port() != port {
			continue
		}
		if a := ap.Addr(); a.IsUnspecified() || a.IsLoopback() {
			return true
		}
	}
	return false
}
