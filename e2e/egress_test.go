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

//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-serve/internal/sandboxnet"
)

// publicTarget is a public host the sandbox reaches, overridable for
// networks that block it.
func publicTarget() string {
	if t := os.Getenv("E2E_PUBLIC_TARGET"); t != "" {
		return t
	}
	return "example.com:80"
}

func TestDockerSandboxEgress(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	owner := newUser(t)
	coder := owner.startAgent("coder")
	requireBoundary(t, coder)

	if got := resultOf(owner.mustSend(coder.url, "dial "+publicTarget())).Text; got != "dial ok" {
		t.Fatalf("a2a send dial %s = %q, want the public host reached", publicTarget(), got)
	}
	if relayImage != "" {
		out, err := exec.Command("docker", "ps", "-q", "--filter", "label=a2a.dir="+coder.dir, "--filter", "label=a2a.relay").Output()
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			t.Fatalf("docker ps of the relay container = %q, %v, want it running", out, err)
		}
	}

	port := hostListener(t)
	targets := []string{"192.168.255.1:80", "10.255.255.1:22", "169.254.169.254:80", "100.100.100.100:80"}
	lan, tailnet := hostAddrs(t)
	for _, a := range []netip.Addr{lan, tailnet} {
		if a.IsValid() {
			targets = append(targets, netip.AddrPortFrom(a, port).String())
		}
	}
	for _, target := range targets {
		if got := resultOf(owner.mustSend(coder.url, "dial "+target)).Text; !strings.HasPrefix(got, "dial error:") {
			t.Fatalf("a2a send dial %s = %q, want it refused", target, got)
		}
	}

	logs := coder.serve("logs", "-n", "200")
	for _, target := range targets {
		if !strings.Contains(logs, "destination="+target+" reason=") {
			t.Fatalf("a2a-serve logs = %q, want the blocked connection to %s", logs, target)
		}
	}
	if st := coder.serve("status"); !strings.Contains(st, fmt.Sprintf("%d connection(s) blocked", len(targets))) {
		t.Fatalf("a2a-serve status = %q, want %d blocked connections", st, len(targets))
	}

	if !lan.IsValid() {
		t.Log("this host has no LAN address; skipping --egress-allow of a LAN host")
		return
	}
	allowed := netip.AddrPortFrom(lan, port).String()
	coder.serve("config", "set", "--egress-allow", allowed)
	if got := resultOf(owner.mustSend(coder.url, "dial "+allowed)).Text; got != "dial ok" {
		t.Fatalf("a2a send dial %s after --egress-allow = %q, want it reached", allowed, got)
	}
}

func TestDockerSandboxDropsCapabilities(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	owner := newUser(t)
	coder := owner.startAgent("coder")

	requireBoundary(t, coder)
	uid := os.Getuid()
	if rootlessDocker(t) {
		uid = 0
	}
	want := fmt.Sprintf("uid=%d CapEff=0000000000000000 CapBnd=0000000000000000 tun=operation not permitted raw=operation not permitted", uid)
	if got := resultOf(owner.mustSend(coder.url, "caps")).Text; got != want {
		t.Fatalf("a2a send caps = %q, want %q", got, want)
	}
}

// hostListener accepts connections on every address of this host and
// returns its port.
func hostListener(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing the host listener error = %v", err)
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if err := c.Close(); err != nil {
				return
			}
		}
	}()
	_, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	return uint16(n)
}

// hostAddrs returns a private LAN address and a tailnet address of this
// host, each invalid when the host has none.
func hostAddrs(t *testing.T) (lan, tailnet netip.Addr) {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces() error = %v", err)
	}
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || strings.HasPrefix(ifc.Name, "docker") || strings.HasPrefix(ifc.Name, "br-") || strings.HasPrefix(ifc.Name, "veth") {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			p, err := netip.ParsePrefix(a.String())
			if err != nil || !p.Addr().Is4() {
				continue
			}
			switch ip := p.Addr(); {
			case cgnat.Contains(ip) && !tailnet.IsValid():
				tailnet = ip
			case ip.IsPrivate() && !lan.IsValid():
				lan = ip
			}
		}
	}
	return lan, tailnet
}

func rootlessDocker(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("docker", "info", "--format", "{{.SecurityOptions}}").Output()
	if err != nil {
		t.Fatalf("docker info error = %v", err)
	}
	return bytes.Contains(out, []byte("rootless"))
}

func TestDockerSandboxEgressContainerComesBack(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	if relayImage == "" {
		t.Skip("needs A2A_SERVE_SANDBOX_RELAY=1")
	}
	owner := newUser(t)
	coder := owner.startAgent("coder")
	if got := resultOf(owner.mustSend(coder.url, "dial "+publicTarget())).Text; got != "dial ok" {
		t.Fatalf("a2a send dial %s = %q, want the public host reached", publicTarget(), got)
	}
	out, err := exec.Command("docker", "ps", "-q", "--filter", "label=a2a.dir="+coder.dir, "--filter", "label=a2a.relay").Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		t.Fatalf("docker ps of the egress container = %q, %v, want it running", out, err)
	}
	if out, err := exec.Command("docker", append([]string{"rm", "-f", "-v"}, strings.Fields(string(out))...)...).CombinedOutput(); err != nil {
		t.Fatalf("docker rm of the egress container error = %v: %s", err, out)
	}
	if crashed := owner.mustSend(coder.url, "crash"); crashed.Status.State != a2a.TaskStateFailed {
		t.Fatalf("a2a send crash state = %v, want %v", crashed.Status.State, a2a.TaskStateFailed)
	}
	if got := resultOf(owner.mustSend(coder.url, "dial "+publicTarget())).Text; got != "dial ok" {
		t.Fatalf("a2a send dial %s after the egress container was removed = %q, want the public host reached", publicTarget(), got)
	}
}

func TestDockerSandboxEgressFallsBackWhenSetupFails(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	owner := newUser(t)
	env := owner.env
	owner.env = append(slices.Clone(env), sandboxnet.EnvFailSetup+"=all")
	coder := owner.newAgent("coder")
	t.Cleanup(coder.cleanup)
	out := coder.serve(append([]string{"start", "acp", "--name", "coder", "--detach"}, sandboxFlags()...)...)
	coder.url = strings.TrimSpace(coder.serve("status", "--url=local"))
	const warning = "egress control is off: the agent's container can't set up its network: the setup failed as the test asked"
	if !strings.Contains(out, warning) {
		t.Fatalf("a2a-serve start output = %q, want the warning %q", out, warning)
	}
	if st := coder.serve("status"); !strings.Contains(st, warning) {
		t.Fatalf("a2a-serve status = %q, want the warning %q", st, warning)
	}
	if got := resultOf(owner.mustSend(coder.url, "dial "+publicTarget())).Text; got != "dial ok" {
		t.Fatalf("a2a send dial %s with Docker's network = %q, want the public host reached", publicTarget(), got)
	}

	owner.env = env
	coder.serve("restart")
	if st := coder.serve("status"); strings.Contains(st, "egress control is off") || !strings.Contains(st, "private networks blocked") {
		t.Fatalf("a2a-serve status after a start without the failure = %q, want the boundary back", st)
	}
	if got := resultOf(owner.mustSend(coder.url, "caps")).Text; !strings.Contains(got, "CapBnd=0000000000000000") {
		t.Fatalf("a2a send caps after a start without the failure = %q, want no capabilities", got)
	}
}

func TestDockerSandboxEgressFallsBackWhenTheAgentsSetupFails(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	owner := newUser(t)
	owner.env = append(owner.env, sandboxnet.EnvFailSetup+"=agent")
	coder := owner.startAgent("coder")
	if got := resultOf(owner.mustSend(coder.url, "dial "+publicTarget())).Text; got != "dial ok" {
		t.Fatalf("a2a send dial %s with Docker's network = %q, want the public host reached", publicTarget(), got)
	}
	const warning = "egress control is off: the agent's container can't set up its network: the setup failed as the test asked"
	if st := coder.serve("status"); !strings.Contains(st, warning) {
		t.Fatalf("a2a-serve status = %q, want the warning %q", st, warning)
	}
}

// requireBoundary fails the test when the server turned egress control
// off, which tests of the boundary don't expect.
func requireBoundary(t *testing.T, a *agent) {
	t.Helper()
	if st := a.serve("status"); strings.Contains(st, "egress control is off") {
		t.Fatalf("a2a-serve status = %q, want the egress boundary on", st)
	}
}

// TestDockerSandboxLauncherRunsAsAnotherUser runs the launcher of an
// agent's container as a user that doesn't own it, with no capabilities,
// as container root runs it on rootful Docker.
func TestDockerSandboxLauncherRunsAsAnotherUser(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	if relayImage != "" {
		t.Skip("with an egress container, the launcher comes from its image")
	}
	owner := newUser(t)
	coder := owner.startAgent("coder")
	requireBoundary(t, coder)
	ids, err := exec.Command("docker", "ps", "-q", "--filter", "label=a2a.dir="+coder.dir).Output()
	if err != nil || len(bytes.TrimSpace(ids)) == 0 {
		t.Fatalf("docker ps of the agent's container = %q, %v", ids, err)
	}
	id := strings.Fields(string(ids))[0]
	src, err := exec.Command("docker", "inspect", "--format", `{{range .Mounts}}{{if eq .Destination "/run/a2a-init"}}{{.Source}}{{end}}{{end}}`, id).Output()
	if err != nil || len(bytes.TrimSpace(src)) == 0 {
		t.Fatalf("docker inspect of the launcher mount = %q, %v", src, err)
	}
	out, err := exec.Command("docker", "run", "--rm", "--user", "4242:4242", "--cap-drop", "ALL",
		"-v", strings.TrimSpace(string(src))+":/run/a2a-init:ro", "--entrypoint", "/run/a2a-init", fakeacpImage, "version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "a2a-serve") {
		t.Fatalf("running the launcher as another user = %q, %v, want its version", out, err)
	}
}
