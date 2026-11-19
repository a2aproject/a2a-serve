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

package fsysmask_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/fsys"
	"github.com/a2aproject/a2a-serve/internal/fsysmask"
)

func write(t *testing.T, f fsys.FS, p, data string) {
	t.Helper()
	if _, err := f.Write(context.Background(), p, strings.NewReader(data), 0o644, ""); err != nil {
		t.Fatalf("FS.Write(%q) error = %v", p, err)
	}
}

func read(t *testing.T, f fsys.FS, p string) string {
	t.Helper()
	rc, err := f.Open(context.Background(), p)
	if err != nil {
		t.Fatalf("FS.Open(%q) error = %v", p, err)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading %q error = %v", p, err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("closing %q error = %v", p, err)
	}
	return string(data)
}

func names(t *testing.T, f fsys.FS, dir string) []string {
	t.Helper()
	infos, err := f.ReadDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("FS.ReadDir(%q) error = %v", dir, err)
	}
	out := make([]string, len(infos))
	for i, info := range infos {
		out[i] = info.Path
	}
	return out
}

func newMasked(t *testing.T, readOnly bool) (tree, masked fsys.FS) {
	t.Helper()
	tree = fsys.Memory()
	write(t, tree, ".mcp.json", `{"mcpServers":{"evil":{}}}`)
	write(t, tree, "main.go", "package main")
	write(t, tree, "sub/.a2a/secret", "secret")
	write(t, tree, "sub/code.go", "package sub")
	write(t, tree, ".opencode/agent.md", "host's")
	return tree, fsysmask.New(tree, fsysmask.Rules{
		ReadOnly: readOnly,
		Files:    map[string][]byte{".mcp.json": []byte("{}"), "opencode.json": []byte("{}")},
		Private:  []string{".opencode"},
		Hidden:   func(name string) bool { return name == ".a2a" },
	})
}

func TestMaskShowsMasks(t *testing.T) {
	t.Parallel()
	_, m := newMasked(t, false)
	ctx := context.Background()
	if got := read(t, m, ".mcp.json"); got != "{}" {
		t.Fatalf("FS.Open(.mcp.json) = %q, want the mask", got)
	}
	if diff := cmp.Diff([]string{".mcp.json", ".opencode", "main.go", "sub"}, names(t, m, ".")); diff != "" {
		t.Fatalf("FS.ReadDir(.) wrong entries (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff([]string{"sub/code.go"}, names(t, m, "sub")); diff != "" {
		t.Fatalf("FS.ReadDir(sub) wrong entries (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff([]string{}, names(t, m, ".opencode")); diff != "" {
		t.Fatalf("FS.ReadDir(.opencode) wrong entries (-want +got) diff = %s", diff)
	}
	for _, p := range []string{"sub/.a2a", "sub/.a2a/secret", "opencode.json"} {
		if _, err := m.Stat(ctx, p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("FS.Stat(%q) error = %v, want %v", p, err, fs.ErrNotExist)
		}
	}
	info, err := m.Stat(ctx, ".mcp.json")
	if err != nil {
		t.Fatalf("FS.Stat(.mcp.json) error = %v", err)
	}
	if info.Size != 2 || info.Mode != 0o444 {
		t.Fatalf("FS.Stat(.mcp.json) = %+v, want 2 bytes, read-only", info)
	}
}

func TestMaskRefusesChanges(t *testing.T) {
	t.Parallel()
	tree, m := newMasked(t, false)
	ctx := context.Background()
	tests := []struct {
		name string
		op   func() error
		want error
	}{
		{name: "write a masked file", op: func() error { _, err := m.Write(ctx, ".mcp.json", strings.NewReader("x"), 0o644, ""); return err }, want: fs.ErrPermission},
		{name: "create a masked file", op: func() error { _, err := m.Write(ctx, "opencode.json", strings.NewReader("x"), 0o644, ""); return err }, want: fs.ErrPermission},
		{name: "create a hidden name", op: func() error { return m.Mkdir(ctx, "x/.a2a", 0o755) }, want: fs.ErrPermission},
		{name: "rename over a masked file", op: func() error { return m.Rename(ctx, "main.go", ".mcp.json") }, want: fs.ErrPermission},
		{name: "rename a hidden entry", op: func() error { return m.Rename(ctx, "sub/.a2a", "stolen") }, want: fs.ErrNotExist},
		{name: "remove a directory with a hidden entry", op: func() error { return m.Remove(ctx, "sub") }, want: fs.ErrPermission},
		{name: "remove a masked file", op: func() error { return m.Remove(ctx, ".mcp.json") }, want: fs.ErrPermission},
		{name: "rename out of a private directory", op: func() error { return m.Rename(ctx, "main.go", ".opencode/main.go") }, want: syscall.EXDEV},
	}
	for _, tc := range tests {
		if err := tc.op(); !errors.Is(err, tc.want) {
			t.Fatalf("%s: error = %v, want %v", tc.name, err, tc.want)
		}
	}
	if got := read(t, tree, ".mcp.json"); got != `{"mcpServers":{"evil":{}}}` {
		t.Fatalf("the tree's .mcp.json = %q, want it unchanged", got)
	}
	if got := read(t, tree, "sub/.a2a/secret"); got != "secret" {
		t.Fatalf("the tree's sub/.a2a/secret = %q, want it unchanged", got)
	}
}

func TestMaskKeepsPrivateWrites(t *testing.T) {
	t.Parallel()
	tree, m := newMasked(t, true)
	write(t, m, ".opencode/package.json", "{}")
	if got := read(t, m, ".opencode/package.json"); got != "{}" {
		t.Fatalf("FS.Open(.opencode/package.json) = %q, want what was written", got)
	}
	if _, err := tree.Stat(context.Background(), ".opencode/package.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the tree's .opencode/package.json error = %v, want %v", err, fs.ErrNotExist)
	}
	if got := read(t, tree, ".opencode/agent.md"); got != "host's" {
		t.Fatalf("the tree's .opencode/agent.md = %q, want it unchanged", got)
	}
}

func TestMaskReadOnly(t *testing.T) {
	t.Parallel()
	_, m := newMasked(t, true)
	ctx := context.Background()
	if _, err := m.Write(ctx, "main.go", strings.NewReader("x"), 0o644, ""); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("FS.Write() of a read-only tree error = %v, want %v", err, fs.ErrPermission)
	}
	if err := m.Remove(ctx, "main.go"); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("FS.Remove() of a read-only tree error = %v, want %v", err, fs.ErrPermission)
	}
	if got := read(t, m, "main.go"); got != "package main" {
		t.Fatalf("FS.Open(main.go) = %q, want it unchanged", got)
	}
}

func TestMaskFiltersChanges(t *testing.T) {
	t.Parallel()
	tree, m := newMasked(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	_, cursor, err := m.Changes(ctx, "")
	if err != nil {
		t.Fatalf("FS.Changes() error = %v", err)
	}
	write(t, tree, "sub/.a2a/other", "x")
	write(t, tree, "new.go", "x")
	changes, _, err := m.Changes(ctx, cursor)
	if err != nil {
		t.Fatalf("FS.Changes() error = %v", err)
	}
	var got []string
	for _, c := range changes {
		got = append(got, c.Path)
	}
	if diff := cmp.Diff([]string{"new.go"}, got); diff != "" {
		t.Fatalf("FS.Changes() wrong paths (-want +got) diff = %s", diff)
	}
}

// serveMasked serves tree with masks like a workspace's over a Unix
// socket, as the agent reaches it, and returns its client.
func serveMasked(t *testing.T, tree fsys.FS) fsys.FS {
	t.Helper()
	dir, err := os.MkdirTemp("", "fsysmask")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("removing the socket directory error = %v", err)
		}
	})
	ln, err := net.Listen("unix", filepath.Join(dir, "fs.sock"))
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	srv := httptest.NewUnstartedServer(fsys.Handler(fsysmask.New(tree, fsysmask.Rules{
		Files:   map[string][]byte{".mcp.json": []byte("{}")},
		Private: []string{".opencode"},
		Hidden:  func(name string) bool { return name == ".a2a" },
	})))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	c, err := fsys.Client("unix://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("fsys.Client() error = %v", err)
	}
	return c
}

func TestMaskRefusesSymlinksIntoMasks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root := t.TempDir()
	c := serveMasked(t, fsys.Dir(root))
	ctx := context.Background()
	for _, tc := range []struct{ target, link string }{
		{".a2a", "h"},
		{".a2a/state", "h"},
		{"sub/.a2a", "h"},
		{"../.a2a", "sub/h"},
		{".mcp.json", "m"},
		{".opencode", "o"},
		{"../.opencode/x", "sub/o"},
	} {
		if err := c.Symlink(ctx, tc.target, tc.link); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("Client.Symlink(%q, %q) error = %v, want %v", tc.target, tc.link, err, fs.ErrPermission)
		}
	}
	for _, tc := range []struct{ target, link string }{{"main.go", "ok"}, {"../main.go", "sub/ok"}, {"/etc/passwd", "abs"}, {"..", "up"}} {
		if err := c.Symlink(ctx, tc.target, tc.link); err != nil {
			t.Fatalf("Client.Symlink(%q, %q) error = %v, want nil", tc.target, tc.link, err)
		}
	}
}

func TestMaskRefusesPathsThroughSymlinks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root := t.TempDir()
	for p, data := range map[string]string{".a2a/secret": "secret", ".opencode/agent.md": "host's"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"h": ".a2a", "o": ".opencode"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	c := serveMasked(t, fsys.Dir(root))
	ctx := context.Background()
	tests := []struct {
		name string
		op   func() error
	}{
		{name: "read", op: func() error { _, err := c.Open(ctx, "h/secret"); return err }},
		{name: "stat", op: func() error { _, err := c.Stat(ctx, "h/secret"); return err }},
		{name: "list", op: func() error { _, err := c.ReadDir(ctx, "h"); return err }},
		{name: "write", op: func() error { _, err := c.Write(ctx, "h/evil", strings.NewReader("x"), 0o644, ""); return err }},
		{name: "write in a private directory", op: func() error { _, err := c.Write(ctx, "o/agent.md", strings.NewReader("x"), 0o644, ""); return err }},
		{name: "remove", op: func() error { return c.Remove(ctx, "h/secret") }},
		{name: "rename out", op: func() error { return c.Rename(ctx, "h/secret", "stolen") }},
	}
	for _, tc := range tests {
		if err := tc.op(); err == nil {
			t.Fatalf("%s through a symlink error = nil, want one", tc.name)
		}
	}
	for p, want := range map[string]string{".a2a/secret": "secret", ".opencode/agent.md": "host's"} {
		got, err := os.ReadFile(filepath.Join(root, p))
		if err != nil || string(got) != want {
			t.Fatalf("the host's %s = %q, %v, want it unchanged", p, got, err)
		}
	}
	for _, p := range []string{".a2a/evil", "stolen"} {
		if _, err := os.Lstat(filepath.Join(root, p)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the host's %s error = %v, want %v", p, err, fs.ErrNotExist)
		}
	}
}
