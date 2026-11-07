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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// FetchTimeout bounds a fetch of a git source.
const FetchTimeout = 5 * time.Minute

// Runner runs git with args in dir and returns its standard output. It
// returns an error with git's standard error when git fails.
type Runner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// ExecGit returns a Runner that runs the host's git, so that the user's SSH
// keys, credential helpers and insteadOf rules apply. Unless interactive,
// git fails where it would ask for credentials.
func ExecGit(interactive bool) Runner {
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		if !interactive {
			cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, gitError(args[0], err, strings.TrimSpace(stderr.String()))
		}
		return stdout.Bytes(), nil
	}
}

// noAccess are the messages of git for a repository it can't read. Git
// hosts answer a missing repository like a private one.
var noAccess = []string{"terminal prompts disabled", "Authentication failed", "Repository not found", "Permission denied (publickey)"}

// gitError returns the error of the git command that failed with err and
// wrote stderr.
func gitError(command string, err error, stderr string) error {
	if slices.ContainsFunc(noAccess, func(m string) bool { return strings.Contains(stderr, m) }) {
		return fmt.Errorf("git %s: the repository doesn't exist, or git has no credentials for it; check the URL, or set up an SSH key or a git credential helper for it (git said: %s)", command, stderr)
	}
	if stderr != "" {
		return fmt.Errorf("git %s: %w: %s", command, err, stderr)
	}
	return fmt.Errorf("git %s: %w", command, err)
}

// Fetcher fetches recipes.
type Fetcher struct {
	Git Runner
}

// Fetched is a fetched recipe.
type Fetched struct {
	// Source is the source with the ref and path of a tree URL split.
	Source Source
	// Commit is the full SHA of the fetched commit, empty for a local
	// directory.
	Commit string
	// Dir is the directory holding the recipe.
	Dir string
}

// Fetch fetches the recipe of src. A git source is checked out into
// workDir, an empty directory, with only the recipe's directory; a local
// source is used where it is.
func (f Fetcher) Fetch(ctx context.Context, src Source, workDir string) (*Fetched, error) {
	if src.Local() {
		if err := checkDir(src.Dir, src.Dir); err != nil {
			return nil, err
		}
		return &Fetched{Source: src, Dir: src.Dir}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	src, err := f.resolve(ctx, src)
	if err != nil {
		return nil, err
	}
	ref := src.Ref
	if ref == "" {
		ref = "HEAD"
	}
	steps := [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", src.Repo},
		{"fetch", "-q", "--depth=1", "--filter=blob:none", "origin", ref},
	}
	if src.Path != "" {
		steps = append(steps, []string{"sparse-checkout", "set", "--no-cone", "/" + escapePattern(src.Path) + "/"})
	}
	steps = append(steps, []string{"-c", "advice.detachedHead=false", "checkout", "-q", "FETCH_HEAD"})
	for _, args := range steps {
		if _, err := f.Git(ctx, workDir, args...); err != nil {
			return nil, fmt.Errorf("fetching %s: %w", src, err)
		}
	}
	out, err := f.Git(ctx, workDir, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", src, err)
	}
	dir := filepath.Join(workDir, filepath.FromSlash(src.Path))
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s at %s has no directory %s", src.Repo, ref, src.Path)
	}
	return &Fetched{Source: src, Commit: strings.TrimSpace(string(out)), Dir: dir}, nil
}

func checkDir(dir, name string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("recipe %s: %w", name, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("recipe %s is not a directory", name)
	}
	return nil
}

// escapePattern escapes the characters of a gitignore pattern, which
// sparse-checkout patterns are, so that p matches only itself.
func escapePattern(p string) string {
	var sb strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`\*?[`, r) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

var fullSHA = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// resolve splits the rest of a tree URL into the ref and the path. A ref
// may contain slashes, so the longest branch or tag that starts the rest
// is the ref. A full commit SHA works too.
func (f Fetcher) resolve(ctx context.Context, src Source) (Source, error) {
	if src.treeRest == "" {
		return src, nil
	}
	rest := src.treeRest
	src.treeRest = ""
	out, err := f.Git(ctx, "", "ls-remote", "--heads", "--tags", src.Repo)
	if err != nil {
		return Source{}, fmt.Errorf("listing the branches and tags of %s: %w", src.Repo, err)
	}
	ref := ""
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		_, name, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			continue
		}
		name = strings.TrimSuffix(name, "^{}")
		name, ok = strings.CutPrefix(name, "refs/heads/")
		if !ok {
			name, ok = strings.CutPrefix(name, "refs/tags/")
		}
		if ok && len(name) > len(ref) && (rest == name || strings.HasPrefix(rest, name+"/")) {
			ref = name
		}
	}
	if err := sc.Err(); err != nil {
		return Source{}, err
	}
	if ref == "" {
		first, _, _ := strings.Cut(rest, "/")
		if !fullSHA.MatchString(first) {
			return Source{}, fmt.Errorf("%s has no branch or tag that starts %q", src.Repo, rest)
		}
		ref = first
	}
	p, err := cleanPath(strings.TrimPrefix(rest, ref))
	if err != nil {
		return Source{}, err
	}
	src.Ref, src.Path = ref, p
	return src, nil
}
