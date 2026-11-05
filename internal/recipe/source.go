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

// Package recipe fetches recipes, directories that hold a ready-made agent
// with a committed .a2a/config.yaml, and keeps the record of the recipe an
// agent came from.
package recipe

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Source is where a recipe comes from: a directory of a git repository at a
// ref, or a local directory.
type Source struct {
	// Repo is the git URL of the repository, empty for a local directory.
	Repo string
	// Ref is a branch, a tag or a full commit SHA. Empty is the default
	// branch.
	Ref string
	// Path is the slash-separated directory of the recipe in the
	// repository. Empty is the repository root.
	Path string
	// Dir is the absolute path of a local directory.
	Dir string

	// treeRest is the part of a tree URL after /tree/, which holds the ref
	// and the path. Resolving it needs the refs of the repository.
	treeRest string
}

// Local reports whether the source is a local directory.
func (s Source) Local() bool { return s.Dir != "" }

// String formats the source so that [ParseSource] reads it back: the
// directory, or the repository with #<ref>:<path>.
func (s Source) String() string {
	if s.Local() {
		return s.Dir
	}
	out := s.Repo
	if s.treeRest != "" {
		return strings.TrimSuffix(out, ".git") + "/tree/" + s.treeRest
	}
	if s.Ref != "" || s.Path != "" {
		out += "#" + s.Ref
	}
	if s.Path != "" {
		out += ":" + s.Path
	}
	return out
}

// Describe names the source for people: the path in the repository, the
// repository without its scheme, and the ref.
func (s Source) Describe() string {
	if s.Local() {
		return s.Dir
	}
	repo := s.Repo
	if i := strings.Index(repo, "://"); i >= 0 {
		repo = repo[i+3:]
	} else if host, p, ok := strings.Cut(repo, ":"); ok {
		repo = host + "/" + p
	}
	if at := strings.Index(repo, "@"); at >= 0 && at < strings.Index(repo+"/", "/") {
		repo = repo[at+1:]
	}
	repo = strings.TrimSuffix(strings.TrimSuffix(repo, "/"), ".git")
	ref := s.Ref
	if ref == "" {
		ref = "the default branch"
	}
	if s.Path == "" {
		return repo + " at " + ref
	}
	return s.Path + " from " + repo + " at " + ref
}

// Name is the default name of an agent created from the source: the last
// element of its path, or else the name of the repository or directory.
func (s Source) Name() string {
	switch {
	case s.Local():
		return filepath.Base(s.Dir)
	case s.Path != "":
		return path.Base(s.Path)
	case s.treeRest != "":
		return path.Base(s.treeRest)
	}
	repo := strings.TrimSuffix(strings.TrimSuffix(s.Repo, "/"), ".git")
	if i := strings.LastIndexAny(repo, "/:"); i >= 0 {
		repo = repo[i+1:]
	}
	return repo
}

// ParseSource parses a recipe source: a tree URL in the GitHub layout on
// any host, a git URL with an optional #<ref>:<path>, or a local directory.
// Tree URLs keep the ref and the path together until [Fetcher.Fetch]
// splits them with the refs of the repository.
func ParseSource(s string) (Source, error) {
	switch {
	case s == "":
		return Source{}, errors.New("no recipe source")
	case strings.HasPrefix(s, "-"):
		return Source{}, fmt.Errorf("invalid recipe source %q", s)
	case strings.Contains(s, "://"):
		return parseURL(s)
	case isSCPLike(s):
		host, rest, _ := strings.Cut(s, ":")
		if !scpHost.MatchString(host) || strings.HasPrefix(rest, ":") {
			return Source{}, fmt.Errorf("invalid recipe source %q: want a tree URL, a git URL or a local directory", s)
		}
		return parseGit(s)
	}
	return parseLocal(s)
}

// scpHost is the [user@]host part of a git URL such as git@host:path.
var scpHost = regexp.MustCompile(`^([A-Za-z0-9._~-]+@)?([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])$`)

// isSCPLike reports whether s is a git URL such as git@host:path, which
// git recognizes by a colon before the first slash.
func isSCPLike(s string) bool {
	colon := strings.Index(s, ":")
	if colon <= 0 {
		return false
	}
	slash := strings.Index(s, "/")
	return slash < 0 || colon < slash
}

func parseLocal(s string) (Source, error) {
	if strings.HasPrefix(s, "~/") || s == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Source{}, err
		}
		s = filepath.Join(home, strings.TrimPrefix(s, "~"))
	}
	dir, err := filepath.Abs(s)
	if err != nil {
		return Source{}, err
	}
	return Source{Dir: dir}, nil
}

func parseURL(s string) (Source, error) {
	u, err := url.Parse(s)
	if err != nil {
		return Source{}, fmt.Errorf("invalid recipe source %q: %w", s, err)
	}
	switch u.Scheme {
	case "https", "ssh", "file":
	case "http", "git":
		return Source{}, fmt.Errorf("recipe source %s uses %s://, which has no transport security; use https:// or ssh://", u.Redacted(), u.Scheme)
	default:
		return Source{}, fmt.Errorf("recipe source %s: unsupported scheme %q; use https://, ssh:// or a local directory", u.Redacted(), u.Scheme)
	}
	if _, ok := u.User.Password(); ok || (u.Scheme == "https" && u.User != nil) {
		return Source{}, fmt.Errorf("recipe source %s has credentials in the URL; remove them and let git's credential helper or SSH keys supply them", u.Redacted())
	}
	if u.Scheme != "https" {
		return parseGit(s)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) >= 3 && segs[2] == "blob" {
		return Source{}, fmt.Errorf("recipe source %s is a file; a recipe is a directory, use its /tree/ URL", s)
	}
	if len(segs) < 3 || segs[2] != "tree" {
		return parseGit(s)
	}
	if u.Fragment != "" {
		return Source{}, fmt.Errorf("invalid recipe source %q: a tree URL takes no #<ref>:<path>", s)
	}
	rest := strings.Trim(strings.Join(segs[3:], "/"), "/")
	if rest == "" {
		return Source{}, fmt.Errorf("invalid recipe source %q: no ref after /tree/", s)
	}
	repo := url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/" + segs[0] + "/" + strings.TrimSuffix(segs[1], ".git") + ".git"}
	return Source{Repo: repo.String(), treeRest: rest}, nil
}

// parseGit parses a git URL with an optional #<ref>:<path>, the syntax of
// Docker build contexts.
func parseGit(s string) (Source, error) {
	repo, frag, _ := strings.Cut(s, "#")
	repo = strings.TrimSuffix(repo, "/")
	if u, err := url.Parse(repo); err == nil && u.Scheme == "https" {
		if segs := strings.Split(strings.Trim(u.Path, "/"), "/"); len(segs) == 2 && !strings.HasSuffix(segs[1], ".git") {
			repo += ".git"
		}
	}
	ref, p, _ := strings.Cut(frag, ":")
	clean, err := cleanPath(p)
	if err != nil {
		return Source{}, fmt.Errorf("invalid recipe source %q: %w", s, err)
	}
	return Source{Repo: repo, Ref: ref, Path: clean}, nil
}

// cleanPath cleans the slash-separated path of a recipe in its repository,
// which must stay inside the repository.
func cleanPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	clean := path.Clean(strings.Trim(p, "/"))
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q leaves the repository", p)
	}
	return clean, nil
}
