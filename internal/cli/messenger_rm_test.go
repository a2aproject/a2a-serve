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
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestMessengerRmOffline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		kind    string
		wantErr string
	}{
		{name: "configured", kind: "telegram"},
		{name: "not configured", kind: "discord", wantErr: "no messenger discord"},
		{name: "unknown kind", kind: "fax", wantErr: "unknown messenger"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := agentdir.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg := &agentdir.Config{Name: "coder", Harness: "opencode"}
			if err := cfg.SetMessenger("telegram", "secret:TELEGRAM_TOKEN", nil); err != nil {
				t.Fatal(err)
			}
			if err := d.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}

			out, err := runCMD(t, "messenger", "rm", tc.kind, "--dir", d.Root)

			if tc.wantErr == "" && err != nil {
				t.Fatalf("messenger rm error = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("messenger rm error = %v, want %q", err, tc.wantErr)
			}
			if tc.wantErr != "" && strings.Contains(out, "removed") {
				t.Fatalf("messenger rm output = %q, want no removal reported", out)
			}
		})
	}
}

func TestMessengerRmReportsAccess(t *testing.T) {
	t.Parallel()
	d, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &agentdir.Config{Name: "coder", Harness: "opencode", Network: agentdir.NetworkConfig{Public: "https://relay.example.com"}, Policy: agentdir.PolicyConfig{PublicAllowIncoming: agentdir.AccessAnyone}}
	if err := cfg.SetMessenger("telegram", "secret:TELEGRAM_TOKEN", nil); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	out, err := runCMD(t, "messenger", "rm", "telegram", "--dir", d.Root)

	if err != nil {
		t.Fatalf("messenger rm error = %v", err)
	}
	if !strings.HasPrefix(out, "✓ removed telegram\nwho may call:\n") {
		t.Fatalf("messenger rm output = %q, want who may call after the removal", out)
	}
}
