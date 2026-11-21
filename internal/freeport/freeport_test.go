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

package freeport_test

import (
	"net"
	"strconv"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/freeport"
)

func TestListenIsBelowTheEphemeralRange(t *testing.T) {
	t.Parallel()
	ln, err := freeport.Listen("127.0.0.1")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing the listener: %v", err)
		}
	})
	if port := ln.Addr().(*net.TCPAddr).Port; port < freeport.Min || port >= freeport.Max {
		t.Fatalf("Listen() port = %d, want in [%d, %d)", port, freeport.Min, freeport.Max)
	}
}

func TestPickIsFreeAndBelowTheEphemeralRange(t *testing.T) {
	t.Parallel()
	port, err := freeport.Pick()
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if port < freeport.Min || port >= freeport.Max {
		t.Fatalf("Pick() = %d, want in [%d, %d)", port, freeport.Min, freeport.Max)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("Listen() on the port from Pick() error = %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
}
