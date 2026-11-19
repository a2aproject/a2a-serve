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

package a2arun_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/a2aproject/a2a-serve/internal/a2arun/hosttools"
	"github.com/a2aproject/a2a-serve/internal/a2arun/refhost"
	"github.com/a2aproject/a2a-serve/internal/a2arun/serve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

func TestToolsReachTheHostThroughTheBoundary(t *testing.T) {
	t.Parallel()
	bin, image := dockerTestEnv(t)
	tests := []struct {
		name        string
		egressProxy bool
		wantEgress  string
	}{
		{name: "egress policy", wantEgress: serve.EgressPolicy},
		{name: "egress proxy", egressProxy: true, wantEgress: serve.EgressProxy},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tools := refhost.NewTools(refhost.ToolsConfig{Hooks: hosttools.HookSupport{BeforeTool: true, AfterTool: true}})
			host := refhost.New(refhost.Config{Tools: tools})
			dir := testutil.ShortTempDir(t)
			url := serveHost(t, host, dir)
			sock := filepath.Join(dir, "a2a.sock")
			args := []string{"--sandbox", "docker", "--image", image, "--command", "/usr/local/bin/fakeacp",
				"--workspace", "dir:" + t.TempDir(), "--tools", url, "--listen", "unix://" + sock, "--max-processes", "1"}
			if tc.egressProxy {
				args = append(args, "--egress-proxy", url)
			} else {
				args = append(args, "--egress", "allowlist")
			}
			if ready := startBinary(t, bin, args...); ready.Egress != tc.wantEgress {
				t.Fatalf("ready line egress = %q, want %q", ready.Egress, tc.wantEgress)
			}
			c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
			if err != nil {
				t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
			}

			task := sendTask(t, c, "context-a", "mcpcall host echo through the boundary")
			refused := sendTask(t, c, "context-a", "dial "+serve.ToolsHost+":81")

			if got, want := testutil.AllArtifactText(task), "mcp result: echo: through the boundary"; got != want {
				t.Fatalf("Client.SendMessage(mcpcall) = %q, want %q", got, want)
			}
			if got := len(tools.Records("tools/call")); got != 1 {
				t.Fatalf("host got %d tools/call, want 1", got)
			}
			if got := len(tools.Records(hosttools.MethodBeforeTool)); got != 1 {
				t.Fatalf("host got %d hooks/beforeTool, want 1", got)
			}
			waitRecords(t, tools, hosttools.MethodAfterTool, 1)
			if text := testutil.AllArtifactText(refused); !strings.Contains(text, "dial error") {
				t.Fatalf("Client.SendMessage(dial %s:81) = %q, want a dial error", serve.ToolsHost, text)
			}
			for _, c := range host.Connects() {
				if strings.HasPrefix(c.Target, serve.ToolsHost) {
					t.Fatalf("the egress proxy got CONNECT %s, want the tools endpoint's traffic kept from it", c.Target)
				}
			}
			for _, r := range host.Requests() {
				if strings.Contains(r.URL, serve.ToolsHost) {
					t.Fatalf("the egress proxy got the request %s, want the tools endpoint's traffic kept from it", r.URL)
				}
			}
		})
	}
}
