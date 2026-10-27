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
	"slices"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/clierr"
)

func TestDoctorJSON(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "acp", Command: []string{"fakeacp"}, Sandbox: agentdir.SandboxLocal, Secrets: []string{"API_KEY"}})
	out, err := runCMD(t, "-o", "json", "doctor", "--dir", d.Root)
	var ce *clierr.Error
	if !errors.As(err, &ce) || !ce.Silent() || ce.Exit != 1 {
		t.Fatalf("a2a serve doctor -o json error = %v, want a silent exit 1 for the missing secret", err)
	}
	var results []struct {
		ID, Status, Message, Fix string
	}
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("parsing %q error = %v", out, err)
	}
	var ids []string
	for _, r := range results {
		ids = append(ids, r.ID+":"+r.Status)
	}
	for _, want := range []string{"config:ok", "secrets:fail", "running:warn"} {
		if !slices.Contains(ids, want) {
			t.Fatalf("a2a serve doctor -o json = %v, want %s", ids, want)
		}
	}
}

func TestDoctorDirWithoutAgent(t *testing.T) {
	t.Parallel()
	_, err := runCMD(t, "doctor", "--dir", t.TempDir())
	if !errors.Is(err, agentdir.ErrNotFound) {
		t.Fatalf("a2a serve doctor --dir <empty> error = %v, want %v", err, agentdir.ErrNotFound)
	}
}
