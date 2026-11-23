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

package sidecar_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/a2aproject/a2a-serve/internal/sidecar/serve"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

func TestEgressInjectHeaderInDockerSandbox(t *testing.T) {
	t.Parallel()
	bin, image := dockerTestEnv(t)
	dir := testutil.ShortTempDir(t)
	sock := filepath.Join(dir, "a2a.sock")

	ready := startBinaryWithEnv(t, bin, []string{"GH_TOKEN=super-secret-gh-token"},
		"--sandbox", dockerSandbox(), "--image", image, "--command", "/usr/local/bin/fakeacp",
		"--workspace", "dir:"+t.TempDir(), "--listen", "unix://"+sock,
		"--egress", "allowlist",
		"--egress-inject-header", "api.github.com=Authorization:Bearer env:GH_TOKEN",
	)
	if ready.Egress != serve.EgressPolicy || !strings.HasPrefix(ready.Sandbox, dockerSandbox()) {
		t.Fatalf("ready line = %+v, want egress %q in the sandbox %s", ready, serve.EgressPolicy, dockerSandbox())
	}
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface("http://a2a/", a2a.TransportProtocolJSONRPC)}, a2aclient.WithJSONRPCTransport(unixClient(sock)))
	if err != nil {
		t.Fatalf("a2aclient.NewFromEndpoints() error = %v", err)
	}

	ghToken := sendTask(t, c, "ctx-hdr", "env GH_TOKEN")
	refused := sendTask(t, c, "ctx-hdr", "fetch https://blocked.test/")

	if got := testutil.AllArtifactText(ghToken); !strings.Contains(got, "a2a-serve-placeholder") || strings.Contains(got, "super-secret-gh-token") {
		t.Fatalf("env GH_TOKEN = %q, want placeholder without secret", got)
	}
	if got := testutil.AllArtifactText(refused); !strings.HasPrefix(got, "fetch error:") {
		t.Fatalf("fetch of a refused host = %q, want an error", got)
	}
}
