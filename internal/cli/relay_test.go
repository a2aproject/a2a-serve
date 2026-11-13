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

package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/backoff"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/relay/control"
	"github.com/a2aproject/a2a-serve/internal/relay/meshadmin"
	"github.com/a2aproject/a2a-serve/internal/relay/meshadmin/meshtest"
	"github.com/a2aproject/a2a-serve/internal/relay/meshbackend"
	"github.com/a2aproject/a2a-serve/internal/relaydir"
)

// testHTTP has a transport of its own: httptest.Server.Close closes the idle
// connections of http.DefaultTransport, which breaks requests that parallel
// tests send through it at the same time.
var testHTTP = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}

const relayAdminToken = "relay-admin"

// remoteRelay serves the control API of a relay at its URL, over a fake
// Agent Mesh with one invited agent.
func remoteRelay(t *testing.T) (string, *meshtest.Mesh) {
	t.Helper()
	m := meshtest.New(t, relayAdminToken)
	m.AddNode(meshadmin.Node{PeerID: "12D3bot", Role: meshadmin.RoleNode, EnrollmentType: "bootstrap", Enrolled: time.Date(2026, 11, 5, 8, 30, 0, 0, time.UTC)}, time.Time{}, "ci-bot")
	var relayURL string
	dir := t.TempDir()
	svc := meshbackend.NewService(meshbackend.ServiceOptions{
		Mesh:        meshadmin.New(m.URL, relayAdminToken, testHTTP),
		Removed:     meshbackend.NewRemovedAgents(filepath.Join(dir, "removed-agents.json")),
		InviteNames: meshbackend.NewInviteNames(filepath.Join(dir, "invite-names.json")),
		Status: func() control.Status {
			return control.Status{URL: relayURL, PID: 7, Versions: control.Versions{A2AServe: "v1.0.0", AgentMesh: "v0.2.0"}, Join: "invites"}
		},
		Admits: func(control.Member) bool { return false },
		Now:    func() time.Time { return meshtest.Issued },
	})
	h := control.RequireToken(relayAdminToken, backoff.NewFailures(time.Millisecond, time.Millisecond, time.Now), control.Handler(svc, nil))
	srv := httptest.NewServer(http.StripPrefix(control.RemotePath, h))
	t.Cleanup(srv.Close)
	relayURL = srv.URL
	return srv.URL, m
}

func runRelayCMD(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runCMDWith(t, &globalConfig{
		credentials: &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")},
		stdin:       pipeWith(t, relayAdminToken),
	}, args...)
}

func TestRelayCommandsAtURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "members",
			args: []string{"relay", "members"},
			want: "AGENT   WHO     JOINED            SEEN  PEER\nci-bot  invite  " + time.Date(2026, 11, 5, 8, 30, 0, 0, time.UTC).Local().Format("2006-01-02 15:04") + "  -     12D3bot\n",
		},
		{
			name: "invite",
			args: []string{"relay", "invite", "coder", "--expires", "48h"},
			want: "invite coder for {url}, one agent, until 2026-11-07 09:00 UTC:\n  mesh-bt-1\non that agent: a2a-serve config set --private {url} --public {url} --network-token -\non a new agent: a2a-serve config set --agent <agent> --private {url} --public {url} --network-token -\n",
		},
		{
			name: "remove",
			args: []string{"relay", "remove", "ci-bot"},
			want: "✓ removed ci-bot (invite, 12D3bot)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			url, _ := remoteRelay(t)

			got, err := runRelayCMD(t, append(tt.args, "--url", url, "--admin-token", "-")...)

			if err != nil {
				t.Fatalf("%s error = %v", strings.Join(tt.args, " "), err)
			}
			want := strings.ReplaceAll(tt.want, "{url}", url)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("%s wrong output (-want +got) diff = %s", strings.Join(tt.args, " "), diff)
			}
		})
	}
}

func TestRelayUnusedInvites(t *testing.T) {
	t.Parallel()
	url, _ := remoteRelay(t)
	if _, err := runRelayCMD(t, "relay", "invite", "build-box", "--url", url, "--admin-token", "-"); err != nil {
		t.Fatalf("relay invite error = %v", err)
	}
	joined := time.Date(2026, 11, 5, 8, 30, 0, 0, time.UTC).Local().Format("2006-01-02 15:04")
	expires := meshtest.Issued.Add(24 * time.Hour).Local().Format("2006-01-02 15:04")
	steps := []struct {
		args []string
		want string
	}{
		{
			args: []string{"relay", "members"},
			want: "AGENT   WHO               JOINED                         SEEN  PEER\n" +
				"ci-bot  invite            " + joined + "               -     12D3bot\n" +
				"-       invite build-box  unused until " + expires + "  -     -\n",
		},
		{args: []string{"relay", "remove", "build-box"}, want: "✓ revoked the unused invite build-box (1)\n"},
		{
			args: []string{"relay", "members"},
			want: "AGENT   WHO     JOINED            SEEN  PEER\nci-bot  invite  " + joined + "  -     12D3bot\n",
		},
	}
	for _, step := range steps {
		got, err := runRelayCMD(t, append(step.args, "--url", url, "--admin-token", "-")...)
		if err != nil {
			t.Fatalf("%s error = %v", strings.Join(step.args, " "), err)
		}
		if diff := cmp.Diff(step.want, got); diff != "" {
			t.Fatalf("%s wrong output (-want +got) diff = %s", strings.Join(step.args, " "), diff)
		}
	}
}

func TestRelayInviteRefusesAnInvalidName(t *testing.T) {
	t.Parallel()
	url, m := remoteRelay(t)

	_, err := runRelayCMD(t, "relay", "invite", "bad name!", "--url", url, "--admin-token", "-")

	if err == nil {
		t.Fatal("relay invite 'bad name!' error = nil, want an error")
	}
	if got := m.Tokens(); len(got) != 0 {
		t.Fatalf("relay invite 'bad name!' minted %v, want nothing", got)
	}
}

func TestRelayInviteLetsBackARemovedAgent(t *testing.T) {
	t.Parallel()
	url, m := remoteRelay(t)
	if _, err := runRelayCMD(t, "relay", "remove", "ci-bot", "--url", url, "--admin-token", "-"); err != nil {
		t.Fatalf("relay remove error = %v", err)
	}

	got, err := runRelayCMD(t, "relay", "invite", "ci-bot", "--url", url, "--admin-token", "-")

	if err != nil {
		t.Fatalf("relay invite error = %v", err)
	}
	want := strings.ReplaceAll("invite ci-bot for {url}, one agent, until 2026-11-06 09:00 UTC:\n  mesh-bt-1\non that agent: a2a-serve config set --private {url} --public {url} --network-token -\non a new agent: a2a-serve config set --agent <agent> --private {url} --public {url} --network-token -\n"+
		"✓ let back ci-bot (12D3bot): it joins again with its identity when it restarts\n", "{url}", url)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("relay invite wrong output (-want +got) diff = %s", diff)
	}
	if got := m.Banned(); len(got) != 0 {
		t.Fatalf("relay invite left %v banned, want none", got)
	}
}

func TestRelayStatusJSON(t *testing.T) {
	t.Parallel()
	url, _ := remoteRelay(t)

	out, err := runRelayCMD(t, "-o", "json", "relay", "status", "--url", url, "--admin-token", "-")

	if err != nil {
		t.Fatalf("relay status error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	want := map[string]any{
		"url": url, "running": true, "pid": float64(7), "join": "invites", "members": float64(1),
		"versions": map[string]any{"a2aServe": "v1.0.0", "agentMesh": "v0.2.0"},
		"pinned":   map[string]any{"a2aServe": buildVersionInfo().Version, "agentMesh": "v0.2.0", "backend": "Agent Mesh", "backendVersion": "v0.2.0"},
	}
	delete(got, "started")
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("relay status -o json wrong result (-want +got) diff = %s", diff)
	}
}

func TestRelayUsageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "url without admin token", args: []string{"relay", "members", "--url", "https://relay.example.com"}},
		{name: "admin token without url", args: []string{"relay", "members", "--admin-token", "tok"}},
		{name: "plain http off loopback", args: []string{"relay", "members", "--url", "http://relay.example.com", "--admin-token", "tok"}},
		{name: "url and dir", args: []string{"relay", "members", "--url", "https://relay.example.com", "--admin-token", "tok", "--dir", "."}},
		{name: "invite for part of an hour", args: []string{"relay", "invite", "bot", "--expires", "90m"}},
		{name: "invite longer than a week", args: []string{"relay", "invite", "bot", "--expires", "169h"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := runRelayCMD(t, tt.args...); !isUsage(err) {
				t.Fatalf("%s error = %v, want a usage error", strings.Join(tt.args, " "), err)
			}
		})
	}
}

func newTestRelay(t *testing.T) relaydir.Dir {
	t.Helper()
	d, err := relaydir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SaveSettings(relaydir.Settings{URL: "https://relay.example.com"}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	return d
}

func TestStoppedRelay(t *testing.T) {
	t.Parallel()
	d := newTestRelay(t)
	if err := d.RecordVersion("v0.2.0"); err != nil {
		t.Fatal(err)
	}

	_, err := runRelayCMD(t, "relay", "members", "--dir", d.Root)
	if ce := clierr.Classify(err); ce == nil || ce.Code != clierr.CodeNotRunning {
		t.Fatalf("relay members of a stopped relay error = %v, want not running", err)
	}

	out, err := runRelayCMD(t, "relay", "status", "--dir", d.Root)
	if err != nil {
		t.Fatalf("relay status of a stopped relay error = %v", err)
	}
	for _, want := range []string{"relay https://relay.example.com is not running", "dir       " + d.Root, "data      last used by Agent Mesh v0.2.0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("relay status = %q, want it to contain %q", out, want)
		}
	}
}

func TestRelayRunRefusesAnAgentDirectory(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

	_, err := runRelayCMD(t, "relay", "run", "--dir", d.Root, "--url", "https://relay.example.com")

	if !errors.Is(err, relaydir.ErrAgentDir) || !isUsage(err) {
		t.Fatalf("relay run in an agent directory error = %v, want %v as a usage error", err, relaydir.ErrAgentDir)
	}
}

func TestNoRelayHere(t *testing.T) {
	t.Parallel()

	_, err := runRelayCMD(t, "relay", "members", "--dir", t.TempDir())

	if ce := clierr.Classify(err); ce == nil || ce.Code != clierr.CodeNoAgent {
		t.Fatalf("relay members without a relay error = %v, want no relay", err)
	}
}

func TestRelayRunRefusesSignInWithoutTheGoogleApp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	_, err := runRelayCMD(t, "relay", "run", "--dir", dir, "--url", "https://relay.example.com", "--allow-email", "*@example.com")

	if !isUsage(err) || !strings.Contains(err.Error(), "--google-client-id") {
		t.Fatalf("relay run --allow-email without the Google app error = %v, want a usage error that names --google-client-id", err)
	}
}
