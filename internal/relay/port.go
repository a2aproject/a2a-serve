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

package relay

import (
	"errors"
	"math/rand/v2"
	"net"
	"strconv"
	"syscall"
)

// PortTaken reports whether another process listens on port. FreePort
// can't reserve the port it returns.
func PortTaken(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return errors.Is(err, syscall.EADDRINUSE)
	}
	closeLogged(ln)
	return false
}

// Ports from freePortMin up to freePortMax lie below the ephemeral ranges of
// Linux, macOS and Windows, and below the ports the agentsrv tests give their
// agents, which run next to the relay tests.
const (
	freePortMin = 10000
	freePortMax = 20000
)

// FreePort returns a free loopback port for a backend, which listens on it
// after it starts. The system hands out ephemeral ports to outgoing
// connections and to listeners on port 0 too, so one of those would often be
// taken by then; ports below the ephemeral range are only taken by another
// program that listens on that exact port.
func FreePort() (int, error) {
	for range 20 {
		port := freePortMin + rand.IntN(freePortMax-freePortMin)
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		return port, ln.Close()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return port, ln.Close()
}
