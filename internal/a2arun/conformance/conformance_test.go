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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/internal/a2arun/hosttools"
	"github.com/a2aproject/a2a-serve/internal/a2arun/refhost"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

var (
	egressProxy   = flag.String("egress-proxy", "", "egress proxy URL to test, http://host:port or unix:///path (default: the reference host)")
	intercept     = flag.String("intercept", "", "host:port that the egress proxy intercepts")
	refuse        = flag.String("refuse", "", "host:port that the egress proxy refuses")
	forward       = flag.Bool("forward", false, "the egress proxy forwards plain HTTP requests to 127.0.0.1; needs -allow-loopback")
	allowLoopback = flag.Bool("allow-loopback", false, "the egress proxy means to reach this host's loopback")
	tools         = flag.String("tools", "", "tools endpoint to test: http://, https:// or unix:///path (default: the reference host)")
	workspace     = flag.String("workspace", "", "workspace file server to test: http://, https:// or unix:///path (default: the reference host)")
)

var allHooks = hosttools.HookSupport{BeforeTool: true, AfterTool: true}

func TestConformance(t *testing.T) {
	if *egressProxy != "" || *tools != "" || *workspace != "" {
		Run(t, Config{EgressProxy: *egressProxy, Intercept: *intercept, Refuse: *refuse, Forward: *forward, AllowLoopback: *allowLoopback, Tools: *tools, Workspace: *workspace})
		return
	}
	t.Run("reference host", func(t *testing.T) {
		url := serveRefhost(t, false, refhost.ToolsConfig{Hooks: allHooks})
		Run(t, Config{EgressProxy: url, Intercept: "api.anthropic.com:443", Refuse: "blocked.example:443", Tools: url, Workspace: url})
	})
	t.Run("file server over HTTP", func(t *testing.T) {
		srv := httptest.NewServer(fsys.Handler(fsys.Memory()))
		t.Cleanup(srv.Close)
		Run(t, Config{Workspace: srv.URL + "/"})
	})
	t.Run("reference host reaching loopback", func(t *testing.T) {
		Run(t, Config{EgressProxy: serveRefhost(t, true, refhost.ToolsConfig{}), Forward: true, AllowLoopback: true})
	})
	t.Run("reference host with event streams", func(t *testing.T) {
		Run(t, Config{Tools: serveRefhost(t, false, refhost.ToolsConfig{Hooks: allHooks, Stream: true})})
	})
	t.Run("reference host without hooks", func(t *testing.T) {
		Run(t, Config{Tools: serveRefhost(t, false, refhost.ToolsConfig{})})
	})
}

// serveRefhost serves the reference host on a Unix socket, with its egress
// proxy refusing blocked.example, and private destinations unless
// allowPrivate, its tools endpoint of tools, and a workspace in memory. It
// returns its URL.
func serveRefhost(t *testing.T, allowPrivate bool, tools refhost.ToolsConfig) string {
	t.Helper()
	h := refhost.New(refhost.Config{
		AllowPrivate: allowPrivate,
		Decide: func(c refhost.Connect) refhost.Decision {
			if c.Target == "blocked.example:443" {
				return refhost.Refuse("blocked.example is off limits")
			}
			return refhost.Intercept()
		},
		Tools:     refhost.NewTools(tools),
		Workspace: fsys.Handler(fsys.Memory()),
	})
	sock := filepath.Join(testutil.ShortTempDir(t), "host.sock")
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

func TestLongPollCheckFailsAServerThatIgnoresWait(t *testing.T) {
	t.Parallel()
	h := fsys.Handler(fsys.Memory())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		q.Set("wait", "0")
		r.URL.RawQuery = q.Encode()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f, err := newFileServer(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	if err := checkLongPoll(t.Context(), f); err == nil || !strings.Contains(err.Error(), "want it to wait") {
		t.Fatalf("checkLongPoll() of a server that ignores wait error = %v, want it to say the server didn't wait", err)
	}
}
