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

package acplib

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

var fakeACP string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "acplib-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeACP = filepath.Join(dir, "fakeacp")
	build := exec.Command("go", "build", "-o", fakeACP, "../../../internal/agentsrv/testdata/fakeacp")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building fakeacp: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", s, err)
	}
	if e, ok := m["error"]; ok {
		t.Fatalf("call error = %v", e)
	}
	return m
}

// collect reads the events of stream h until its end, and returns the
// text of the response artifact and the final task state.
func collect(t *testing.T, l *Library, h int64) (string, string) {
	t.Helper()
	var text strings.Builder
	state := ""
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		m := decode(t, l.StreamNext(h, time.Second))
		if m["done"] == true {
			return text.String(), state
		}
		ev, ok := m["event"].(map[string]any)
		if !ok {
			continue
		}
		if au, ok := ev["artifactUpdate"].(map[string]any); ok {
			for _, p := range au["artifact"].(map[string]any)["parts"].([]any) {
				if s, ok := p.(map[string]any)["text"].(string); ok {
					text.WriteString(s)
				}
			}
		}
		if su, ok := ev["statusUpdate"].(map[string]any); ok {
			state = su["status"].(map[string]any)["state"].(string)
		}
		if task, ok := ev["task"].(map[string]any); ok {
			state = task["status"].(map[string]any)["state"].(string)
		}
	}
	t.Fatal("the stream didn't end")
	return "", ""
}

func send(t *testing.T, l *Library, executor int64, contextID, text string) int64 {
	t.Helper()
	req, err := json.Marshal(map[string]any{"message": map[string]any{"contextId": contextID, "parts": []any{map[string]any{"text": text}}}})
	if err != nil {
		t.Fatal(err)
	}
	return int64(decode(t, l.Send(executor, string(req)))["stream"].(float64))
}

func TestExecutorRunsProcess(t *testing.T) {
	t.Parallel()
	l := New()
	cfg := fmt.Sprintf(`{"agent": {"argv": [%q], "dir": %q}}`, fakeACP, t.TempDir())
	h := int64(decode(t, l.NewExecutor(cfg))["handle"].(float64))
	t.Cleanup(func() { decode(t, l.CloseExecutor(h)) })

	text, state := collect(t, l, send(t, l, h, "", "hello"))
	if text != "Hello from fakeacp." || state != "TASK_STATE_COMPLETED" {
		t.Fatalf("Send(hello) = %q, %s, want %q, completed", text, state, "Hello from fakeacp.")
	}
}

func TestExecutorStartsExternalAgents(t *testing.T) {
	t.Parallel()
	l := New()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`{"external": true, "per_context": true, "session": {"cwd": %q, "mcp_servers": [{"type": "http", "name": "adk", "url": "http://tools.internal/mcp", "headers": []}]}}`, dir)
	h := int64(decode(t, l.NewExecutor(cfg))["handle"].(float64))
	t.Cleanup(func() { decode(t, l.CloseExecutor(h)) })
	keys := make(chan string, 4)
	go func() {
		for range 2 {
			ev := decode(t, l.NextEvent(30*time.Second))
			data := ev["data"].(map[string]any)
			keys <- data["key"].(string)
			fd, err := startFake(dir)
			answer := fmt.Sprintf(`{"fd": %d}`, fd)
			if err != nil {
				answer = fmt.Sprintf(`{"error": %q}`, err)
			}
			decode(t, l.Reply(int64(ev["id"].(float64)), answer))
		}
	}()

	text, _ := collect(t, l, send(t, l, h, "ctx-1", "mcp"))
	if !strings.Contains(text, "tools.internal") {
		t.Fatalf("Send(mcp) = %q, want the MCP server of the config", text)
	}
	if _, state := collect(t, l, send(t, l, h, "ctx-2", "hello")); state != "TASK_STATE_COMPLETED" {
		t.Fatalf("Send(hello) state = %s, want completed", state)
	}
	got := []string{<-keys, <-keys}
	if diff := cmp.Diff([]string{"ctx-1", "ctx-2"}, got); diff != "" {
		t.Fatalf("start questions wrong result (-want +got) diff = %s", diff)
	}
}

// startFake starts fakeacp on one end of a socket pair and returns the
// other, as the sandbox library does.
func startFake(dir string) (int, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	child := os.NewFile(uintptr(fds[0]), "child")
	cmd := exec.Command(fakeACP)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout = child, child
	err = cmd.Start()
	if cerr := child.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return -1, err
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			fmt.Fprintln(os.Stderr, "fakeacp:", err)
		}
	}()
	return fds[1], nil
}

func TestExecutorFailsWithoutAnswer(t *testing.T) {
	t.Parallel()
	l := New()
	if m := l.NewExecutor(`{}`); !strings.Contains(m, "error") {
		t.Fatalf("NewExecutor({}) = %s, want an error", m)
	}
	if m := l.Reply(99, `{}`); !strings.Contains(m, "error") {
		t.Fatalf("Reply(99) = %s, want an error", m)
	}
}

func TestExecutorAsksSessionParams(t *testing.T) {
	t.Parallel()
	l := New()
	cfg := fmt.Sprintf(`{"id": "e1", "agent": {"argv": [%q], "dir": %q}, "per_context": true, "ask_session": true}`, fakeACP, t.TempDir())
	h := int64(decode(t, l.NewExecutor(cfg))["handle"].(float64))
	t.Cleanup(func() { decode(t, l.CloseExecutor(h)) })
	asked := make(chan map[string]any, 4)
	go func() {
		for range 2 {
			ev := decode(t, l.NextEvent(30*time.Second))
			data := ev["data"].(map[string]any)
			asked <- map[string]any{"type": ev["type"], "id": data["id"], "key": data["key"]}
			answer := fmt.Sprintf(`{"mcp_servers": [{"type": "http", "name": "adk", "url": "http://127.0.0.1:1/mcp/%s", "headers": []}]}`, data["key"])
			decode(t, l.Reply(int64(ev["id"].(float64)), answer))
		}
	}()

	first, _ := collect(t, l, send(t, l, h, "ctx-1", "mcp"))
	second, _ := collect(t, l, send(t, l, h, "ctx-2", "mcp"))

	if !strings.Contains(first, "/mcp/ctx-1") || !strings.Contains(second, "/mcp/ctx-2") {
		t.Fatalf("Send(mcp) = %q, %q, want the MCP server of each session's answer", first, second)
	}
	got := []map[string]any{<-asked, <-asked}
	want := []map[string]any{{"type": "session", "id": "e1", "key": "ctx-1"}, {"type": "session", "id": "e1", "key": "ctx-2"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("session questions wrong result (-want +got) diff = %s", diff)
	}
}
