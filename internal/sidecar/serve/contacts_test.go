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

package serve_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/sidecar/serve"
)

func TestStartRefusesInvalidContacts(t *testing.T) {
	t.Parallel()
	const secret = "s3cret-token"
	env := map[string]string{"TOKEN": secret}
	valid := serve.Config{Agent: "opencode", Workspace: "dir:" + t.TempDir(), Sandbox: serve.SandboxLocal}
	tooMany := make([]string, 101)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("c%d=https://c%d.example.com", i, i)
	}
	tests := []struct {
		name  string
		edit  func(*serve.Config)
		stdin string
		want  string
	}{
		{name: "contact without a URL", edit: func(c *serve.Config) { c.Contacts = []string{"reviewer"} }, want: "invalid --contact: want name=url"},
		{name: "contact URL without a name", edit: func(c *serve.Config) { c.Contacts = []string{"https://user:" + secret + "@r.example.com"} }, want: "invalid --contact: want name=url"},
		{name: "invalid name", edit: func(c *serve.Config) { c.Contacts = []string{"-bad=https://r.example.com"} }, want: "invalid --contact name: want a name of up to 64"},
		{name: "URL with a query as the name", edit: func(c *serve.Config) { c.Contacts = []string{"https://" + secret + "@r.example.com/?a=b"} }, want: "invalid --contact name: want a name of up to 64"},
		{name: "auth without a name", edit: func(c *serve.Config) { c.ContactAuth = []string{"Bearer " + secret} }, want: "invalid --contact-auth: want name=value"},
		{
			name: "auth token that ends in =",
			edit: func(c *serve.Config) {
				c.Contacts, c.ContactAuth = []string{"r=https://r.example.com"}, []string{secret + "=="}
			},
			want: "--contact-auth names a contact that no --contact names; the contacts are: r",
		},
		{name: "name twice", edit: func(c *serve.Config) { c.Contacts = []string{"r=https://a.example.com", "R=https://b.example.com"} }, want: "--contact names R twice"},
		{name: "plain http", edit: func(c *serve.Config) { c.Contacts = []string{"r=http://127.0.0.1:8080"} }, want: "invalid --contact r: a2a-serve-sidecar calls contacts over https:// or a unix:/// socket"},
		{name: "other scheme", edit: func(c *serve.Config) { c.Contacts = []string{"r=ftp://r.example.com?key=" + secret} }, want: "the URL must start with https:// or unix:///"},
		{name: "relative socket", edit: func(c *serve.Config) { c.Contacts = []string{"r=unix://r.sock"} }, want: "invalid --contact r: want unix:///absolute/path"},
		{name: "too many contacts", edit: func(c *serve.Config) { c.Contacts = tooMany }, want: "101 --contact flags: a2a-serve-sidecar takes at most 100"},
		{name: "auth for an unknown contact", edit: func(c *serve.Config) { c.ContactAuth = []string{"r=Bearer " + secret} }, want: "--contact-auth names a contact that no --contact names; the contacts are: none"},
		{
			name: "auth twice",
			edit: func(c *serve.Config) {
				c.Contacts, c.ContactAuth = []string{"r=https://r.example.com"}, []string{"r=Bearer " + secret, "r=Bearer env:TOKEN"}
			},
			want: "--contact-auth names r twice",
		},
		{
			name: "auth without a value",
			edit: func(c *serve.Config) { c.Contacts, c.ContactAuth = []string{"r=https://r.example.com"}, []string{"r="} },
			want: "--contact-auth r: empty header value",
		},
		{
			name: "auth from an unset variable",
			edit: func(c *serve.Config) {
				c.Contacts, c.ContactAuth = []string{"r=https://r.example.com"}, []string{"r=Bearer env:MISSING"}
			},
			want: "--contact-auth r: the variable MISSING is unset or empty",
		},
		{
			name: "auth from stdin twice",
			edit: func(c *serve.Config) {
				c.Contacts = []string{"a=https://a.example.com", "b=unix:///run/b.sock"}
				c.ContactAuth = []string{"a=Bearer -", "b=Bearer -"}
			},
			stdin: secret + "\n" + secret + "\n",
			want:  "--contact-auth b: only one --contact-auth can read its value from stdin",
		},
		{
			name: "auth from empty stdin",
			edit: func(c *serve.Config) {
				c.Contacts, c.ContactAuth = []string{"r=https://r.example.com"}, []string{"r=Bearer -"}
			},
			stdin: "\n",
			want:  "--contact-auth r: stdin had no value",
		},
		{
			name: "auth with a line break",
			edit: func(c *serve.Config) {
				c.Contacts, c.ContactAuth = []string{"r=https://r.example.com"}, []string{"r=Bearer " + secret + "\nX-Other: 1"}
			},
			want: "--contact-auth r: the header value has a line break",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			cfg.Getenv = func(name string) string { return env[name] }
			cfg.Stdin = strings.NewReader(tc.stdin)
			tc.edit(&cfg)

			srv, err := serve.Start(context.Background(), cfg)

			if srv != nil {
				t.Errorf("serve.Start() = %v, want nil", srv)
				if err := srv.Close(context.Background()); err != nil {
					t.Errorf("Server.Close() error = %v", err)
				}
			}
			if _, ok := errors.AsType[*serve.UsageError](err); !ok || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("serve.Start() error = %v, want a UsageError with %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("serve.Start() error = %v, want it without the secret", err)
			}
		})
	}
}

func TestStartStopsReadingTheContactAuthWhenCanceled(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		if err := pw.Close(); err != nil {
			t.Error(err)
		}
	})
	cfg := serve.Config{
		Agent: "opencode", Workspace: "dir:" + t.TempDir(), Sandbox: serve.SandboxLocal,
		Contacts: []string{"r=https://r.example.com"}, ContactAuth: []string{"r=Bearer -"}, Stdin: pr,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		srv, err := serve.Start(ctx, cfg)
		if srv != nil {
			err = errors.Join(errors.New("started"), srv.Close(context.Background()))
		}
		done <- err
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("serve.Start() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("serve.Start() didn't return after its context ended while reading stdin")
	}
}

func TestStartRefusesALongContactAuthLine(t *testing.T) {
	t.Parallel()
	cfg := serve.Config{
		Agent: "opencode", Workspace: "dir:" + t.TempDir(), Sandbox: serve.SandboxLocal,
		Contacts: []string{"r=https://r.example.com"}, ContactAuth: []string{"r=Bearer -"},
		Stdin: strings.NewReader(strings.Repeat("a", 65<<10) + "\n"),
	}

	_, err := serve.Start(context.Background(), cfg)

	if _, ok := errors.AsType[*serve.UsageError](err); !ok || !strings.Contains(err.Error(), "--contact-auth r: the line on stdin is longer than 64 KiB") {
		t.Fatalf("serve.Start() error = %v, want a UsageError for the long line", err)
	}
}
