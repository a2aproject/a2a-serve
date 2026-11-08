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
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/aojea/agents.net/sdk/pkg/tun2connect"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The device and resolver addresses sit in the top /24 and /120 of the
// synthetic pools, which the virtual DNS never hands out. The prefix
// lengths give the kernel routes that cover the whole pools.
const (
	tunDevice      = "tun0"
	tunMTU         = 1500
	guestAddr4     = "100.127.255.254/10"
	guestAddr6     = "100::ffff:ffff:ffff:fffe/64"
	resolverAddr   = "100.127.255.253"
	resolvConfPath = "/etc/resolv.conf"
)

// Launch sets up the container's only route, a TUN device whose flows
// leave as CONNECT tunnels on the boundary socket, drops every capability,
// and runs the agent as cfg.UID and cfg.GID. It does the duties of PID 1
// and returns the agent's exit code.
func Launch(cfg LaunchConfig) (int, error) {
	if len(cfg.Argv) == 0 {
		return 0, errors.New("no command to run")
	}
	if err := checkInterfaces(); err != nil {
		return 0, err
	}
	fd, err := openTUN(tunDevice, tunMTU)
	if err != nil {
		return 0, err
	}
	if err := configureTUN(tunDevice); err != nil {
		return 0, fmt.Errorf("configuring %s: %w", tunDevice, err)
	}
	// The engine answers DNS on every address, so the image's resolver
	// still works when the file can't be written.
	if err := os.WriteFile(resolvConfPath, []byte("nameserver "+resolverAddr+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "a2a-init: pointing %s at the sandbox resolver: %v\n", resolvConfPath, err)
	}
	dev, err := tun2connect.NewTUNDevice(fd, tunMTU)
	if err != nil {
		return 0, fmt.Errorf("opening the link endpoint: %w", err)
	}
	var d net.Dialer
	eng, err := tun2connect.New(tun2connect.Config{
		Device: dev,
		Dialer: &tun2connect.BoundaryClient{DialBoundary: func(ctx context.Context) (net.Conn, error) {
			return d.DialContext(ctx, "unix", cfg.Boundary)
		}},
		DNS: tun2connect.NewVirtualDNS(),
	})
	if err != nil {
		return 0, err
	}
	defer eng.Close()
	if err := dropPrivileges(cfg.UID, cfg.GID); err != nil {
		return 0, fmt.Errorf("dropping privileges: %w", err)
	}
	cmd := exec.Command(cfg.Argv[0], cfg.Argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Signals that arrive while the agent starts wait in the channel.
	sig := make(chan os.Signal, 16)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGCHLD)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting %s: %w", cfg.Argv[0], err)
	}
	return supervise(cmd.Process.Pid, sig), nil
}

// checkInterfaces refuses a namespace with an interface besides loopback,
// which would be a second way out.
func checkInterfaces() error {
	ifaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("listing interfaces: %w", err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback == 0 {
			return fmt.Errorf("the container has the network interface %s; it must run with --network none", ifc.Name)
		}
	}
	return nil
}

func openTUN(name string, mtu uint32) (int, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("opening /dev/net/tun: %w", err)
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return -1, errors.Join(err, unix.Close(fd))
	}
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return -1, errors.Join(fmt.Errorf("creating %s: %w", name, err), unix.Close(fd))
	}
	link, err := netlink.LinkByName(name)
	if err == nil {
		err = netlink.LinkSetMTU(link, int(mtu))
	}
	if err == nil {
		err = unix.SetNonblock(fd, true)
	}
	if err != nil {
		return -1, errors.Join(err, unix.Close(fd))
	}
	return fd, nil
}

// configureTUN gives the device its addresses and makes it the default
// route, so that connections to literal addresses and to any DNS server
// reach the engine too. IPv6 is optional, for hosts that turn it off.
func configureTUN(name string) error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		return fmt.Errorf("bringing up loopback: %w", err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	if err := addAddr(link, guestAddr4); err != nil {
		return err
	}
	ipv6 := true
	if err := addAddr(link, guestAddr6); err != nil {
		fmt.Fprintf(os.Stderr, "a2a-init: continuing without IPv6: %v\n", err)
		ipv6 = false
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bringing up %s: %w", name, err)
	}
	if err := defaultRoute(link, &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}); err != nil {
		return err
	}
	if !ipv6 {
		return nil
	}
	if err := defaultRoute(link, &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}); err != nil {
		fmt.Fprintf(os.Stderr, "a2a-init: continuing without IPv6: %v\n", err)
	}
	return nil
}

func addAddr(link netlink.Link, cidr string) error {
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("adding %s: %w", cidr, err)
	}
	return nil
}

func defaultRoute(link netlink.Link, dst *net.IPNet) error {
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Scope: netlink.SCOPE_LINK, Dst: dst}); err != nil {
		return fmt.Errorf("default route for %s: %w", dst, err)
	}
	return nil
}

// dropPrivileges switches the launcher to uid and gid and drops every
// capability, so that the agent it starts inherits none and can't gain
// any. The launcher keeps the same user as the agent, which owns the
// boundary socket; it stays undumpable, so the agent can't trace it.
// Securebits stop uid 0 from gaining capabilities on exec, for rootless
// Docker, where the agent runs as uid 0. Capabilities belong to threads,
// so every change applies to all of them, which needs a build without cgo.
func dropPrivileges(uid, gid int) error {
	// From linux/securebits.h: each bit and its lock bit.
	const (
		noRoot              = 1<<0 | 1<<1
		noSetuidFixupLocked = 1 << 3
		keepCapsLocked      = 1 << 5
		noCapAmbientRaise   = 1<<6 | 1<<7
	)
	if err := prctlAll(unix.PR_SET_SECUREBITS, noRoot|noSetuidFixupLocked|keepCapsLocked|noCapAmbientRaise); err != nil {
		return fmt.Errorf("securebits: %w", err)
	}
	if err := prctlAll(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL); err != nil {
		return fmt.Errorf("ambient set: %w", err)
	}
	last, err := lastCap()
	if err != nil {
		return err
	}
	for c := 0; c <= last; c++ {
		if err := prctlAll(unix.PR_CAPBSET_DROP, uintptr(c)); err != nil {
			return fmt.Errorf("bounding set: %w", err)
		}
	}
	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid %d: %w", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}
	if err := clearCapabilities(); err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("dumpable: %w", err)
	}
	return checkNoCapabilities()
}

// checkNoCapabilities confirms that the process holds no capability.
func checkNoCapabilities() error {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return err
	}
	for line := range strings.Lines(string(status)) {
		for _, set := range []string{"CapInh:", "CapPrm:", "CapEff:", "CapBnd:", "CapAmb:"} {
			if v, ok := strings.CutPrefix(line, set); ok && strings.Trim(strings.TrimSpace(v), "0") != "" {
				return fmt.Errorf("%s is still %s", strings.TrimSuffix(set, ":"), strings.TrimSpace(v))
			}
		}
	}
	return nil
}

func prctlAll(option, arg uintptr) error {
	if _, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, option, arg, 0, 0, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

// clearCapabilities empties the capability sets of every thread.
func clearCapabilities() error {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if _, _, errno := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("capset: %w", errno)
	}
	return nil
}

func lastCap() (int, error) {
	b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// supervise reaps every child that ends, forwards the signals that end the
// container to the agent's process group, and returns the agent's exit code.
func supervise(agent int, sig <-chan os.Signal) int {
	for s := range sig {
		if s != syscall.SIGCHLD {
			if err := unix.Kill(-agent, s.(syscall.Signal)); err != nil && !errors.Is(err, unix.ESRCH) {
				fmt.Fprintf(os.Stderr, "a2a-init: forwarding %v: %v\n", s, err)
			}
			continue
		}
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			if pid != agent {
				continue
			}
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
	}
	return 0
}
