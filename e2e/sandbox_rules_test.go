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

//go:build e2e

package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxRulesGiveCallersTheirSecrets(t *testing.T) {
	t.Parallel()
	dependsOnSandbox(t)
	owner, anna, ben := newUser(t), newUser(t), newUser(t)
	coder := owner.startAgent("coder", "--secret", "KEY=owner-key", "--secret", "GUEST=guest-key")
	anna.pairJoin(coder.url, coder.invite("anna", "--storage", "read-only", "--secrets", "KEY=secret:GUEST"))
	ben.pairJoin(coder.url, coder.invite("ben"))

	status := coder.serve("status")
	wantStatus := []string{
		"sandbox  you: shared workspace, secrets KEY, GUEST",
		"the pairing anna: read-only workspace, secrets KEY (from GUEST)",
		"other callers: shared workspace, secrets KEY, GUEST",
	}
	if sandbox != "docker" {
		wantStatus = []string{"storage rules don't apply"}
	}
	for _, want := range wantStatus {
		if !strings.Contains(status, want) {
			t.Fatalf("a2a-serve status = %q, want it to contain %q", status, want)
		}
	}
	tests := []struct {
		name   string
		caller *user
		prompt string
		want   string
	}{
		{name: "owner", caller: owner, prompt: "env KEY", want: "env KEY=owner-key"},
		{name: "anna's rule", caller: anna, prompt: "env KEY", want: "env KEY=guest-key"},
		{name: "anna's rule", caller: anna, prompt: "env GUEST", want: "env GUEST unset"},
		{name: "default for ben", caller: ben, prompt: "env KEY", want: "env KEY=owner-key"},
	}
	for _, tc := range tests {
		if got := resultOf(tc.caller.mustSend(coder.url, tc.prompt)).Text; got != tc.want {
			t.Fatalf("a2a send %s by %s = %q, want %q", tc.prompt, tc.name, got, tc.want)
		}
	}

	coder.serve("config", "set", "--guest-secrets", "KEY=secret:GUEST")
	if got := resultOf(ben.mustSend(coder.url, "env KEY")).Text; got != "env KEY=owner-key" {
		t.Fatalf("a2a send env KEY by ben after config set = %q, want the owner's key", got)
	}

	coder.serve("pair", "set", "ben", "--storage", "read-only", "--secrets", "none")
	var p struct {
		Sandbox struct {
			Storage string   `json:"storage"`
			Secrets []string `json:"secrets"`
		} `json:"sandbox"`
	}
	coder.serveJSON(&p, "pair", "get", "ben")
	if p.Sandbox.Storage != "read-only" || len(p.Sandbox.Secrets) != 0 {
		t.Fatalf("a2a-serve pair get ben sandbox = %+v, want read-only with no secrets", p.Sandbox)
	}
	if got := resultOf(ben.mustSend(coder.url, "env GUEST")).Text; got != "env GUEST unset" {
		t.Fatalf("a2a send env GUEST by ben after pair set = %q, want it unset", got)
	}

	coder.serve("pair", "rm", "anna")
	if strings.Contains(coder.serve("status"), "the pairing anna") {
		t.Fatalf("a2a-serve status after pair rm anna still lists her sandbox rule")
	}
}

func TestSandboxStorageModes(t *testing.T) {
	t.Parallel()
	requireDocker(t)
	owner, anna, ben, cy := newUser(t), newUser(t), newUser(t), newUser(t)
	coder := owner.startAgent("coder")
	anna.pairJoin(coder.url, coder.invite("anna", "--storage", "read-only"))
	ben.pairJoin(coder.url, coder.invite("ben", "--storage", "own"))
	cy.pairJoin(coder.url, coder.invite("cy", "--storage", "empty"))
	file := func(name string) string { return filepath.Join(coder.dir, name) }

	tests := []struct {
		name   string
		caller *user
		prompt string
		want   string
	}{
		{name: "the owner writes the workspace", caller: owner, prompt: "write " + file("owner.txt"), want: "write ok"},
		{name: "anna can't write it", caller: anna, prompt: "write " + file("anna.txt"), want: "write error: open " + file("anna.txt") + ": read-only file system"},
		{name: "anna reads it", caller: anna, prompt: "ls " + coder.dir, want: "owner.txt"},
		{name: "ben writes his copy", caller: ben, prompt: "write " + file("ben.txt"), want: "write ok"},
		{name: "ben's copy has the owner's files", caller: ben, prompt: "ls " + coder.dir, want: "owner.txt"},
		{name: "ben's copy keeps his files", caller: ben, prompt: "ls " + coder.dir, want: "ben.txt"},
		{name: "cy writes her empty workspace", caller: cy, prompt: "write " + file("cy.txt"), want: "write ok"},
	}
	for _, tc := range tests {
		if got := resultOf(tc.caller.mustSend(coder.url, tc.prompt)).Text; !strings.Contains(got, tc.want) {
			t.Fatalf("a2a send %q (%s) = %q, want it to contain %q", tc.prompt, tc.name, got, tc.want)
		}
	}
	if got := resultOf(cy.mustSend(coder.url, "ls "+coder.dir)).Text; strings.Contains(got, "owner.txt") || strings.Contains(got, "ben.txt") {
		t.Fatalf("a2a send ls of cy's empty workspace = %q, want none of the others' files", got)
	}
	if got := resultOf(ben.mustSend(coder.url, "ls "+file(".a2a"))).Text; got != "ls: " && !strings.Contains(got, "permission denied") {
		t.Fatalf("a2a send ls of .a2a in ben's copy = %q, want it empty or unreadable", got)
	}
	for _, name := range []string{"anna.txt", "ben.txt", "cy.txt"} {
		if _, err := os.Stat(file(name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("os.Stat(%s) error = %v, want the owner's workspace without it", name, err)
		}
	}

	coder.serve("pair", "rm", "ben")
	if entries, err := os.ReadDir(filepath.Join(coder.dir, ".a2a", "state", "workspaces")); err != nil || len(entries) != 1 {
		t.Fatalf("private workspaces after pair rm ben = %v, %v, want only cy's", entries, err)
	}
}
