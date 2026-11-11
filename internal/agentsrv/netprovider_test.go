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

package agentsrv

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/harness"
	"github.com/a2aproject/a2a-serve/internal/netinstall"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

// findLoopnet finds the loopnet provider for the network URLs.
func findLoopnet(context.Context, string) (netinstall.Provider, error) {
	return netinstall.Provider{Name: "loopnet", Path: loopnet}, nil
}

func onLoopnet(url string) func(*agentdir.Config) {
	return func(c *agentdir.Config) {
		c.Network.Private, c.Network.Provider = url, loopnet
	}
}

func TestNetworkProviderCallsBetweenAgents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	net := "loopnet://" + testutil.ShortTempDir(t) + "?owner=ben@example.com"
	startServer(t, serverOpts{name: "reviewer", resolveNetwork: findLoopnet, configure: onLoopnet(net)})
	coder := startServer(t, serverOpts{name: "coder", resolveNetwork: findLoopnet, configure: onLoopnet(net)})

	ctl := controlapi.NewClient(coder.dir)
	st, err := ctl.Status(ctx)
	if err != nil {
		t.Fatalf("ctl.Status() error = %v", err)
	}
	if diff := cmp.Diff([]string{"http://loopnet.local/coder", "works only from other agents on loopnet"}, []string{st.Private, st.PrivateNote}); diff != "" {
		t.Fatalf("ctl.Status() wrong result (-want +got) diff = %s", diff)
	}
	peers, err := ctl.Peers(ctx)
	if err != nil {
		t.Fatalf("ctl.Peers() error = %v", err)
	}
	if len(peers) != 1 || peers[0].Name != "reviewer" || peers[0].URL != "http://loopnet.local/reviewer" {
		t.Fatalf("ctl.Peers() = %+v, want reviewer at http://loopnet.local/reviewer", peers)
	}
	if _, err := ctl.AddContact(ctx, controlapi.ContactRequest{Name: "reviewer"}); err != nil {
		t.Fatalf("ctl.AddContact() error = %v", err)
	}
	task := send(t, coder.client(coder.pair("me", auth.RoleOwner)), userText("call reviewer hello", "", ""))
	wantResult(t, task, a2a.TaskStateCompleted, "Hello from fakeacp.")
}

func TestNetworkProviderRefusedTokenFailsBeforeAgentStarts(t *testing.T) {
	t.Parallel()
	dir, err := agentdir.Open(testutil.ShortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &agentdir.Config{Name: "coder", Harness: "fake", Command: []string{fakeACP}, Sandbox: harness.SandboxLocal, Port: freePort(t)}
	onLoopnet("loopnet://" + testutil.ShortTempDir(t) + "?token=good")(cfg)
	if err := dir.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	out := &lockedBuffer{}

	err = Run(context.Background(), Options{
		Dir: dir, Config: cfg, Secrets: agentdir.Secrets{agentdir.SecretNetworkToken: "bad"}, ResolveNetwork: findLoopnet,
		Out: out, HarnessLog: io.Discard, Timings: testTimings,
	})

	var te *network.TokenError
	if !errors.As(err, &te) || !strings.Contains(te.Fix, "ask for a new token, then run `a2a-serve config set --network-token -`") {
		t.Fatalf("Run() error = %v, want a token error with the next step", err)
	}
	if strings.Contains(out.String(), "is running") {
		t.Fatalf("Run() output = %q, want the agent not started", out.String())
	}
	problem, _, err := network.TokenProblem(dir.NetworkStateDir("loopnet"))
	if err != nil || problem != "the network doesn't know this token" {
		t.Fatalf("network.TokenProblem() = %q, %v, want the refused token", problem, err)
	}
	if _, err := os.Stat(filepath.Join(dir.NetworkStateDir("loopnet"), "token-problem.json")); err != nil {
		t.Fatalf("token problem file: %v", err)
	}
}
