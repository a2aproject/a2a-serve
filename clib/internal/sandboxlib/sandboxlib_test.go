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

package sandboxlib

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/clib/internal/bridge"
	"github.com/a2aproject/a2a-serve/sandbox"
	"github.com/a2aproject/a2a-serve/sandbox/sandboxnet"
)

// answer answers the next question of calls with answer, and returns the
// question.
func answer(t *testing.T, calls *bridge.Calls, answer string) <-chan bridge.Event {
	t.Helper()
	got := make(chan bridge.Event, 1)
	go func() {
		e, ok := calls.Next(5 * time.Second)
		if !ok {
			t.Errorf("Calls.Next() = false, want a question")
			close(got)
			return
		}
		got <- e
		if err := calls.Reply(e.ID, json.RawMessage(answer)); err != nil {
			t.Errorf("Calls.Reply() error = %v", err)
		}
	}()
	return got
}

func TestInterceptorConnect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		answer string
		want   sandboxnet.ConnectAction
	}{
		{answer: `{"action": "allow"}`, want: sandboxnet.ConnectAllow},
		{answer: `{"action": "inspect"}`, want: sandboxnet.ConnectInspect},
		{answer: `{"action": "policy"}`, want: sandboxnet.ConnectPolicy},
		{answer: `{"action": "deny"}`, want: sandboxnet.ConnectDeny},
		{answer: `{"action": "maybe"}`, want: sandboxnet.ConnectDeny},
		{answer: `"not an object"`, want: sandboxnet.ConnectDeny},
	}
	for _, tc := range tests {
		t.Run(tc.answer, func(t *testing.T) {
			t.Parallel()
			calls := bridge.NewCalls(nil)
			t.Cleanup(calls.Close)
			asked := answer(t, calls, tc.answer)
			got := (&interceptor{calls: calls, sandbox: 7}).Connect(context.Background(), "example.com", 443)
			if got != tc.want {
				t.Fatalf("interceptor.Connect() = %v, want %v", got, tc.want)
			}
			q := <-asked
			if diff := cmp.Diff(map[string]any{"sandbox": int64(7), "host": "example.com", "port": uint16(443)}, q.Data); diff != "" {
				t.Fatalf("question wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestInterceptorRequest(t *testing.T) {
	t.Parallel()
	newReq := func() *http.Request {
		r, err := http.NewRequest(http.MethodPost, "http://llm.internal/v1/chat", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("X-Api-Key", "fake")
		return r
	}

	t.Run("respond", func(t *testing.T) {
		t.Parallel()
		calls := bridge.NewCalls(nil)
		t.Cleanup(calls.Close)
		answer(t, calls, `{"action": "respond", "status": 201, "headers": {"X-A": ["1"]}, "body": "aGk="}`)
		resp, err := (&interceptor{calls: calls}).Request(context.Background(), newReq())
		if err != nil {
			t.Fatalf("interceptor.Request() error = %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 201 || string(body) != "hi" || resp.Header.Get("X-A") != "1" {
			t.Fatalf("interceptor.Request() = %d %q %v, want 201 %q", resp.StatusCode, body, resp.Header, "hi")
		}
	})

	t.Run("forward", func(t *testing.T) {
		t.Parallel()
		calls := bridge.NewCalls(nil)
		t.Cleanup(calls.Close)
		answer(t, calls, `{"action": "forward", "url": "https://model.example/v1/chat", "set_headers": {"Authorization": "Bearer t"}, "remove_headers": ["X-Api-Key"]}`)
		r := newReq()
		resp, err := (&interceptor{calls: calls}).Request(context.Background(), r)
		if err != nil || resp != nil {
			t.Fatalf("interceptor.Request() = %v, %v, want nil, nil", resp, err)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{r.URL.String(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), string(body)}
		if diff := cmp.Diff([]string{"https://model.example/v1/chat", "Bearer t", "", "{}"}, got); diff != "" {
			t.Fatalf("forwarded request wrong result (-want +got) diff = %s", diff)
		}
	})

	t.Run("deny", func(t *testing.T) {
		t.Parallel()
		calls := bridge.NewCalls(nil)
		t.Cleanup(calls.Close)
		answer(t, calls, `{"action": "deny"}`)
		resp, err := (&interceptor{calls: calls}).Request(context.Background(), newReq())
		if err != nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("interceptor.Request() = %v, %v, want 403", resp, err)
		}
	})

	t.Run("unknown action", func(t *testing.T) {
		t.Parallel()
		calls := bridge.NewCalls(nil)
		t.Cleanup(calls.Close)
		answer(t, calls, `{"action": "maybe"}`)
		if _, err := (&interceptor{calls: calls}).Request(context.Background(), newReq()); err == nil {
			t.Fatal("interceptor.Request() error = nil, want an error")
		}
	})
}

func TestLibraryRefusesBadCalls(t *testing.T) {
	t.Parallel()
	l := New()
	t.Cleanup(l.Calls.Close)
	for name, got := range map[string]string{
		"spec":     l.Open(`{`),
		"name":     l.Open(`{"image": "x"}`),
		"backend":  l.Open(`{"name": "x", "backend": "vm"}`),
		"start":    l.Start(42),
		"files":    l.ReadFile(42, "a"),
		"reply":    l.Reply(1, `{}`),
		"not json": l.Reply(1, `{`),
	} {
		if !strings.Contains(got, `"error"`) {
			t.Errorf("%s: call = %s, want an error", name, got)
		}
	}
}

func TestOrphaned(t *testing.T) {
	t.Parallel()
	scope, ok := ownerScope()
	if !ok {
		t.Skip("this host has no /proc")
	}
	self, ok := owner(os.Getpid())
	if !ok {
		t.Fatal("owner() = false, want the owner of this process")
	}
	start, err := startTime(os.Getpid())
	if err != nil {
		t.Fatalf("startTime() error = %v", err)
	}
	tests := []struct {
		value string
		want  bool
	}{
		{value: self, want: false},
		{value: fmt.Sprintf("%s/%d/%s0", scope, os.Getpid(), start), want: true},
		{value: scope + "/999999999/1", want: true},
		{value: "another-host/4026531836/0/999999999/1", want: false},
		{value: scope + "0/999999999/1", want: false},
		{value: "malformed", want: false},
	}
	for _, tc := range tests {
		if got := orphaned(tc.value, scope); got != tc.want {
			t.Errorf("orphaned(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestPruneRemovesOrphanedVolumes(t *testing.T) {
	ctx := context.Background()
	if _, err := sandbox.CheckDocker(ctx); err != nil {
		t.Skipf("docker isn't available: %v", err)
	}
	scope, ok := ownerScope()
	if !ok {
		t.Skip("this host has no /proc")
	}
	self, ok := owner(os.Getpid())
	if !ok {
		t.Fatal("owner() = false, want the owner of this process")
	}
	suffix := strconv.Itoa(os.Getpid())
	orphan, live := "a2a-sandboxlib-test-orphan-"+suffix, "a2a-sandboxlib-test-live-"+suffix
	for name, value := range map[string]string{orphan: scope + "/999999999/1", live: self} {
		if out, err := exec.Command("docker", "volume", "create", "--label", OwnerLabel+"="+value, name).CombinedOutput(); err != nil {
			t.Fatalf("docker volume create error = %v: %s", err, out)
		}
		t.Cleanup(func() {
			if out, err := exec.Command("docker", "volume", "rm", "-f", name).CombinedOutput(); err != nil {
				t.Errorf("docker volume rm error = %v: %s", err, out)
			}
		})
	}
	if err := prune(ctx); err != nil {
		t.Fatalf("prune() error = %v", err)
	}
	if err := exec.Command("docker", "volume", "inspect", orphan).Run(); err == nil {
		t.Fatalf("volume %s of a process that is gone exists after prune()", orphan)
	}
	if err := exec.Command("docker", "volume", "inspect", live).Run(); err != nil {
		t.Fatalf("volume %s of this process after prune() error = %v, want it kept", live, err)
	}
}
