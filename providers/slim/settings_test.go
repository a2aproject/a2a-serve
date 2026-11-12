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

package main

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		url     string
		want    settings
		wantErr bool
	}{
		{
			name: "defaults",
			url:  "http://127.0.0.1:46357",
			want: settings{endpoint: "http://127.0.0.1:46357", org: "a2a", ns: "default"},
		},
		{
			name: "all settings",
			url:  "http://127.0.0.1:46357/?org=acme&namespace=demo&peers=reviewer,coder",
			want: settings{endpoint: "http://127.0.0.1:46357", org: "acme", ns: "demo", peers: []string{"reviewer", "coder"}},
		},
		{
			name: "https is TLS",
			url:  "https://slim.example.com:443?namespace=demo",
			want: settings{endpoint: "https://slim.example.com:443", tls: true, org: "a2a", ns: "demo"},
		},
		{
			name: "peers trimmed and deduplicated",
			url:  "http://node:46357?peers=+coder,,coder,+reviewer+",
			want: settings{endpoint: "http://node:46357", org: "a2a", ns: "default", peers: []string{"coder", "reviewer"}},
		},
		{name: "unknown scheme", url: "slim://node:46357", wantErr: true},
		{name: "no host", url: "http:///?namespace=demo", wantErr: true},
		{name: "credentials", url: "http://user:secret@node:46357", wantErr: true},
		{name: "path", url: "http://node:46357/demo", wantErr: true},
		{name: "unknown setting", url: "http://node:46357?nmespace=demo", wantErr: true},
		{name: "repeated setting", url: "http://node:46357?org=a&org=b", wantErr: true},
		{name: "org with slash", url: "http://node:46357?org=a%2Fb", wantErr: true},
		{name: "empty namespace", url: "http://node:46357?namespace=", wantErr: true},
		{name: "upper-case peer", url: "http://node:46357?peers=Coder", wantErr: true},
		{name: "peer with underscore", url: "http://node:46357?peers=code_r", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseSettings(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSettings(%q) error = %v, want error %v", tt.url, err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got, cmp.AllowUnexported(settings{})); diff != "" {
				t.Fatalf("parseSettings(%q) wrong result (-want +got) diff = %s", tt.url, diff)
			}
		})
	}
}

func TestRoute(t *testing.T) {
	t.Parallel()
	coder := name{org: "a2a", ns: "demo", agent: "coder"}
	tests := []struct {
		path     string
		wantName name
		wantRest string
		wantOK   bool
	}{
		{path: "/a2a/demo/coder", wantName: coder, wantRest: "/", wantOK: true},
		{path: "/a2a/demo/coder/", wantName: coder, wantRest: "/", wantOK: true},
		{path: "/a2a/demo/coder/.well-known/agent-card.json", wantName: coder, wantRest: "/.well-known/agent-card.json", wantOK: true},
		{path: "/a2a/demo/coder/tasks/t%2F1:subscribe", wantName: coder, wantRest: "/tasks/t%2F1:subscribe", wantOK: true},
		{path: "/agntcy/demo/echo_agent", wantName: name{org: "agntcy", ns: "demo", agent: "echo_agent"}, wantRest: "/", wantOK: true},
		{path: "/"},
		{path: "/a2a/demo"},
		{path: "/a2a//coder"},
		{path: "/a2a/../coder"},
		{path: "/a2a/demo/co%2Fder"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			n, rest, ok := route(tt.path)
			if ok != tt.wantOK {
				t.Fatalf("route(%q) ok = %v, want %v", tt.path, ok, tt.wantOK)
			}
			if diff := cmp.Diff(tt.wantName, n, cmp.AllowUnexported(name{})); diff != "" {
				t.Fatalf("route(%q) wrong name (-want +got) diff = %s", tt.path, diff)
			}
			if rest != tt.wantRest {
				t.Fatalf("route(%q) rest = %q, want %q", tt.path, rest, tt.wantRest)
			}
		})
	}
}
