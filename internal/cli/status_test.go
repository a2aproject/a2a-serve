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
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
	"github.com/a2aproject/a2a-serve/internal/output"
)

func TestStatusStoppedJSON(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

	out, err := runCMD(t, "-o", "json", "status", "--dir", d.Root)
	if err != nil {
		t.Fatalf("a2a-serve status error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", out, err)
	}
	want := map[string]any{"name": "coder", "agent": "opencode", "dir": d.Root, "running": false}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("a2a-serve status -o json wrong result (-want +got) diff = %s", diff)
	}
}

func TestStatusURLScope(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode", Port: 4100})
	empty := t.TempDir()
	tests := []struct {
		name     string
		args     []string
		wantOut  string
		wantExit int
	}{
		{name: "scope after the flag", args: []string{"status", "--url", "local", "--dir", d.Root}, wantOut: "http://127.0.0.1:4100\n"},
		{name: "scope joined with =", args: []string{"status", "--url=local", "--dir", d.Root}, wantOut: "http://127.0.0.1:4100\n"},
		{name: "scope of a stopped agent which needs the server", args: []string{"status", "--url", "private", "--dir", d.Root}, wantExit: 6},
		{name: "flag without a scope of a stopped agent", args: []string{"status", "--url", "--dir", d.Root}, wantExit: 6},
		{name: "scope without the flag", args: []string{"status", "local", "--dir", d.Root}, wantExit: 2},
		{name: "second argument", args: []string{"status", "--url", "local", "extra", "--dir", d.Root}, wantExit: 2},
		{name: "unknown scope before the directory lookup", args: []string{"status", "--url", "bogus", "--dir", empty}, wantExit: 2},
		{name: "unknown scope joined with =", args: []string{"status", "--url=bogus", "--dir", empty}, wantExit: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := runCMD(t, tc.args...)
			gotExit := 0
			if err != nil {
				gotExit = clierr.Classify(err).Exit
			}
			if gotExit != tc.wantExit {
				t.Fatalf("a2a-serve %s error = %v (exit %d), want exit %d", strings.Join(tc.args, " "), err, gotExit, tc.wantExit)
			}
			if out != tc.wantOut {
				t.Fatalf("a2a-serve %s = %q, want %q", strings.Join(tc.args, " "), out, tc.wantOut)
			}
		})
	}
}

func TestInspectScopeValidatedFirst(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()

	_, err := runCMD(t, "inspect", "--url", "public", "--dir", empty)

	want := "the inspector isn't served on the public URL; want local or private"
	if err == nil || err.Error() != want {
		t.Fatalf("a2a-serve inspect --url public error = %v, want %q", err, want)
	}
	if got := clierr.Classify(err).Exit; got != 2 {
		t.Fatalf("a2a-serve inspect --url public exit = %d, want 2", got)
	}
}

func TestPrintStatusAccess(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	cfg := &globalConfig{Printer: output.NewPrinter(&out, output.ModeText)}
	st := &controlapi.Status{
		Name: "coder", Harness: "opencode", Started: time.Now(), Local: "http://127.0.0.1:4000", Private: "https://coder.tail1234.ts.net",
		Access: &controlapi.Access{Summary: []string{"you: this machine and your tailnet devices", "everyone on the tailnet, 2 pairings", "the public URL needs a pairing"}},
	}
	if err := printStatus(cfg, st, nil); err != nil {
		t.Fatalf("printStatus() error = %v", err)
	}
	want := "  access   you: this machine and your tailnet devices\n" +
		"           everyone on the tailnet, 2 pairings\n" +
		"           the public URL needs a pairing\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("printStatus() = %q, want the access block %q", out.String(), want)
	}
}

func TestStatusJSONNamesAgentOnce(t *testing.T) {
	t.Parallel()
	st := &controlapi.Status{Name: "coder", Harness: "opencode", Dir: "/work"}

	data, err := json.Marshal(statusJSON{Name: st.Name, Harness: st.Harness, Dir: st.Dir, Running: true, Status: st})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", data, err)
	}
	if _, ok := got["harness"]; ok {
		t.Fatalf("status -o json = %s, want no harness key", data)
	}
	if got["agent"] != "opencode" {
		t.Fatalf("status -o json agent = %v, want %q", got["agent"], "opencode")
	}
}
