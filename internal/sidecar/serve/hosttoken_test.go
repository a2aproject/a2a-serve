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

package serve

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/contact"
	"github.com/a2aproject/a2a-serve/core/egress"
)

func TestResolveSendsTheHostTokenToEveryLoopbackContact(t *testing.T) {
	t.Parallel()
	env := map[string]string{"HOST_AUTH": "host-auth", "REMOTE_AUTH": "remote-auth"}
	cfg := Config{
		Agent:     "opencode",
		Sandbox:   SandboxLocal,
		Workspace: "memory",
		Contacts: []string{
			"host=http://127.0.0.1:8080",
			"local=http://localhost:9999",
			"remote=https://r.example.com",
			"sock=unix:///run/host/a2a.sock",
		},
		ContactAuth: []string{"host=Bearer env:HOST_AUTH", "remote=Bearer env:REMOTE_AUTH"},
		Getenv:      func(name string) string { return env[name] },
		Stdin:       strings.NewReader("host-token\n"),
	}

	got, err := cfg.resolve(context.Background())
	if err != nil {
		t.Fatalf("Config.resolve() error = %v", err)
	}

	token := http.Header{egress.ProxyHeaderToken: {"host-token"}}
	want := []contact.Entry{
		{Name: "host", URL: "http://127.0.0.1:8080", Auth: "Bearer host-auth", Headers: token},
		{Name: "local", URL: "http://localhost:9999", Headers: token},
		{Name: "remote", URL: "https://r.example.com", Auth: "Bearer remote-auth"},
		{Name: "sock", URL: "unix:///run/host/a2a.sock"},
	}
	if diff := cmp.Diff(want, got.contacts); diff != "" {
		t.Fatalf("Config.resolve() contacts wrong result (-want +got) diff = %s", diff)
	}
}

func TestStdinSecretSaysWhatItWaitsFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		waitHint time.Duration
		delay    time.Duration
		want     string
	}{
		{name: "line arrives at once", waitHint: time.Hour, want: ""},
		{name: "line arrives late", waitHint: time.Millisecond, delay: 200 * time.Millisecond, want: "loopback host token: waiting for one line on stdin\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pr, pw := io.Pipe()
			go func() {
				time.Sleep(tc.delay)
				if _, err := io.WriteString(pw, "host-token\n"); err != nil {
					t.Errorf("writing stdin error = %v", err)
				}
			}()
			var prompt bytes.Buffer
			s := &stdinSecret{in: pr, prompt: &prompt, waitHint: tc.waitHint}

			got, err := s.read(context.Background(), "host-token", "loopback host token")
			if err != nil || got != "host-token" {
				t.Fatalf("stdinSecret.read() = %q, %v, want %q", got, err, "host-token")
			}
			if prompt.String() != tc.want {
				t.Fatalf("stdinSecret.read() wrote %q on stderr, want %q", prompt.String(), tc.want)
			}
		})
	}
}
