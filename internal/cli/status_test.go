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
