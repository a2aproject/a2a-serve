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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/credstore"
	"github.com/a2aproject/a2a-serve/internal/flagparse"
	"github.com/a2aproject/a2a-serve/internal/localsrv"
	"github.com/a2aproject/a2a-serve/internal/output"
	"github.com/a2aproject/a2a-serve/internal/testutil"
)

var (
	fakeACPOnce sync.Once
	fakeACPPath string
	fakeACPErr  error
)

// buildFakeACP builds the scripted ACP agent of the agentsrv tests.
func buildFakeACP(t *testing.T) string {
	t.Helper()
	fakeACPOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakeacp")
		if err != nil {
			fakeACPErr = err
			return
		}
		fakeACPPath = filepath.Join(dir, "fakeacp")
		out, err := exec.Command("go", "build", "-o", fakeACPPath, "../agentsrv/testdata/fakeacp").CombinedOutput()
		if err != nil {
			fakeACPErr = err
			t.Logf("go build: %s", out)
		}
	})
	if fakeACPErr != nil {
		t.Fatalf("building fakeacp: %v", fakeACPErr)
	}
	return fakeACPPath
}

func runCMDWithCreds(t *testing.T, creds *credstore.Store, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cfg := &globalConfig{
		Printer:     output.NewPrinter(&buf, output.ModeText),
		credentials: creds,
	}
	root := newRootCmd(cfg)
	root.SetOut(&buf)
	root.SetArgs(rootArgs(args))
	err := root.Execute()
	return buf.String(), err
}

func runCMD(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runCMDWithCreds(t, &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}, args...)
}

// send sends text to the agent at url with the token creds has for it, the
// way the a2a CLI does, and returns the text of the answer.
func send(t *testing.T, creds *credstore.Store, url, text string) (string, error) {
	t.Helper()
	params := &flagparse.ServiceParams{}
	c, ok, err := creds.Lookup(url)
	if err != nil {
		t.Fatalf("creds.Lookup() error = %v", err)
	}
	if ok {
		params.SetDefaultAuth("Bearer " + c.Token)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client, err := localsrv.UpstreamClient(ctx, url, params, false)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := client.Destroy(); err != nil {
			t.Errorf("client.Destroy() error = %v", err)
		}
	}()
	if p := params.Params(); len(p) > 0 {
		ctx = a2aclient.AttachServiceParams(ctx, p)
	}
	res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))})
	if err != nil {
		return "", err
	}
	switch r := res.(type) {
	case *a2a.Task:
		return testutil.AllArtifactText(r), nil
	case *a2a.Message:
		return output.MessageText(r), nil
	default:
		return "", fmt.Errorf("unexpected result %T", res)
	}
}

// TestServerPairAndSend runs serially: building fakeacp loads the machine,
// and parallel tests with millisecond timeouts would time out.
func TestServerPairAndSend(t *testing.T) {
	fake := buildFakeACP(t)
	dir, err := os.MkdirTemp("", "a2a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("removing %s: %v", dir, err)
		}
	})
	creds := &credstore.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}

	done := make(chan error, 1)
	go func() {
		_, err := runCMDWithCreds(t, creds, "acp", "--dir", dir, "--name", "coder", "--command", fake, "--sandbox", "local")
		done <- err
	}()
	var status struct {
		Running bool   `json:"running"`
		Local   string `json:"local"`
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := runCMDWithCreds(t, creds, "-o", "json", "status", "--dir", dir)
		if err == nil && json.Unmarshal([]byte(out), &status) == nil && status.Local != "" {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("a2a serve acp error = %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent did not start: %s %v", out, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !status.Running {
		t.Fatalf("a2a serve status -o json running = false, want true")
	}
	out, err := send(t, creds, status.Local, "hello")
	if err != nil || !strings.Contains(out, "Hello from fakeacp.") {
		t.Fatalf("send with the owner token of this machine = %q, %v, want the agent's answer", out, err)
	}
	if out, err := runCMDWithCreds(t, creds, "url", "--dir", dir); err != nil || out != status.Local+"\n" {
		t.Fatalf("a2a serve url = %q, %v, want %q", out, err, status.Local)
	}
	if _, err := runCMDWithCreds(t, creds, "url", "--private", "--dir", dir); err == nil || !strings.Contains(err.Error(), "has no private URL") {
		t.Fatalf("a2a serve url --private error = %v, want no private URL", err)
	}
	if out, err := runCMDWithCreds(t, creds, "contact", "list", "--dir", dir); err != nil || !strings.Contains(out, "no contacts; add one with") {
		t.Fatalf("a2a serve contact list = %q, %v, want a hint to add one", out, err)
	}
	if out, err := runCMDWithCreds(t, creds, "pair"); err != nil || !strings.Contains(out, "invite") {
		t.Fatalf("a2a serve pair = %q, %v, want help listing invite", out, err)
	}

	out, err = runCMDWithCreds(t, creds, "pair", "invite", "me", "--dir", dir, "--role", "owner")
	if err != nil {
		t.Fatalf("a2a serve pair invite error = %v", err)
	}
	if !strings.Contains(out, "this URL only works on this machine") {
		t.Fatalf("a2a serve pair invite on a localhost-only agent = %q, want a warning", out)
	}
	code := regexp.MustCompile(`code ([A-Z0-9]{4}-[A-Z0-9]{4})`).FindStringSubmatch(out)
	if code == nil {
		t.Fatalf("a2a serve pair invite = %q, want a code", out)
	}
	if _, err := runCMDWithCreds(t, creds, "tokens", "add", status.Local, code[1]); err != nil {
		t.Fatalf("a2a serve tokens add error = %v", err)
	}
	out, err = send(t, creds, status.Local, "hello")
	if err != nil {
		t.Fatalf("send error = %v", err)
	}
	if !strings.Contains(out, "Hello from fakeacp.") {
		t.Fatalf("send = %q, want the agent's answer", out)
	}
	if _, err := send(t, &credstore.Store{Path: filepath.Join(t.TempDir(), "none.json")}, status.Local, "hello"); err == nil {
		t.Fatalf("send without a token error = nil, want error")
	}

	out, err = runCMDWithCreds(t, creds, "pair", "invite", "grok", "--dir", dir, "--webhook", "https://example.com/hook")
	if err != nil {
		t.Fatalf("a2a serve pair invite --webhook error = %v", err)
	}
	code = regexp.MustCompile(`of the agent at (\S+)\.\n.*\n.*Your pairing code is ([A-Z0-9]{4}-[A-Z0-9]{4})\.`).FindStringSubmatch(out)
	if code == nil {
		t.Fatalf("a2a serve pair invite --webhook = %q, want the node's instructions", out)
	}
	if out, err := runCMDWithCreds(t, creds, "tokens", "add", code[1], code[2]); err != nil || !strings.Contains(out, "node grok") {
		t.Fatalf("a2a serve tokens add with a node code = %q, %v, want the node token", out, err)
	}
	if out, err := runCMDWithCreds(t, creds, "mailbox", "list"); err != nil || !strings.Contains(out, "grok (node grok)") {
		t.Fatalf("a2a serve mailbox list = %q, %v, want the node", out, err)
	}
	if out, err := runCMDWithCreds(t, creds, "relay", "inbox"); err != nil || !strings.Contains(out, "grok (node grok)") {
		t.Fatalf("a2a serve relay inbox = %q, %v, want the node", out, err)
	}
	if _, err := send(t, creds, status.Local, "hello"); err != nil {
		t.Fatalf("send after pairing a node error = %v", err)
	}

	if out, err := runCMDWithCreds(t, creds, "pair", "list", "--dir", dir); err != nil || !strings.Contains(out, "user:owner") || !strings.Contains(out, "node:grok") {
		t.Fatalf("a2a serve pair list = %q, %v, want me and grok", out, err)
	}
	if out, err := runCMDWithCreds(t, creds, "-o", "jsonl", "pair", "rm", "grok", "--dir", dir); err != nil || out != `{"kind":"node","name":"grok","removed":true}`+"\n" {
		t.Fatalf("a2a serve -o jsonl pair rm grok = %q, %v, want the node removed", out, err)
	}
	if _, err := runCMDWithCreds(t, creds, "pair", "rm", "me", "--dir", dir); err != nil {
		t.Fatalf("a2a serve pair rm error = %v", err)
	}
	if _, err := send(t, creds, status.Local, "hello"); err == nil {
		t.Fatalf("send after a2a serve pair rm error = nil, want error")
	}

	out, err = runCMDWithCreds(t, creds, "stop", "--uninstall", "--dir", dir)
	if err != nil || !strings.Contains(out, "stopped coder") || !strings.Contains(out, "no service was installed") {
		t.Fatalf("a2a serve stop --uninstall = %q, %v, want the agent stopped and no service removed", out, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a2a serve acp error = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("agent did not stop")
	}
}

func TestServeNoAgent(t *testing.T) {
	t.Parallel()
	_, err := runCMD(t, "--dir", "/nonexistent-a2a-agent")
	if err == nil || !strings.Contains(err.Error(), "has no .a2a/config.yaml") {
		t.Fatalf("a2a serve --dir /nonexistent-a2a-agent error = %v, want %q", err, "has no .a2a/config.yaml")
	}
}

func TestServeUsageErrors(t *testing.T) {
	t.Parallel()
	const agentURL = "https://agent.example.com"
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "legacy echo", args: []string{"--echo"}, wantErr: "run `a2a serve echo`"},
		{name: "legacy proxy", args: []string{"--proxy", agentURL}, wantErr: "run `a2a serve proxy " + agentURL + "`"},
		{name: "legacy exec", args: []string{"--exec", "tr a-z A-Z"}, wantErr: "run `a2a serve script 'tr a-z A-Z'`"},
		{name: "test server flag on serve", args: []string{"--port", "9000"}, wantErr: "unknown flag: --port"},
		{name: "script stream and chunk", args: []string{"script", "--stream", "--chunk", ",", "cat"}, wantErr: "--stream and --chunk are mutually exclusive"},
		{name: "pair add", args: []string{"pair", "add", agentURL, "3MZ8-LQ0D"}, wantErr: "run `a2a serve tokens add " + agentURL + " 3MZ8-LQ0D`"},
		{name: "pair add without arguments", args: []string{"pair", "add"}, wantErr: "run `a2a serve tokens add <agent-url> <code>`"},
		{name: "pair tokens", args: []string{"pair", "tokens"}, wantErr: "run `a2a serve tokens list`"},
		{name: "pair forget", args: []string{"pair", "forget", agentURL}, wantErr: "run `a2a serve tokens rm " + agentURL + "`"},
		{name: "dir on a command without an agent", args: []string{"tokens", "list", "--dir", "/tmp"}, wantErr: "unknown flag: --dir"},
		{name: "timeout on a command without requests", args: []string{"logs", "--timeout", "1s"}, wantErr: "unknown flag: --timeout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := runCMD(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("a2a serve %s error = %v, want %q", strings.Join(tc.args, " "), err, tc.wantErr)
			}
			if got := clierr.Classify(err).Code; got != clierr.CodeUsage {
				t.Fatalf("a2a serve %s error code = %v, want %v", strings.Join(tc.args, " "), got, clierr.CodeUsage)
			}
		})
	}
}

func TestPluginInfo(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if code := printPluginInfo(&buf); code != 0 {
		t.Fatalf("printPluginInfo() = %d, want 0", code)
	}
	var got pluginInfo
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", buf.String(), err)
	}
	if got.Name != "serve" || got.Description != serveShort {
		t.Fatalf("printPluginInfo() = %+v, want name serve and the command description", got)
	}
}
