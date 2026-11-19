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

package conformance

import (
	"context"
	"flag"
	"net"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/a2arun/refhost"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

var (
	egressProxy   = flag.String("egress-proxy", "", "egress proxy URL to test, http://host:port or unix:///path (default: the reference host)")
	intercept     = flag.String("intercept", "", "host:port that the egress proxy intercepts")
	refuse        = flag.String("refuse", "", "host:port that the egress proxy refuses")
	forward       = flag.Bool("forward", false, "the egress proxy forwards plain HTTP requests to 127.0.0.1; needs -allow-loopback")
	allowLoopback = flag.Bool("allow-loopback", false, "the egress proxy means to reach this host's loopback")
)

func TestConformance(t *testing.T) {
	if *egressProxy != "" {
		Run(t, Config{EgressProxy: *egressProxy, Intercept: *intercept, Refuse: *refuse, Forward: *forward, AllowLoopback: *allowLoopback})
		return
	}
	t.Run("reference host", func(t *testing.T) {
		Run(t, Config{EgressProxy: serveRefhost(t, false), Intercept: "api.anthropic.com:443", Refuse: "blocked.example:443"})
	})
	t.Run("reference host reaching loopback", func(t *testing.T) {
		Run(t, Config{EgressProxy: serveRefhost(t, true), Forward: true, AllowLoopback: true})
	})
}

// serveRefhost serves the reference host on a Unix socket, refusing
// blocked.example, and private destinations unless allowPrivate, and
// returns its URL.
func serveRefhost(t *testing.T, allowPrivate bool) string {
	t.Helper()
	h := refhost.New(refhost.Config{AllowPrivate: allowPrivate, Decide: func(c refhost.Connect) refhost.Decision {
		if c.Target == "blocked.example:443" {
			return refhost.Refuse("blocked.example is off limits")
		}
		return refhost.Intercept()
	}})
	sock := filepath.Join(testutil.ShortTempDir(t), "egress.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Host.Serve() error = %v", err)
		}
	})
	return "unix://" + sock
}
