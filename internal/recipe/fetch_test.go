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
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// gitRepo is a repository for fetch tests, served through file://.
type gitRepo struct {
	t   *testing.T
	dir string
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	r := &gitRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *gitRepo) url() string { return "file://" + r.dir }

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files, by slash-separated path, commits them and returns
// the commit's SHA.
func (r *gitRepo) commit(files map[string]string) string {
	r.t.Helper()
	for name, content := range files {
		writeFile(r.t, r.dir, name, content)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "update")
	return r.git("rev-parse", "HEAD")
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFetch(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t)
	first := repo.commit(map[string]string{
		"README.md":                             "repo",
		"experts/elk/AGENTS.md":                 "v1",
		"experts/elk/.a2a/config.yaml":          "agent: opencode\n",
		"experts/other/AGENTS.md":               "other",
		"experts/we*ird/AGENTS.md":              "weird",
		"experts/we*ird/sub/AGENTS.md":          "weird sub",
		"experts/weXird/AGENTS.md":              "not weird",
		"experts/elk-extra/AGENTS.md":           "extra",
		"experts/elk/.agents/skills/x/SKILL.md": "skill",
	})
	repo.git("tag", "-a", "v1.0", "-m", "v1.0")
	repo.git("branch", "release/v1")
	second := repo.commit(map[string]string{"experts/elk/AGENTS.md": "v2"})

	tests := []struct {
		name       string
		src        Source
		wantSource Source
		wantCommit string
		wantFiles  map[string]string
		wantAbsent []string
	}{
		{
			name:       "default branch",
			src:        Source{Repo: repo.url(), Path: "experts/elk"},
			wantSource: Source{Repo: repo.url(), Path: "experts/elk"},
			wantCommit: second,
			wantFiles:  map[string]string{"experts/elk/AGENTS.md": "v2"},
			wantAbsent: []string{"experts/other/AGENTS.md", "experts/elk-extra/AGENTS.md", "README.md"},
		},
		{
			name:       "annotated tag",
			src:        Source{Repo: repo.url(), Ref: "v1.0", Path: "experts/elk"},
			wantSource: Source{Repo: repo.url(), Ref: "v1.0", Path: "experts/elk"},
			wantCommit: first,
			wantFiles:  map[string]string{"experts/elk/AGENTS.md": "v1"},
		},
		{
			name:       "tree rest with a branch with a slash",
			src:        Source{Repo: repo.url(), treeRest: "release/v1/experts/elk"},
			wantSource: Source{Repo: repo.url(), Ref: "release/v1", Path: "experts/elk"},
			wantCommit: first,
			wantFiles:  map[string]string{"experts/elk/AGENTS.md": "v1"},
		},
		{
			name:       "tree rest with a commit",
			src:        Source{Repo: repo.url(), treeRest: first + "/experts/elk"},
			wantSource: Source{Repo: repo.url(), Ref: first, Path: "experts/elk"},
			wantCommit: first,
			wantFiles:  map[string]string{"experts/elk/AGENTS.md": "v1"},
		},
		{
			name:       "path with pattern characters",
			src:        Source{Repo: repo.url(), Path: "experts/we*ird"},
			wantSource: Source{Repo: repo.url(), Path: "experts/we*ird"},
			wantCommit: second,
			wantFiles:  map[string]string{"experts/we*ird/AGENTS.md": "weird", "experts/we*ird/sub/AGENTS.md": "weird sub"},
			wantAbsent: []string{"experts/weXird/AGENTS.md"},
		},
		{
			name:       "repository root",
			src:        Source{Repo: repo.url()},
			wantSource: Source{Repo: repo.url()},
			wantCommit: second,
			wantFiles:  map[string]string{"README.md": "repo", "experts/other/AGENTS.md": "other"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			work := t.TempDir()
			got, err := Fetcher{Git: ExecGit(false)}.Fetch(t.Context(), tc.src, work)
			if err != nil {
				t.Fatalf("Fetcher.Fetch() error = %v", err)
			}
			want := &Fetched{Source: tc.wantSource, Commit: tc.wantCommit, Dir: filepath.Join(work, filepath.FromSlash(tc.wantSource.Path))}
			if diff := cmp.Diff(want, got, cmp.AllowUnexported(Source{})); diff != "" {
				t.Fatalf("Fetcher.Fetch() wrong result (-want +got) diff = %s", diff)
			}
			for name, content := range tc.wantFiles {
				data, err := os.ReadFile(filepath.Join(work, filepath.FromSlash(name)))
				if err != nil {
					t.Fatalf("reading %s: %v", name, err)
				}
				if string(data) != content {
					t.Fatalf("%s = %q, want %q", name, data, content)
				}
			}
			for _, name := range tc.wantAbsent {
				if _, err := os.Lstat(filepath.Join(work, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Lstat(%s) error = %v, want it absent", name, err)
				}
			}
		})
	}
}

func TestFetchErrors(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t)
	repo.commit(map[string]string{"elk/AGENTS.md": "v1", "README.md": "repo"})
	tests := []struct {
		name    string
		src     Source
		wantErr string
	}{
		{name: "missing path", src: Source{Repo: repo.url(), Path: "nope"}, wantErr: "at HEAD has no directory nope"},
		{name: "path is a file", src: Source{Repo: repo.url(), Ref: "main", Path: "README.md"}, wantErr: "at main has no directory README.md"},
		{name: "missing ref", src: Source{Repo: repo.url(), Ref: "nope", Path: "elk"}, wantErr: "fetching"},
		{name: "tree rest without a known ref", src: Source{Repo: repo.url(), treeRest: "nope/elk"}, wantErr: `no branch or tag that starts "nope/elk"`},
		{name: "missing repository", src: Source{Repo: "file://" + filepath.Join(t.TempDir(), "nope")}, wantErr: "fetching"},
		{name: "missing local directory", src: Source{Dir: filepath.Join(t.TempDir(), "nope")}, wantErr: "no such file or directory"},
		{name: "local file", src: Source{Dir: filepath.Join(repo.dir, "README.md")}, wantErr: "is not a directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Fetcher{Git: ExecGit(false)}.Fetch(t.Context(), tc.src, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Fetcher.Fetch() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestFetchLocal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got, err := Fetcher{}.Fetch(t.Context(), Source{Dir: dir}, t.TempDir())
	if err != nil {
		t.Fatalf("Fetcher.Fetch() error = %v", err)
	}
	if diff := cmp.Diff(&Fetched{Source: Source{Dir: dir}, Dir: dir}, got, cmp.AllowUnexported(Source{})); diff != "" {
		t.Fatalf("Fetcher.Fetch() wrong result (-want +got) diff = %s", diff)
	}
}

func TestFetchResolvesTreeRest(t *testing.T) {
	t.Parallel()
	const refs = "aaa\trefs/heads/main\n" +
		"bbb\trefs/heads/release\n" +
		"ccc\trefs/heads/release/v1\n" +
		"ddd\trefs/tags/v2.0\n" +
		"eee\trefs/tags/v2.0^{}\n" +
		"fff\trefs/pull/1/head\n"
	tests := []struct {
		name     string
		rest     string
		wantRef  string
		wantPath string
	}{
		{name: "branch", rest: "main/experts/elk", wantRef: "main", wantPath: "experts/elk"},
		{name: "longest branch", rest: "release/v1/elk", wantRef: "release/v1", wantPath: "elk"},
		{name: "shorter branch", rest: "release/v2/elk", wantRef: "release", wantPath: "v2/elk"},
		{name: "tag", rest: "v2.0", wantRef: "v2.0"},
		{name: "commit", rest: strings.Repeat("a1", 20) + "/elk", wantRef: strings.Repeat("a1", 20), wantPath: "elk"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			git := func(_ context.Context, _ string, args ...string) ([]byte, error) {
				calls = append(calls, strings.Join(args, " "))
				if args[0] == "ls-remote" {
					return []byte(refs), nil
				}
				return nil, errors.New("stop")
			}
			src := Source{Repo: "https://github.com/acme/agents.git", treeRest: tc.rest}
			got, err := Fetcher{Git: git}.resolve(t.Context(), src)
			if err != nil {
				t.Fatalf("Fetcher.resolve() error = %v", err)
			}
			want := Source{Repo: src.Repo, Ref: tc.wantRef, Path: tc.wantPath}
			if diff := cmp.Diff(want, got, cmp.AllowUnexported(Source{})); diff != "" {
				t.Fatalf("Fetcher.resolve() wrong result (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff([]string{"ls-remote --heads --tags https://github.com/acme/agents.git"}, calls); diff != "" {
				t.Fatalf("Fetcher.resolve() wrong git calls (-want +got) diff = %s", diff)
			}
		})
	}
}
