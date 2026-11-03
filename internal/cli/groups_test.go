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

	"github.com/a2aproject/a2a-serve/internal/output"
)

func TestServeCommandsHaveGroups(t *testing.T) {
	t.Parallel()
	root := newRootCmd(&globalConfig{Printer: output.NewPrinter(&strings.Builder{}, output.ModeText)})
	serve, _, err := root.Find([]string{pluginName})
	if err != nil {
		t.Fatalf("root.Find(serve) error = %v", err)
	}
	for _, c := range serve.Commands() {
		if c.IsAvailableCommand() && c.GroupID == "" {
			t.Fatalf("a2a serve %s has no help group, want one of %v", c.Name(), serve.Groups())
		}
	}
}

func TestServeHelp(t *testing.T) {
	t.Parallel()
	out, err := runCMD(t, "--help")
	if err != nil {
		t.Fatalf("a2a serve --help error = %v", err)
	}
	for _, want := range []string{"Run the agent:", "Configure the agent:", "Pairing and contacts:", "Webhook node:", "Setup:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("a2a serve --help = %q, want %q", out, want)
		}
	}
	for _, flag := range []string{"--port", "--detach", "--quiet", "--chunk", "--timeout"} {
		if strings.Contains(out, flag) {
			t.Fatalf("a2a serve --help = %q, want no %s", out, flag)
		}
	}
}
