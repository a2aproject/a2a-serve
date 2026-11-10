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
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/messenger/telegram"
	"github.com/a2aproject/a2a-serve/internal/messenger/telegram/telegramtest"
)

var quotedCommand = regexp.MustCompile("`((?:a2a|a2a-serve) [^`]+)`")

// printedCommands returns the commands quoted in text that start with
// prefix, split into words.
func printedCommands(t *testing.T, text, prefix string) [][]string {
	t.Helper()
	var out [][]string
	for _, m := range quotedCommand.FindAllStringSubmatch(text, -1) {
		if strings.HasPrefix(m[1], prefix) {
			out = append(out, strings.Fields(m[1]))
		}
	}
	if len(out) == 0 {
		t.Fatalf("%q quotes no command starting with %q", text, prefix)
	}
	return out
}

// replaceWord returns cmd with each word old replaced by the words new.
func replaceWord(cmd []string, old string, new ...string) []string {
	var out []string
	for _, w := range cmd {
		if w == old {
			out = append(out, new...)
			continue
		}
		out = append(out, w)
	}
	return out
}

// serveCommand runs a printed `a2a-serve` command as `a2a serve`, the way
// binDir provides it.
func serveCommand(cmd []string) []string {
	return append([]string{"a2a", "serve"}, cmd[1:]...)
}

func TestBusyTaskHintsWorkAsPrinted(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	task := owner.mustSend(coder.url, "--async", "slow 10s")

	_, err := owner.send(coder.url, "--task-id", string(task.ID), "hello")
	if err == nil {
		t.Fatalf("a2a send to the working task error = nil, want it refused")
	}

	cmds := printedCommands(t, err.Error(), "a2a ")
	if len(cmds) != 2 {
		t.Fatalf("a2a send to the working task error = %v, want two commands", err)
	}
	if _, err := owner.exec(owner.home, cmds[0]...); err != nil {
		t.Fatalf("printed command error = %v", err)
	}
	next := append(cmds[1], "-o", "json", "hello")
	eventually(t, "continuing the conversation once the task is done", func() (bool, string) {
		out, err := owner.exec(owner.home, next...)
		if err != nil {
			return false, err.Error()
		}
		got, err := decodeTask(out)
		if err != nil {
			return false, err.Error()
		}
		return got.Status.State == a2a.TaskStateCompleted && got.ContextID == task.ContextID, string(got.Status.State)
	})
}

func TestWaitingQuestionHintWorksAsPrinted(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	asked := owner.mustSend(coder.url, "ask")

	_, err := owner.send(coder.url, "--context-id", asked.ContextID, "hello")
	if err == nil {
		t.Fatalf("a2a send in the waiting conversation error = nil, want it refused")
	}

	cmd := append(printedCommands(t, err.Error(), "a2a send")[0], "-o", "json")
	out, err := owner.exec(owner.home, cmd...)
	if err != nil {
		t.Fatalf("printed command error = %v", err)
	}
	got, err := decodeTask(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("printed command task state = %s, want %s", got.Status.State, a2a.TaskStateCompleted)
	}
}

func TestSecretOfPairingRuleHintWorksAsPrinted(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder", "--secret", "KEY=owner-key", "--secret", "G2=other-key")
	coder.invite("ben", "--secrets", "KEY,G2")

	_, err := owner.exec(coder.dir, "a2a", "serve", "config", "unset", "secret", "G2")
	if err == nil {
		t.Fatalf("a2a-serve config unset secret G2 error = nil, want it refused")
	}

	cmd := printedCommands(t, err.Error(), "a2a-serve pair set")[0]
	if !slices.Contains(cmd, "KEY") {
		t.Fatalf("printed command %q, want it to keep KEY", cmd)
	}
	owner.run(coder.dir, serveCommand(cmd)...)
	coder.serve("config", "unset", "secret", "G2")
}

func TestStatusHintAfterConfigDeletedWorksAsPrinted(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	coder.stop()
	if err := os.Remove(coder.dir + "/.a2a/config.yaml"); err != nil {
		t.Fatal(err)
	}

	status := coder.serve("status")

	cmd := printedCommands(t, status, "a2a-serve start")[0]
	if !strings.Contains(status, "(claude") {
		t.Fatalf("a2a-serve status = %q, want it to list the agents", status)
	}
	coder.stopped = false
	owner.run(coder.dir, serveCommand(replaceWord(cmd, "<agent>", append([]string{"acp", "--name", coder.name, "--detach"}, sandboxFlags()...)...))...)
	if !coder.running() {
		t.Fatalf("the agent isn't running after the printed command")
	}
}

func TestRevokedContactHintWorksAsPrinted(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder, reviewer := owner.startAgent("coder"), owner.startAgent("reviewer")
	coder.serve("contact", "add", "reviewer", "--url", reviewer.url, "--code", reviewer.invite("coder"))
	reviewer.serve("pair", "rm", "coder")

	refused := resultOf(owner.mustSend(coder.url, "call reviewer hello"))

	invite := printedCommands(t, refused.Text, "a2a-serve pair invite")[0]
	var code struct {
		Code string `json:"code"`
	}
	reviewer.u.runJSON(reviewer.dir, &code, serveCommand(replaceWord(invite, "<name>", "coder"))...)
	add := printedCommands(t, refused.Text, "a2a-serve contact add")[0]
	owner.run(coder.dir, serveCommand(replaceWord(add, "<code>", code.Code))...)
	got := resultOf(owner.mustSend(coder.url, "call reviewer hello"))
	if got.State != a2a.TaskStateCompleted || !strings.HasSuffix(got.Text, "TASK_STATE_COMPLETED: Hello from fakeacp.") {
		t.Fatalf("a2a send call reviewer after the printed commands = %+v, want the reviewer's answer", got)
	}
}

func TestUnsetScalarsHintWorksAsPrinted(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	coder.serve("config", "set", "--memory", "2g", "--cpus", "2")

	_, err := owner.exec(coder.dir, "a2a", "serve", "config", "unset", "memory", "cpus")
	if err == nil {
		t.Fatalf("a2a-serve config unset memory cpus error = nil, want it refused")
	}

	for _, cmd := range printedCommands(t, err.Error(), "a2a-serve config unset") {
		owner.run(coder.dir, serveCommand(cmd)...)
	}
	saved, err := os.ReadFile(coder.dir + "/.a2a/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "memory:") || strings.Contains(string(saved), "cpus:") {
		t.Fatalf("config.yaml = %q, want memory and cpus reset", saved)
	}
}

func TestConfigEditDraftHintWorksAsPrinted(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	dir := t.TempDir()
	withEditor := func(name, script string) *user {
		path := dir + "/" + name
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		u := *owner
		u.env = append(slices.Clone(owner.env), "EDITOR="+path)
		return &u
	}
	breaks := withEditor("break.sh", "#!/bin/sh\nsed -i 's/^sandbox: local$/sandbox: vm/' \"$1\"\nprintf 'description: from the draft\\n' >> \"$1\"\n")
	fixes := withEditor("fix.sh", "#!/bin/sh\nsed -i 's/^sandbox: vm$/sandbox: local/' \"$1\"\n")

	_, err := breaks.exec(coder.dir, "a2a", "serve", "config", "edit")
	if err == nil {
		t.Fatalf("a2a-serve config edit error = nil, want the invalid edit rejected")
	}

	cmd := printedCommands(t, err.Error(), "a2a-serve config edit")[0]
	fixes.run(coder.dir, serveCommand(cmd)...)
	saved, err := os.ReadFile(coder.dir + "/.a2a/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "description: from the draft") {
		t.Fatalf("config.yaml = %q, want the fixed draft saved", saved)
	}
}

func TestScheduleUnknownUserHintWorksAsPrinted(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	add := []string{"a2a", "serve", "schedule", "add", "t9", "--prompt", "hi", "--cron", "@daily", "--as", "nobody"}

	_, err := owner.exec(coder.dir, add...)
	if err == nil {
		t.Fatalf("a2a-serve schedule add --as nobody error = nil, want it refused")
	}

	owner.run(coder.dir, serveCommand(printedCommands(t, err.Error(), "a2a-serve pair invite")[0])...)
	owner.run(coder.dir, add...)
}

func TestScheduleNoChatHintsWorkAsPrinted(t *testing.T) {
	t.Parallel()
	api := telegramtest.NewServer(t, telegramtest.Options{})
	owner := newUser(t)
	owner.env = append(owner.env, telegram.EnvServerURL+"="+api.URL, "E2E_BOT_TOKEN=123:abc")
	coder := owner.startAgent("coder")
	coder.invite("anna")
	coder.serve("messenger", "add", "telegram", "--token", "env:E2E_BOT_TOKEN")

	_, err := owner.exec(coder.dir, "a2a", "serve", "schedule", "add", "t9", "--prompt", "hi", "--cron", "@daily", "--chat", "telegram:anna")
	if err == nil {
		t.Fatalf("a2a-serve schedule add --chat telegram:anna error = nil, want it refused")
	}
	var code struct {
		Code string `json:"code"`
	}
	owner.runJSON(coder.dir, &code, serveCommand(printedCommands(t, err.Error(), "a2a-serve pair invite")[0])...)
	if code.Code == "" {
		t.Fatalf("the printed pair invite returned no code")
	}

	_, err = owner.exec(coder.dir, "a2a", "serve", "schedule", "add", "t9", "--prompt", "hi", "--cron", "@daily", "--chat", "telegram")
	if err == nil {
		t.Fatalf("a2a-serve schedule add --chat telegram error = nil, want it refused")
	}
	out := owner.run(coder.dir, serveCommand(printedCommands(t, err.Error(), "a2a-serve restart")[0])...)
	if !strings.Contains(out, "link yourself as owner: ") {
		t.Fatalf("the printed restart output = %q, want the owner link", out)
	}
}
