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

package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	neturl "net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// changesWait is how long the suite asks the file server to wait for a
// change, and changesDelay how long after the request it changes a file.
const (
	changesWait  = 10 * time.Second
	changesDelay = 300 * time.Millisecond
	// idleWait is the wait of a request for changes when nothing changes,
	// which must hold the answer for at least half of it.
	idleWait = time.Second
)

// entry is an entry of the workspace protocol.
type entry struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	Mode    uint32 `json:"mode"`
	Size    int64  `json:"size"`
	Version string `json:"version"`
	Target  string `json:"target"`
	Removed bool   `json:"removed"`
}

// failure is the body of a failed request.
type failure struct {
	Error  string `json:"error"`
	Code   string `json:"code"`
	Cursor string `json:"cursor"`
}

type changes struct {
	Changes []entry `json:"changes"`
	Cursor  string  `json:"cursor"`
}

// fileServer sends requests of the workspace protocol to a host's file
// server.
type fileServer struct {
	base string
	http *http.Client
}

func newFileServer(rawURL string) (*fileServer, error) {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid workspace URL: %w", err)
	}
	transport := &http.Transport{ResponseHeaderTimeout: changesWait + timeout}
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return nil, fmt.Errorf("invalid workspace URL %q: no host", rawURL)
		}
		return &fileServer{base: strings.TrimSuffix(u.String(), "/"), http: &http.Client{Transport: transport}}, nil
	case "unix":
		if u.Host != "" || u.Path == "" {
			return nil, fmt.Errorf("invalid workspace URL %q: want unix:///path", rawURL)
		}
		socket := u.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
		return &fileServer{base: "http://fsys", http: &http.Client{Transport: transport}}, nil
	}
	return nil, fmt.Errorf("invalid workspace URL %q: want http://, https:// or unix:///path", rawURL)
}

// do sends a request and returns its status, headers and body.
func (f *fileServer) do(t *testing.T, method, route string, query neturl.Values, header http.Header, body []byte) (int, http.Header, []byte) {
	t.Helper()
	status, h, data, err := f.send(t.Context(), method, route, query, header, body)
	if err != nil {
		t.Fatal(err)
	}
	return status, h, data
}

// send is do for another goroutine than the test's.
func (f *fileServer) send(ctx context.Context, method, route string, query neturl.Values, header http.Header, body []byte) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, changesWait+timeout)
	defer cancel()
	u := f.base + route
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	maps.Copy(req.Header, header)
	resp, err := f.http.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s error = %w", method, route, err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, nil, nil, fmt.Errorf("reading the answer to %s %s error = %w", method, route, err)
	}
	return resp.StatusCode, resp.Header, data, nil
}

// want sends a request that must answer status, and decodes a JSON answer
// into v unless v is nil.
func (f *fileServer) want(t *testing.T, status int, method, route string, query neturl.Values, header http.Header, body []byte, v any) http.Header {
	t.Helper()
	got, h, data := f.do(t, method, route, query, header, body)
	if got != status {
		t.Fatalf("%s %s?%s status = %d, want %d; body %q", method, route, query.Encode(), got, status, data)
	}
	if v != nil {
		if err := json.Unmarshal(data, v); err != nil {
			t.Fatalf("%s %s?%s answer %q isn't the JSON the protocol specifies: %v", method, route, query.Encode(), data, err)
		}
	}
	return h
}

// wantFailure sends a request that must fail with status and code.
func (f *fileServer) wantFailure(t *testing.T, status int, code, method, route string, query neturl.Values, header http.Header, body []byte) failure {
	t.Helper()
	var fl failure
	f.want(t, status, method, route, query, header, body, &fl)
	if fl.Code != code || fl.Error == "" {
		t.Fatalf("%s %s?%s failure = %+v, want the code %q and a message", method, route, query.Encode(), fl, code)
	}
	return fl
}

func (f *fileServer) stat(t *testing.T, p string) entry {
	t.Helper()
	var e entry
	f.want(t, http.StatusOK, http.MethodGet, "/stat", pathOf(p), nil, nil, &e)
	if e.Path != p {
		t.Fatalf("GET /stat?path=%s described %q, want %q", p, e.Path, p)
	}
	return e
}

func (f *fileServer) put(t *testing.T, p, data string, header http.Header) entry {
	t.Helper()
	var e entry
	f.want(t, http.StatusOK, http.MethodPut, "/file", fileOf(p), header, []byte(data), &e)
	if e.Path != p || e.Type != "file" || e.Size != int64(len(data)) || e.Version == "" {
		t.Fatalf("PUT /file?path=%s = %+v, want the file written with a version", p, e)
	}
	return e
}

func (f *fileServer) post(t *testing.T, route string, body any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	f.want(t, http.StatusNoContent, http.MethodPost, route, nil, http.Header{"Content-Type": {"application/json"}}, data, nil)
}

func (f *fileServer) changes(t *testing.T, cursor string, wait time.Duration) changes {
	t.Helper()
	var c changes
	f.want(t, http.StatusOK, http.MethodGet, "/changes", neturl.Values{"cursor": {cursor}, "wait": {fmt.Sprint(wait.Milliseconds())}}, nil, nil, &c)
	if c.Cursor == "" {
		t.Fatalf("GET /changes?cursor=%s answered no cursor", cursor)
	}
	return c
}

func pathOf(p string) neturl.Values { return neturl.Values{"path": {p}} }

// fileOf is the query of a write of the file p.
func fileOf(p string) neturl.Values { return neturl.Values{"path": {p}, "mode": {"0644"}} }

func quoted(version string) http.Header { return http.Header{"If-Match": {`"` + version + `"`}} }

func testWorkspace(t *testing.T, rawURL string) {
	f, err := newFileServer(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	dir := "a2a-run-conformance-" + hex.EncodeToString(b)
	t.Cleanup(func() {
		status, _, body, err := f.send(context.Background(), http.MethodDelete, "/file", pathOf(dir), nil, nil)
		if err != nil || status != http.StatusNoContent {
			t.Errorf("removing the suite's directory %s = %d, %v, want 204; body %q", dir, status, err, body)
		}
	})
	t.Run("stats the root", func(t *testing.T) {
		if e := f.stat(t, "."); e.Type != "dir" {
			t.Fatalf("GET /stat?path=. = %+v, want a directory", e)
		}
	})
	t.Run("writes and reads files", func(t *testing.T) { testFileReadWrite(t, f, dir+"/rw") })
	t.Run("writes only over the expected version", func(t *testing.T) { testConditionalWrite(t, f, dir+"/cond") })
	t.Run("lists directories", func(t *testing.T) { testList(t, f, dir+"/list") })
	t.Run("creates directories and symlinks", func(t *testing.T) { testMkdirSymlink(t, f, dir+"/links") })
	t.Run("renames and removes", func(t *testing.T) { testRenameRemove(t, f, dir+"/mv") })
	t.Run("refuses invalid paths", func(t *testing.T) {
		for _, p := range []string{"", "/abs", "../x", "a/../b", "./a", "a/", ".."} {
			f.wantFailure(t, http.StatusBadRequest, "invalid", http.MethodGet, "/stat", pathOf(p), nil, nil)
		}
	})
	t.Run("feeds changes", func(t *testing.T) { testChanges(t, f, dir+"/feed") })
	t.Run("waits for changes", func(t *testing.T) {
		if err := checkLongPoll(t.Context(), f); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("asks to resync for an unknown cursor", func(t *testing.T) {
		fl := f.wantFailure(t, http.StatusGone, "resync", http.MethodGet, "/changes", neturl.Values{"cursor": {"a2a-run-conformance-unknown"}}, nil, nil)
		if fl.Cursor == "" {
			t.Fatalf("GET /changes of an unknown cursor = %+v, want the current cursor", fl)
		}
		f.changes(t, fl.Cursor, 0)
	})
}

func testFileReadWrite(t *testing.T, f *fileServer, dir string) {
	p := dir + "/sub/a.txt"
	e := f.put(t, p, "0123456789", nil)
	if parent := f.stat(t, dir+"/sub"); parent.Type != "dir" {
		t.Fatalf("GET /stat of the parent of a written file = %+v, want a directory", parent)
	}
	_, h, body := f.do(t, http.MethodGet, "/file", pathOf(p), nil, nil)
	if string(body) != "0123456789" || h.Get("ETag") != `"`+e.Version+`"` {
		t.Fatalf("GET /file?path=%s = %q with ETag %q, want the contents with ETag %q", p, body, h.Get("ETag"), `"`+e.Version+`"`)
	}
	status, h, body := f.do(t, http.MethodGet, "/file", pathOf(p), http.Header{"Range": {"bytes=2-5"}}, nil)
	if status != http.StatusPartialContent || string(body) != "2345" || h.Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("GET /file with Range: bytes=2-5 = %d %q with Content-Range %q, want 206 \"2345\" with \"bytes 2-5/10\"", status, body, h.Get("Content-Range"))
	}
	status, _, body = f.do(t, http.MethodGet, "/file", pathOf(p), http.Header{"Range": {"bytes=7-"}}, nil)
	if status != http.StatusPartialContent || string(body) != "789" {
		t.Fatalf("GET /file with Range: bytes=7- = %d %q, want 206 \"789\"", status, body)
	}
	if status, _, _ := f.do(t, http.MethodGet, "/file", pathOf(p), http.Header{"Range": {"bytes=20-"}}, nil); status != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("GET /file with a range past the end status = %d, want 416", status)
	}
	f.wantFailure(t, http.StatusNotFound, "not_exist", http.MethodGet, "/stat", pathOf(dir+"/missing"), nil, nil)
	f.wantFailure(t, http.StatusNotFound, "not_exist", http.MethodGet, "/file", pathOf(dir+"/missing"), nil, nil)
}

func testConditionalWrite(t *testing.T, f *fileServer, dir string) {
	p := dir + "/c.txt"
	v1 := f.put(t, p, "one", http.Header{"If-None-Match": {"*"}})
	f.wantFailure(t, http.StatusPreconditionFailed, "conflict", http.MethodPut, "/file", fileOf(p), http.Header{"If-None-Match": {"*"}}, []byte("again"))
	v2 := f.put(t, p, "two", quoted(v1.Version))
	if v2.Version == v1.Version {
		t.Fatalf("PUT /file kept the version %q, want a new one on every write", v1.Version)
	}
	f.wantFailure(t, http.StatusPreconditionFailed, "conflict", http.MethodPut, "/file", fileOf(p), quoted(v1.Version), []byte("stale"))
	if _, _, body := f.do(t, http.MethodGet, "/file", pathOf(p), nil, nil); string(body) != "two" {
		t.Fatalf("GET /file after a refused write = %q, want \"two\" unchanged", body)
	}
	f.put(t, p, "three", nil)
}

func testList(t *testing.T, f *fileServer, dir string) {
	f.put(t, dir+"/b.txt", "b", nil)
	f.put(t, dir+"/a.txt", "a", nil)
	f.post(t, "/mkdir", map[string]any{"path": dir + "/c", "mode": 0o755})
	var l struct {
		Entries []entry `json:"entries"`
	}
	f.want(t, http.StatusOK, http.MethodGet, "/list", pathOf(dir), nil, nil, &l)
	var got []string
	for _, e := range l.Entries {
		got = append(got, e.Path+":"+e.Type)
	}
	if want := []string{dir + "/a.txt:file", dir + "/b.txt:file", dir + "/c:dir"}; !slices.Equal(got, want) {
		t.Fatalf("GET /list?path=%s = %v, want %v", dir, got, want)
	}
	f.wantFailure(t, http.StatusNotFound, "not_exist", http.MethodGet, "/list", pathOf(dir+"/missing"), nil, nil)
}

func testMkdirSymlink(t *testing.T, f *fileServer, dir string) {
	f.post(t, "/mkdir", map[string]any{"path": dir + "/m/n", "mode": 0o755})
	f.post(t, "/mkdir", map[string]any{"path": dir + "/m/n", "mode": 0o755})
	if e := f.stat(t, dir+"/m/n"); e.Type != "dir" || e.Mode != 0o755 {
		t.Fatalf("GET /stat of a new directory = %+v, want a directory with mode 493", e)
	}
	f.put(t, dir+"/target.txt", "t", nil)
	f.post(t, "/symlink", map[string]any{"target": "target.txt", "path": dir + "/link"})
	if e := f.stat(t, dir+"/link"); e.Type != "symlink" || e.Target != "target.txt" {
		t.Fatalf("GET /stat of a symlink = %+v, want a symlink to target.txt", e)
	}
	f.post(t, "/symlink", map[string]any{"target": "m", "path": dir + "/dirlink"})
	f.wantFailure(t, http.StatusNotFound, "not_exist", http.MethodGet, "/stat", pathOf(dir+"/dirlink/n"), nil, nil)
	f.wantFailure(t, http.StatusUnprocessableEntity, "not_dir", http.MethodPut, "/file", fileOf(dir+"/dirlink/x"), nil, []byte("x"))
}

func testRenameRemove(t *testing.T, f *fileServer, dir string) {
	f.put(t, dir+"/from/f.txt", "moved", nil)
	f.post(t, "/rename", map[string]any{"from": dir + "/from", "to": dir + "/to"})
	f.wantFailure(t, http.StatusNotFound, "not_exist", http.MethodGet, "/stat", pathOf(dir+"/from"), nil, nil)
	if _, _, body := f.do(t, http.MethodGet, "/file", pathOf(dir+"/to/f.txt"), nil, nil); string(body) != "moved" {
		t.Fatalf("GET /file of a renamed directory's file = %q, want \"moved\"", body)
	}
	f.want(t, http.StatusNoContent, http.MethodDelete, "/file", pathOf(dir+"/to"), nil, nil, nil)
	f.wantFailure(t, http.StatusNotFound, "not_exist", http.MethodGet, "/stat", pathOf(dir+"/to/f.txt"), nil, nil)
}

func testChanges(t *testing.T, f *fileServer, dir string) {
	start := f.changes(t, "", 0)
	if len(start.Changes) > 0 {
		t.Fatalf("GET /changes without a cursor = %+v, want no changes", start.Changes)
	}
	p := dir + "/f.txt"
	written := make(chan error, 1)
	go func() {
		time.Sleep(changesDelay)
		status, _, body, err := f.send(t.Context(), http.MethodPut, "/file", fileOf(p), nil, []byte("fed"))
		if err != nil {
			written <- err
			return
		}
		if status != http.StatusOK {
			written <- fmt.Errorf("PUT /file?path=%s status = %d; body %q", p, status, body)
			return
		}
		written <- nil
	}()
	began := time.Now()
	cursor, got := start.Cursor, []entry(nil)
	for !slices.ContainsFunc(got, func(e entry) bool { return e.Path == p }) {
		if time.Since(began) > changesWait {
			t.Fatalf("GET /changes didn't report %s within %v", p, changesWait)
		}
		c := f.changes(t, cursor, changesWait)
		cursor, got = c.Cursor, c.Changes
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	f.want(t, http.StatusNoContent, http.MethodDelete, "/file", pathOf(p), nil, nil, nil)
	c := f.changes(t, cursor, changesWait)
	if !slices.ContainsFunc(c.Changes, func(e entry) bool { return e.Path == p && e.Removed }) {
		t.Fatalf("GET /changes after a removal = %+v, want %s removed", c.Changes, p)
	}
}

// checkLongPoll checks that a request for changes with a wait holds its
// answer while nothing changes. Changes that others make to the tree
// answer it early, so it tries a few times.
func checkLongPoll(ctx context.Context, f *fileServer) error {
	cursor := ""
	for range 5 {
		q := neturl.Values{"cursor": {cursor}}
		if cursor != "" {
			q.Set("wait", fmt.Sprint(idleWait.Milliseconds()))
		}
		began := time.Now()
		status, _, body, err := f.send(ctx, http.MethodGet, "/changes", q, nil, nil)
		if err != nil {
			return err
		}
		elapsed := time.Since(began)
		var c changes
		if status != http.StatusOK || json.Unmarshal(body, &c) != nil || c.Cursor == "" {
			return fmt.Errorf("GET /changes?%s = %d %q, want 200 with a cursor", q.Encode(), status, body)
		}
		if cursor != "" && len(c.Changes) == 0 && elapsed < idleWait/2 {
			return fmt.Errorf("GET /changes?%s answered no changes after %v, want it to wait about %v for one", q.Encode(), elapsed, idleWait)
		}
		if cursor != "" && len(c.Changes) == 0 {
			return nil
		}
		cursor = c.Cursor
	}
	return errors.New("GET /changes kept answering with changes; run the suite on a tree that nothing else changes")
}
