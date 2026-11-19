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

package fsys_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/core/fsys"
)

// serveHTTP serves f on a loopback port and returns its client.
func serveHTTP(t *testing.T, f fsys.FS) fsys.FS {
	t.Helper()
	srv := httptest.NewServer(fsys.Handler(f))
	t.Cleanup(srv.Close)
	c, err := fsys.Client(srv.URL)
	if err != nil {
		t.Fatalf("fsys.Client(%q) error = %v", srv.URL, err)
	}
	return c
}

// serveUnix serves f on a Unix socket and returns its client.
func serveUnix(t *testing.T, f fsys.FS) fsys.FS {
	t.Helper()
	dir, err := os.MkdirTemp("", "fsys")
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
	srv := httptest.NewUnstartedServer(fsys.Handler(f))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	c, err := fsys.Client("unix://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("fsys.Client() error = %v", err)
	}
	return c
}

func TestClientRefusesURL(t *testing.T) {
	t.Parallel()
	for _, u := range []string{"ftp://host", "http://", "unix://host/sock", "unix://", "http://user@host", "http://host?x=1", "::"} {
		if _, err := fsys.Client(u); err == nil {
			t.Fatalf("fsys.Client(%q) error = nil, want one", u)
		}
	}
}

func TestClientOpenRange(t *testing.T) {
	t.Parallel()
	mem := fsys.Memory()
	write(t, mem, "f", "0123456789", 0o644)
	c := serveHTTP(t, mem)
	tests := []struct {
		offset, length int64
		want           string
	}{
		{0, -1, "0123456789"},
		{3, -1, "3456789"},
		{3, 4, "3456"},
		{8, 10, "89"},
		{10, -1, ""},
		{20, 5, ""},
		{0, 0, ""},
	}
	for _, tc := range tests {
		for name, f := range map[string]fsys.FS{"client": c, "memory": mem} {
			rc, err := fsys.OpenRange(context.Background(), f, "f", tc.offset, tc.length)
			if err != nil {
				t.Fatalf("fsys.OpenRange(%s, %d, %d) error = %v", name, tc.offset, tc.length, err)
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("reading the range error = %v", err)
			}
			if err := rc.Close(); err != nil {
				t.Fatalf("closing the range error = %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("fsys.OpenRange(%s, %d, %d) = %q, want %q", name, tc.offset, tc.length, got, tc.want)
			}
		}
	}
}

func TestClientMapsErrors(t *testing.T) {
	t.Parallel()
	mem := fsys.Memory()
	write(t, mem, "file", "x", 0o644)
	write(t, mem, "dir/f", "x", 0o644)
	c := serveUnix(t, mem)
	ctx := context.Background()
	tests := []struct {
		name string
		op   func() error
		want error
	}{
		{name: "missing", op: func() error { _, err := c.Stat(ctx, "missing"); return err }, want: fs.ErrNotExist},
		{name: "write over a directory", op: func() error { _, err := c.Write(ctx, "dir", strings.NewReader("x"), 0o644, ""); return err }, want: syscall.EISDIR},
		{name: "list a file", op: func() error { _, err := c.ReadDir(ctx, "file"); return err }, want: syscall.ENOTDIR},
		{name: "symlink over a file", op: func() error { return c.Symlink(ctx, "x", "file") }, want: fs.ErrExist},
		{name: "stale version", op: func() error { _, err := c.Write(ctx, "file", strings.NewReader("y"), 0o644, "stale"); return err }, want: fsys.ErrConflict},
		{name: "resync", op: func() error { _, _, err := c.Changes(ctx, "old.1"); return err }, want: fsys.ErrResync},
	}
	for _, tc := range tests {
		if err := tc.op(); !errors.Is(err, tc.want) {
			t.Fatalf("%s: error = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// do sends a raw request to h and returns its status.
func do(t *testing.T, h http.Handler, method, target string, header http.Header, body io.Reader) int {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	maps.Copy(req.Header, header)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestHandlerRefusesBadRequests(t *testing.T) {
	t.Parallel()
	mem := fsys.Memory()
	write(t, mem, "f", "hello", 0o644)
	h := fsys.Handler(mem)
	tests := []struct {
		name   string
		method string
		target string
		header http.Header
		body   string
		want   int
	}{
		{name: "parent path", method: "GET", target: "/stat?path=../etc", want: 400},
		{name: "absolute path", method: "GET", target: "/stat?path=/etc", want: 400},
		{name: "NUL in a path", method: "GET", target: "/stat?path=a%00b", want: 400},
		{name: "long path", method: "GET", target: "/stat?path=" + strings.Repeat("a", 5000), want: 400},
		{name: "rename out of the tree", method: "POST", target: "/rename", body: `{"from":"f","to":"../x"}`, want: 400},
		{name: "write without a mode", method: "PUT", target: "/file?path=g", body: "x", want: 400},
		{name: "write over the limit", method: "PUT", target: "/file?path=g&mode=0644", header: http.Header{"Content-Length": {strconv.Itoa(fsys.MaxFileSize + 1)}}, want: 413},
		{name: "unquoted If-Match", method: "PUT", target: "/file?path=f&mode=0644", header: http.Header{"If-Match": {"v"}}, body: "x", want: 400},
		{name: "range past the end", method: "GET", target: "/file?path=f", header: http.Header{"Range": {"bytes=9-"}}, want: 416},
		{name: "several ranges", method: "GET", target: "/file?path=f", header: http.Header{"Range": {"bytes=0-1,3-4"}}, want: 400},
		{name: "large request body", method: "POST", target: "/mkdir", body: `{"path":"` + strings.Repeat("a", 70<<10) + `"}`, want: 400},
		{name: "negative wait", method: "GET", target: "/changes?wait=-1", want: 400},
		{name: "unknown route", method: "GET", target: "/other", want: 404},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
		maps.Copy(req.Header, tc.header)
		if cl := tc.header.Get("Content-Length"); cl != "" {
			n, err := strconv.ParseInt(cl, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			req.ContentLength = n
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s: %s %s status = %d, want %d: %s", tc.name, tc.method, tc.target, rec.Code, tc.want, rec.Body)
		}
	}
	if got := read(t, mem, "f"); got != "hello" {
		t.Fatalf("FS.Open(f) after the refused requests = %q, want %q", got, "hello")
	}
}

func TestHandlerServesRangeAndVersion(t *testing.T) {
	t.Parallel()
	mem := fsys.Memory()
	info := write(t, mem, "f", "hello world", 0o644)
	h := fsys.Handler(mem)
	req := httptest.NewRequest("GET", "/file?path=f", nil)
	req.Header.Set("Range", "bytes=6-")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	got := struct{ Status, Body, Range, ETag string }{strconv.Itoa(rec.Code), rec.Body.String(), rec.Header().Get("Content-Range"), rec.Header().Get("ETag")}
	want := struct{ Status, Body, Range, ETag string }{"206", "world", "bytes 6-10/11", `"` + info.Version + `"`}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("GET /file with a range wrong result (-want +got) diff = %s", diff)
	}
	if code := do(t, h, "PUT", "/file?path=f&mode=0644", http.Header{"If-Match": {`"` + info.Version + `"`}}, strings.NewReader("new")); code != 200 {
		t.Fatalf("PUT /file with the current version status = %d, want 200", code)
	}
	if code := do(t, h, "PUT", "/file?path=f&mode=0644", http.Header{"If-Match": {`"` + info.Version + `"`}}, strings.NewReader("stale")); code != 412 {
		t.Fatalf("PUT /file with an old version status = %d, want 412", code)
	}
	if code := do(t, h, "PUT", "/file?path=f&mode=0644", http.Header{"If-None-Match": {"*"}}, strings.NewReader("x")); code != 412 {
		t.Fatalf("PUT /file with If-None-Match over a file status = %d, want 412", code)
	}
	if got := read(t, mem, "f"); got != "new" {
		t.Fatalf("FS.Open(f) = %q, want %q", got, "new")
	}
}

func TestClientRefusesEntriesOutsideTheDirectory(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"entries":[{"path":"../escape","type":"file"}]}`); err != nil {
			t.Errorf("writing the answer error = %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := fsys.Client(srv.URL)
	if err != nil {
		t.Fatalf("fsys.Client() error = %v", err)
	}
	if _, err := c.ReadDir(context.Background(), "."); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("Client.ReadDir() of an entry out of the tree error = %v, want %v", err, fs.ErrInvalid)
	}
}

// closeRecorder is a reader that records whether it was closed.
type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error {
	c.closed = true
	return nil
}

func TestClientWriteLeavesTheReaderToTheCaller(t *testing.T) {
	t.Parallel()
	c := serveHTTP(t, fsys.Memory())
	r := &closeRecorder{Reader: strings.NewReader("data")}

	if _, err := c.Write(context.Background(), "f", r, 0o644, ""); err != nil {
		t.Fatalf("Client.Write() error = %v", err)
	}

	if r.closed {
		t.Fatalf("Client.Write() closed the reader, want it left to the caller")
	}
}

func TestClientSpeaksTLSForHTTPS(t *testing.T) {
	t.Parallel()
	srv := httptest.NewUnstartedServer(fsys.Handler(fsys.Memory()))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	c, err := fsys.Client(srv.URL)
	if err != nil {
		t.Fatalf("fsys.Client(%q) error = %v", srv.URL, err)
	}

	_, err = c.Stat(context.Background(), ".")

	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("Client.Stat() of a server with an untrusted certificate error = %v, want a certificate error", err)
	}
}
