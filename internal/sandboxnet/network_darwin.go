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
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"time"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// DefaultRoute implements [Network]: the kernel's route to addr must leave
// through the interface and gateway of its route to a public probe
// address, as through a full-tunnel VPN too. The LAN, whose routes have a
// link-layer gateway, a split-tunnel VPN and other interfaces fail.
func (hostNetwork) DefaultRoute(addr netip.Addr) (bool, error) {
	addr = addr.Unmap()
	probe := internetProbe4
	if addr.Is6() {
		probe = internetProbe6
	}
	got, ok, err := getRoute(addr)
	if err != nil || !ok {
		return false, err
	}
	want, ok, err := getRoute(probe)
	if err != nil || !ok {
		return false, err
	}
	return got == want, nil
}

// CheckInternetRoute checks that this host has a route to the internet,
// without which the sandbox reaches no public host.
func CheckInternetRoute() error {
	if _, ok, err := getRoute(internetProbe4); err != nil || !ok {
		return errors.Join(errNoInternetRoute, err)
	}
	return nil
}

// darwinRoute is what a route says about where traffic leaves.
type darwinRoute struct {
	index   int
	gateway string
}

var routeSeq atomic.Int32

// getRoute asks the kernel for its route to addr with RTM_GET, or returns
// false when it has none.
func getRoute(addr netip.Addr) (darwinRoute, bool, error) {
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return darwinRoute{}, false, err
	}
	defer func() {
		if err := unix.Close(fd); err != nil {
			slog.Debug("closing a routing socket", "error", err)
		}
	}()
	tv := unix.NsecToTimeval(time.Second.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return darwinRoute{}, false, err
	}
	var dst route.Addr
	if ip, v6 := routeDestination(addr); v6 {
		dst = &route.Inet6Addr{IP: [16]byte(ip)}
	} else {
		dst = &route.Inet4Addr{IP: [4]byte(ip)}
	}
	seq := int(routeSeq.Add(1))
	pid := uintptr(unix.Getpid())
	req := route.RouteMessage{Version: unix.RTM_VERSION, Type: unix.RTM_GET, Flags: unix.RTF_UP | unix.RTF_HOST, ID: pid, Seq: seq, Addrs: []route.Addr{unix.RTAX_DST: dst}}
	b, err := req.Marshal()
	if err != nil {
		return darwinRoute{}, false, err
	}
	if _, err := unix.Write(fd, b); err != nil {
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) {
			return darwinRoute{}, false, nil
		}
		return darwinRoute{}, false, err
	}
	buf := make([]byte, 4096)
	// The socket also gets the kernel's other routing messages.
	for range 64 {
		n, err := unix.Read(fd, buf)
		if err != nil {
			return darwinRoute{}, false, err
		}
		msgs, err := route.ParseRIB(route.RIBTypeRoute, buf[:n])
		if err != nil {
			continue
		}
		for _, msg := range msgs {
			m, ok := msg.(*route.RouteMessage)
			if !ok || m.Seq != seq || m.ID != pid {
				continue
			}
			if m.Err != nil {
				return darwinRoute{}, false, nil
			}
			r := darwinRoute{index: m.Index}
			if len(m.Addrs) > unix.RTAX_GATEWAY && m.Flags&unix.RTF_GATEWAY != 0 {
				r.gateway = addrKey(m.Addrs[unix.RTAX_GATEWAY])
			}
			return r, true, nil
		}
	}
	return darwinRoute{}, false, errors.New("the kernel didn't answer RTM_GET")
}

func addrKey(a route.Addr) string {
	switch a := a.(type) {
	case *route.Inet4Addr:
		return netip.AddrFrom4(a.IP).String()
	case *route.Inet6Addr:
		return netip.AddrFrom16(a.IP).String()
	case *route.LinkAddr:
		return fmt.Sprintf("link %d %x", a.Index, a.Addr)
	case nil:
		return ""
	}
	return fmt.Sprintf("%T %v", a, a)
}
