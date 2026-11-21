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

// Package freeport picks loopback ports that stay free while their owner
// restarts. The system hands out ephemeral ports to outgoing connections
// and to listeners on port 0, so a program that listens on one again after
// it restarts often finds it taken. Ports below the ephemeral ranges are
// only taken by another program that listens on that exact port.
package freeport

import (
	"math/rand/v2"
	"net"
	"strconv"
)

// Ports from Min up to Max lie below the ephemeral ranges of Linux, macOS
// and Windows.
const (
	Min = 10000
	Max = 20000
)

// tries bounds the random ports Listen tries before it takes one of the
// system's.
const tries = 20

// Listen listens on a free TCP port of host from Min up to Max, or on one
// the system picks when it finds none.
func Listen(host string) (net.Listener, error) {
	for range tries {
		port := Min + rand.IntN(Max-Min)
		if ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port))); err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", net.JoinHostPort(host, "0"))
}

// Pick returns a loopback port that Listen would listen on, for a program
// that listens on it later. Another program can take it in between.
func Pick() (int, error) {
	ln, err := Listen("127.0.0.1")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return port, ln.Close()
}
