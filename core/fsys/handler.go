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

package fsys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Handler serves f over HTTP with the wire protocol that [Client] speaks,
// which docs/reference/workspace-protocol.md describes. It refuses paths
// outside the rules of the package and bounds what it reads: a written
// file to [MaxFileSize], a listing and a batch of changes to [MaxEntries],
// and a request for changes to [MaxChangesWait].
func Handler(f FS) http.Handler {
	h := &handler{fs: f, slots: make(chan struct{}, maxInFlight)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stat", h.stat)
	mux.HandleFunc("GET /list", h.list)
	mux.HandleFunc("GET /file", h.read)
	mux.HandleFunc("PUT /file", h.write)
	mux.HandleFunc("DELETE /file", h.remove)
	mux.HandleFunc("POST /mkdir", h.mkdir)
	mux.HandleFunc("POST /symlink", h.symlink)
	mux.HandleFunc("POST /rename", h.rename)
	mux.HandleFunc("GET /changes", h.changes)
	h.mux = mux
	return h
}

type handler struct {
	fs    FS
	mux   *http.ServeMux
	slots chan struct{}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case h.slots <- struct{}{}:
	default:
		fail(w, "serve", "", fmt.Errorf("%w: more than %d requests at once", errBusy, maxInFlight))
		return
	}
	defer func() { <-h.slots }()
	h.mux.ServeHTTP(w, r)
}

// pathParam returns the path in the query of r, or fails the request.
func pathParam(w http.ResponseWriter, r *http.Request, op string) (string, bool) {
	p := r.URL.Query().Get("path")
	if err := checkWirePath(op, p); err != nil {
		fail(w, op, p, err)
		return "", false
	}
	return p, true
}

func checkWirePath(op, p string) error {
	if len(p) > maxPath {
		return &fs.PathError{Op: op, Path: p[:64] + "...", Err: fmt.Errorf("%w: longer than %d bytes", fs.ErrInvalid, maxPath)}
	}
	return checkPath(op, p)
}

func (h *handler) stat(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r, "stat")
	if !ok {
		return
	}
	info, err := h.fs.Stat(r.Context(), p)
	if err != nil {
		fail(w, "stat", p, err)
		return
	}
	reply(w, http.StatusOK, toWire(info))
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r, "readdir")
	if !ok {
		return
	}
	infos, err := h.fs.ReadDir(r.Context(), p)
	if err != nil {
		fail(w, "readdir", p, err)
		return
	}
	if len(infos) > MaxEntries {
		fail(w, "readdir", p, fmt.Errorf("%w: more than %d entries", errTooLarge, MaxEntries))
		return
	}
	out := wireList{Entries: make([]wireInfo, len(infos))}
	for i, info := range infos {
		out.Entries[i] = toWire(info)
	}
	reply(w, http.StatusOK, out)
}

func (h *handler) read(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r, "open")
	if !ok {
		return
	}
	info, err := h.fs.Stat(r.Context(), p)
	if err != nil {
		fail(w, "open", p, err)
		return
	}
	if !info.Mode.IsRegular() {
		fail(w, "open", p, fmt.Errorf("%w: not a regular file", fs.ErrInvalid))
		return
	}
	offset, length, partial, err := parseRange(r.Header.Get("Range"), info.Size)
	if err != nil {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
		fail(w, "open", p, err)
		return
	}
	rc, err := OpenRange(r.Context(), h.fs, p, offset, length)
	if err != nil {
		fail(w, "open", p, err)
		return
	}
	defer func() {
		if err := rc.Close(); err != nil {
			slog.Debug("closing a file the file server read", "path", p, "error", err)
		}
	}()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	if info.Version != "" {
		w.Header().Set("ETag", quote(info.Version))
	}
	status := http.StatusOK
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, info.Size))
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	if _, err := io.Copy(w, rc); err != nil {
		slog.Debug("sending a file of the file server", "path", p, "error", err)
	}
}

// errRange is the error of a range past the end of a file.
var errRange = errors.New("the range starts past the end of the file")

// parseRange parses the Range header of a request for a file of size
// bytes: one range, "bytes=first-" or "bytes=first-last". Without one it
// returns the whole file and partial false.
func parseRange(header string, size int64) (offset, length int64, partial bool, err error) {
	if header == "" {
		return 0, -1, false, nil
	}
	spec, ok := strings.CutPrefix(header, "bytes=")
	first, last, dash := strings.Cut(spec, "-")
	if !ok || !dash || strings.Contains(spec, ",") {
		return 0, 0, false, fmt.Errorf("%w: unsupported range %q", fs.ErrInvalid, header)
	}
	offset, err = strconv.ParseInt(first, 10, 64)
	if err != nil || offset < 0 {
		return 0, 0, false, fmt.Errorf("%w: unsupported range %q", fs.ErrInvalid, header)
	}
	if offset >= size {
		return 0, 0, false, errRange
	}
	end := size - 1
	if last != "" {
		if end, err = strconv.ParseInt(last, 10, 64); err != nil || end < offset {
			return 0, 0, false, fmt.Errorf("%w: unsupported range %q", fs.ErrInvalid, header)
		}
		end = min(end, size-1)
	}
	return offset, end - offset + 1, true, nil
}

func (h *handler) write(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r, "write")
	if !ok {
		return
	}
	mode, err := strconv.ParseUint(r.URL.Query().Get("mode"), 8, 32)
	if err != nil {
		fail(w, "write", p, fmt.Errorf("%w: want the mode in octal, such as 0644", fs.ErrInvalid))
		return
	}
	ifVersion := ""
	switch match, none := r.Header.Get("If-Match"), r.Header.Get("If-None-Match"); {
	case match != "" && none != "":
		fail(w, "write", p, fmt.Errorf("%w: If-Match and If-None-Match together", fs.ErrInvalid))
		return
	case none == "*":
		ifVersion = IfMissing
	case none != "":
		fail(w, "write", p, fmt.Errorf("%w: If-None-Match takes only *", fs.ErrInvalid))
		return
	case match != "":
		if ifVersion, err = unquote(match); err != nil {
			fail(w, "write", p, err)
			return
		}
	}
	if r.ContentLength > MaxFileSize {
		fail(w, "write", p, fmt.Errorf("%w: more than %d bytes", errTooLarge, MaxFileSize))
		return
	}
	body := &cappedReader{r: r.Body, left: MaxFileSize}
	info, err := h.fs.Write(r.Context(), p, body, fs.FileMode(mode)&fs.ModePerm, ifVersion)
	if body.over {
		err = fmt.Errorf("%w: more than %d bytes", errTooLarge, MaxFileSize)
	}
	if err != nil {
		fail(w, "write", p, err)
		return
	}
	if info.Version != "" {
		w.Header().Set("ETag", quote(info.Version))
	}
	reply(w, http.StatusOK, toWire(info))
}

// cappedReader reads r and fails past left bytes, which a tree's Write
// then doesn't apply.
type cappedReader struct {
	r    io.Reader
	left int64
	over bool
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	if int64(n) > c.left {
		c.over = true
		return 0, errTooLarge
	}
	c.left -= int64(n)
	return n, err
}

func (h *handler) remove(w http.ResponseWriter, r *http.Request) {
	p, ok := pathParam(w, r, "remove")
	if !ok {
		return
	}
	if err := h.fs.Remove(r.Context(), p); err != nil {
		fail(w, "remove", p, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) mkdir(w http.ResponseWriter, r *http.Request) {
	var req wireMkdir
	if !decodeRequest(w, r, "mkdir", &req) {
		return
	}
	if err := checkWirePath("mkdir", req.Path); err != nil {
		fail(w, "mkdir", req.Path, err)
		return
	}
	if err := h.fs.Mkdir(r.Context(), req.Path, fs.FileMode(req.Mode)&fs.ModePerm); err != nil {
		fail(w, "mkdir", req.Path, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) symlink(w http.ResponseWriter, r *http.Request) {
	var req wireSymlink
	if !decodeRequest(w, r, "symlink", &req) {
		return
	}
	if err := checkWirePath("symlink", req.Path); err != nil {
		fail(w, "symlink", req.Path, err)
		return
	}
	if err := checkTarget("symlink", req.Path, req.Target); err != nil {
		fail(w, "symlink", req.Path, err)
		return
	}
	if err := h.fs.Symlink(r.Context(), req.Target, req.Path); err != nil {
		fail(w, "symlink", req.Path, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) rename(w http.ResponseWriter, r *http.Request) {
	var req wireRename
	if !decodeRequest(w, r, "rename", &req) {
		return
	}
	for _, p := range []string{req.From, req.To} {
		if err := checkWirePath("rename", p); err != nil {
			fail(w, "rename", p, err)
			return
		}
	}
	if err := h.fs.Rename(r.Context(), req.From, req.To); err != nil {
		fail(w, "rename", req.From, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) changes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cursor := q.Get("cursor")
	if len(cursor) > maxPath {
		fail(w, "changes", "", fmt.Errorf("%w: cursor longer than %d bytes", fs.ErrInvalid, maxPath))
		return
	}
	wait := time.Duration(0)
	if v := q.Get("wait"); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ms < 0 {
			fail(w, "changes", "", fmt.Errorf("%w: want wait in milliseconds", fs.ErrInvalid))
			return
		}
		wait = min(time.Duration(ms)*time.Millisecond, MaxChangesWait)
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	changes, next, err := h.fs.Changes(ctx, cursor)
	if errors.Is(err, ErrResync) {
		reply(w, http.StatusGone, wireError{Error: err.Error(), Code: codeResync, Cursor: next})
		return
	}
	if err != nil {
		fail(w, "changes", "", err)
		return
	}
	if len(changes) > MaxEntries {
		reply(w, http.StatusGone, wireError{Error: fmt.Sprintf("more than %d changes; read the tree again", MaxEntries), Code: codeResync, Cursor: next})
		return
	}
	out := wireChanges{Changes: make([]wireInfo, len(changes)), Cursor: next}
	for i, c := range changes {
		out.Changes[i] = toWire(c)
	}
	reply(w, http.StatusOK, out)
}

// decodeRequest decodes the JSON body of r into v, or fails the request.
func decodeRequest(w http.ResponseWriter, r *http.Request, op string, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestJSON))
	if err := dec.Decode(v); err != nil {
		fail(w, op, "", fmt.Errorf("%w: the request body: %v", fs.ErrInvalid, err))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("writing an answer of the file server", "error", err)
	}
}

// fail answers a request of op on path that failed with err. A range past
// the end of a file gets 416.
func fail(w http.ResponseWriter, op, path string, err error) {
	if errors.Is(err, errRange) {
		reply(w, http.StatusRequestedRangeNotSatisfiable, wireError{Error: err.Error()})
		return
	}
	code, status := classify(err)
	if status == http.StatusInternalServerError {
		slog.Debug("a request of the file server failed", "op", op, "path", path, "error", err)
	}
	reply(w, status, wireError{Error: err.Error(), Code: code})
}

func quote(version string) string { return `"` + version + `"` }

func unquote(etag string) (string, error) {
	v, ok := strings.CutPrefix(etag, `"`)
	if v, ok2 := strings.CutSuffix(v, `"`); ok && ok2 && !strings.ContainsAny(v, `"`) {
		return v, nil
	}
	return "", fmt.Errorf("%w: want If-Match with a quoted version", fs.ErrInvalid)
}
