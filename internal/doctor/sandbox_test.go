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

package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/agentcfg"
	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/sandboxrt"
)

func TestBwrapChecks(t *testing.T) {
	t.Parallel()
	appArmor := "AppArmor restricts unprivileged user namespaces (kernel.apparmor_restrict_unprivileged_userns=1): give bwrap an AppArmor profile that allows them, or run sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0"
	tests := []struct {
		name  string
		mode  string
		setup func(m *fakeMachine)
		want  []Result
	}{
		{
			name: "working",
			want: []Result{
				{ID: "bwrap", Status: StatusOK, Message: "bubblewrap makes sandboxes"},
				{ID: "harness-command", Status: StatusOK, Message: "opencode at /usr/bin/opencode (sandbox bwrap)"},
				{ID: "egress", Status: StatusOK, Message: "the sandbox connects only through the server (--egress internet)"},
			},
		},
		{
			name:  "not installed",
			setup: func(m *fakeMachine) { m.bwrapErr = &sandboxrt.BubblewrapError{Problem: "is not installed", Fix: "install bubblewrap", Err: errors.New("exec: \"bwrap\": executable file not found in $PATH")} },
			want: []Result{{
				ID: "bwrap", Status: StatusFail, Message: "bwrap is not installed",
				Detail: "exec: \"bwrap\": executable file not found in $PATH",
				Fix:    "install bubblewrap or a2a-serve config set --sandbox docker",
			}},
		},
		{
			name:  "user namespaces restricted by AppArmor",
			setup: func(m *fakeMachine) { m.bwrapErr = &sandboxrt.BubblewrapError{Problem: "can't make a sandbox on this machine", Fix: appArmor} },
			want: []Result{{
				ID: "bwrap", Status: StatusFail, Message: "bwrap can't make a sandbox on this machine",
				Fix: appArmor + ", or a2a-serve config set --sandbox docker",
			}},
		},
		{
			name:  "not Linux",
			setup: func(m *fakeMachine) { m.bwrapErr = &sandboxrt.BubblewrapError{Problem: "runs only on Linux", Fix: "use the Docker sandbox", Err: errors.ErrUnsupported} },
			want: []Result{{
				ID: "bwrap", Status: StatusFail, Message: "bwrap runs only on Linux; use the Docker sandbox: unsupported operation",
				Fix: "a2a-serve config set --sandbox docker",
			}},
		},
		{
			name:  "no TUN device",
			setup: func(m *fakeMachine) { m.bwrap.Boundary = errors.New("this machine has no /dev/net/tun; load the tun kernel module") },
			want: []Result{
				{ID: "bwrap", Status: StatusOK, Message: "bubblewrap makes sandboxes"},
				{ID: "harness-command", Status: StatusOK, Message: "opencode at /usr/bin/opencode (sandbox bwrap)"},
				{ID: "egress", Status: StatusFail, Message: "the bubblewrap sandbox can't route the agent's connections through the server: this machine has no /dev/net/tun; load the tun kernel module. The agent doesn't start, so that it can't reach this machine's network and its services on loopback. Fix that, or run a2a-serve config set --sandbox docker"},
			},
		},
		{
			name: "unrestricted",
			mode: "unrestricted",
			want: []Result{
				{ID: "bwrap", Status: StatusOK, Message: "bubblewrap makes sandboxes"},
				{ID: "harness-command", Status: StatusOK, Message: "opencode at /usr/bin/opencode (sandbox bwrap)"},
				{ID: "egress", Status: StatusWarn, Message: "--egress unrestricted: " + agentcfg.BwrapUnrestrictedEffect, Fix: "a2a-serve config unset egress --restart"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newFakeMachine(t)
			m.paths["opencode"] = "/usr/bin/opencode"
			if tc.setup != nil {
				tc.setup(m)
			}
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode", Sandbox: agentdir.SandboxBwrap, Egress: agentdir.EgressConfig{Mode: tc.mode}}
			d := newAgent(t, cfg)
			got := bwrapChecks(context.Background(), m.Env(), d, cfg)
			if diff := cmp.Diff(tc.want, got, cmpopts.IgnoreUnexported(Result{})); diff != "" {
				t.Fatalf("bwrapChecks() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
