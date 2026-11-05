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

package recipe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseSource(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		in       string
		want     Source
		wantName string
	}{
		{
			name:     "tree URL",
			in:       "https://github.com/acme/agents/tree/release/v1.2/experts/elk",
			want:     Source{Repo: "https://github.com/acme/agents.git", treeRest: "release/v1.2/experts/elk"},
			wantName: "elk",
		},
		{
			name:     "tree URL on another host with a trailing slash",
			in:       "https://ghe.example.com/acme/agents.git/tree/main/",
			want:     Source{Repo: "https://ghe.example.com/acme/agents.git", treeRest: "main"},
			wantName: "main",
		},
		{
			name:     "repository URL",
			in:       "https://github.com/acme/agents",
			want:     Source{Repo: "https://github.com/acme/agents.git"},
			wantName: "agents",
		},
		{
			name:     "repository URL with ref and path",
			in:       "https://github.com/acme/agents.git#v1.2:experts/elk/",
			want:     Source{Repo: "https://github.com/acme/agents.git", Ref: "v1.2", Path: "experts/elk"},
			wantName: "elk",
		},
		{
			name:     "deeper https URL",
			in:       "https://gitlab.example.com/group/sub/agents.git#:elk",
			want:     Source{Repo: "https://gitlab.example.com/group/sub/agents.git", Path: "elk"},
			wantName: "elk",
		},
		{
			name:     "scp-like URL",
			in:       "git@github.com:acme/agents.git#v1.2:experts/elk",
			want:     Source{Repo: "git@github.com:acme/agents.git", Ref: "v1.2", Path: "experts/elk"},
			wantName: "elk",
		},
		{
			name:     "scp-like URL with ref only",
			in:       "git@github.com:acme/agents.git#main",
			want:     Source{Repo: "git@github.com:acme/agents.git", Ref: "main"},
			wantName: "agents",
		},
		{
			name:     "ssh URL",
			in:       "ssh://git@example.com/acme/agents.git#:.",
			want:     Source{Repo: "ssh://git@example.com/acme/agents.git"},
			wantName: "agents",
		},
		{
			name:     "file URL",
			in:       "file:///srv/git/agents.git#main:elk",
			want:     Source{Repo: "file:///srv/git/agents.git", Ref: "main", Path: "elk"},
			wantName: "elk",
		},
		{
			name:     "relative directory",
			in:       "./experts/elk",
			want:     Source{Dir: filepath.Join(wd, "experts", "elk")},
			wantName: "elk",
		},
		{
			name:     "directory in home",
			in:       "~/src/agents/elk",
			want:     Source{Dir: filepath.Join(home, "src", "agents", "elk")},
			wantName: "elk",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseSource(tc.in)
			if err != nil {
				t.Fatalf("ParseSource(%q) error = %v", tc.in, err)
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(Source{})); diff != "" {
				t.Fatalf("ParseSource(%q) wrong result (-want +got) diff = %s", tc.in, diff)
			}
			if got.Name() != tc.wantName {
				t.Fatalf("Source.Name() = %q, want %q", got.Name(), tc.wantName)
			}
			again, err := ParseSource(got.String())
			if err != nil {
				t.Fatalf("ParseSource(%q) error = %v", got.String(), err)
			}
			if diff := cmp.Diff(got, again, cmp.AllowUnexported(Source{})); diff != "" {
				t.Fatalf("ParseSource(Source.String()) wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestParseSourceRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{name: "empty", in: "", wantErr: "no recipe source"},
		{name: "option", in: "--upload-pack=x", wantErr: "invalid recipe source"},
		{name: "http", in: "http://github.com/acme/agents", wantErr: "no transport security"},
		{name: "git protocol", in: "git://github.com/acme/agents", wantErr: "no transport security"},
		{name: "other scheme", in: "ext::sh -c touch% /tmp/x", wantErr: "invalid recipe source"},
		{name: "unsupported scheme", in: "ftp://example.com/agents", wantErr: "unsupported scheme"},
		{name: "password", in: "https://user:hunter2@github.com/acme/agents", wantErr: "credentials in the URL"},
		{name: "token as user", in: "https://ghp_token@github.com/acme/agents", wantErr: "credentials in the URL"},
		{name: "ssh password", in: "ssh://git:hunter2@example.com/agents.git", wantErr: "credentials in the URL"},
		{name: "blob URL", in: "https://github.com/acme/agents/blob/main/AGENTS.md", wantErr: "is a file"},
		{name: "tree URL without ref", in: "https://github.com/acme/agents/tree/", wantErr: "no ref after /tree/"},
		{name: "tree URL with fragment", in: "https://github.com/acme/agents/tree/main#x", wantErr: "takes no #<ref>:<path>"},
		{name: "path out of the repository", in: "git@github.com:acme/agents.git#main:../x", wantErr: "leaves the repository"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseSource(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParseSource(%q) error = %v, want %q", tc.in, err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("ParseSource(%q) error = %v, want the password redacted", tc.in, err)
			}
		})
	}
}
