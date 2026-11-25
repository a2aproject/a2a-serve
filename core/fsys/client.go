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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// ClientOptions configures [ClientWithOptions].
type ClientOptions struct {
	// Header holds extra headers sent on every request to the file server,
	// such as an authentication token.
	Header http.Header
	// RequestHeader, if set, returns more headers for the request of ctx,
	// such as who the request is for. A request whose headers a header
	// can't carry fails without being sent.
	RequestHeader func(ctx context.Context) http.Header
}

// Client returns the tree that a file server serves at rawURL with the
// wire protocol of [Handler]: http://host:port or https://host:port with
// an optional path prefix, or unix:///path/to/socket. It refuses paths outside the rules of
// the package before it sends them, and bounds what it reads: a listing
// and a batch of changes to [MaxEntries]. Changes waits at most until the
// deadline of its context, in requests of up to [MaxChangesWait].
func Client(rawURL string) (FS, error) {
	return ClientWithOptions(rawURL, ClientOptions{})
}

// ClientWithOptions is [Client] configured with opts.
func ClientWithOptions(rawURL string, opts ClientOptions) (FS, error) {
	u, err := url.Parse(rawURL)
	if ue, ok := errors.AsType[*url.Error](err); ok {
		// The error of url.Parse quotes the URL, whose path may carry a
		// secret.
		err = ue.Err
	}
	if err != nil {
		return nil, fmt.Errorf("parsing the file server URL: %w", err)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("the file server URL can't have user info, a query or a fragment")
	}
	for name, vals := range opts.Header {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n") {
			return nil, errors.New("the file server client header name is invalid")
		}
		for _, val := range vals {
			if strings.ContainsAny(val, "\r\n") {
				return nil, errors.New("the file server client header value has a line break")
			}
		}
	}
	transport := &http.Transport{
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       time.Minute,
		ResponseHeaderTimeout: MaxChangesWait + time.Minute,
	}
	c := &client{http: &http.Client{Transport: transport, CheckRedirect: noRedirects}, header: opts.Header.Clone(), requestHeader: opts.RequestHeader}
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return nil, errors.New("the file server URL has no host")
		}
		c.base = strings.TrimSuffix(u.String(), "/")
		c.shown = u.Scheme + "://" + u.Host
	case "unix":
		if u.Host != "" || u.Path == "" {
			return nil, errors.New("want the file server socket as unix:///path/to/socket")
		}
		socket := u.Path
		var d net.Dialer
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", socket)
		}
		c.base = "http://fsys"
		c.shown = "unix://" + socket
	default:
		return nil, fmt.Errorf("the file server URL has the scheme %q; want http, https or unix", u.Scheme)
	}
	return c, nil
}

// pastDeadlineTimeout bounds the request of Changes whose deadline passed.
const pastDeadlineTimeout = 10 * time.Second

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

type client struct {
	base string
	// shown names the server in errors: its scheme and host, or its
	// socket, without the path, which may carry a secret.
	shown         string
	header        http.Header
	requestHeader func(context.Context) http.Header
	http          *http.Client
}

// requestError is the error of a request that got no answer. Unlike the
// *url.Error of net/http, it leaves out the URL of the request.
type requestError struct {
	method, shown string
	err           error
}

func (e *requestError) Error() string { return e.method + " " + e.shown + ": " + e.err.Error() }
func (e *requestError) Unwrap() error { return e.err }

// Timeout reports whether the request timed out, as net.Error does.
func (e *requestError) Timeout() bool {
	t, ok := e.err.(interface{ Timeout() bool })
	return ok && t.Timeout()
}

// requestErr returns err of a request without the URL of the request.
func (c *client) requestErr(method string, err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		err = ue.Err
	}
	return &requestError{method: method, shown: c.shown, err: err}
}

var (
	_ FS          = (*client)(nil)
	_ RangeOpener = (*client)(nil)
)

// do sends a request of op on p and returns its response when its status
// is ok, and otherwise the error it reports.
func (c *client) do(ctx context.Context, op, p, method, route string, query url.Values, header http.Header, body io.Reader, ok ...int) (*http.Response, error) {
	u := c.base + route
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, &fs.PathError{Op: op, Path: p, Err: c.requestErr(method, err)}
	}
	for k, vals := range c.header {
		req.Header[http.CanonicalHeaderKey(k)] = slices.Clone(vals)
	}
	if c.requestHeader != nil {
		for k, vals := range c.requestHeader(ctx) {
			if !httpguts.ValidHeaderFieldName(k) || slices.ContainsFunc(vals, func(v string) bool { return !httpguts.ValidHeaderFieldValue(v) }) {
				return nil, &fs.PathError{Op: op, Path: p, Err: c.requestErr(method, errors.New("a request header has a character that a header can't carry"))}
			}
			req.Header[http.CanonicalHeaderKey(k)] = slices.Clone(vals)
		}
	}
	maps.Copy(req.Header, header)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &fs.PathError{Op: op, Path: p, Err: c.requestErr(method, err)}
	}
	if slices.Contains(ok, resp.StatusCode) {
		return resp, nil
	}
	defer closeBody(resp)
	var w wireError
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxInfoJSON)).Decode(&w); err != nil {
		w = wireError{}
	}
	return nil, errorOf(op, p, resp.StatusCode, w)
}

// decode decodes the JSON body of resp, at most limit bytes, into v.
func decode(op, p string, resp *http.Response, limit int64, v any) error {
	defer closeBody(resp)
	body := &cappedReader{r: resp.Body, left: limit}
	if err := json.NewDecoder(body).Decode(v); err != nil {
		if body.over {
			err = fmt.Errorf("%w: an answer of more than %d bytes", errTooLarge, limit)
		}
		return &fs.PathError{Op: op, Path: p, Err: fmt.Errorf("decoding the answer of the file server: %w", err)}
	}
	return nil
}

// closeBody drains a little of the body of resp, so that its connection
// can be reused, and closes it.
func closeBody(resp *http.Response) {
	if _, err := io.CopyN(io.Discard, resp.Body, 4<<10); err != nil && !errors.Is(err, io.EOF) {
		slog.Debug("draining an answer of the file server", "error", err)
	}
	if err := resp.Body.Close(); err != nil {
		slog.Debug("closing an answer of the file server", "error", err)
	}
}

func pathQuery(p string) url.Values { return url.Values{"path": {p}} }

func (c *client) Stat(ctx context.Context, p string) (Info, error) {
	if err := checkWirePath("stat", p); err != nil {
		return Info{}, err
	}
	resp, err := c.do(ctx, "stat", p, http.MethodGet, "/stat", pathQuery(p), nil, nil, http.StatusOK)
	if err != nil {
		return Info{}, err
	}
	var w wireInfo
	if err := decode("stat", p, resp, maxInfoJSON, &w); err != nil {
		return Info{}, err
	}
	info, err := fromWire(w)
	if err != nil {
		return Info{}, err
	}
	if info.Path != p {
		return Info{}, &fs.PathError{Op: "stat", Path: p, Err: fmt.Errorf("the file server described %q", info.Path)}
	}
	return info, nil
}

func (c *client) ReadDir(ctx context.Context, p string) ([]Info, error) {
	if err := checkWirePath("readdir", p); err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, "readdir", p, http.MethodGet, "/list", pathQuery(p), nil, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var w wireList
	if err := decode("readdir", p, resp, maxListJSON, &w); err != nil {
		return nil, err
	}
	if len(w.Entries) > MaxEntries {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: fmt.Errorf("%w: more than %d entries", errTooLarge, MaxEntries)}
	}
	out := make([]Info, len(w.Entries))
	for i, e := range w.Entries {
		info, err := fromWire(e)
		if err != nil {
			return nil, err
		}
		if info.Path == "." || path.Dir(info.Path) != p || info.Removed {
			return nil, &fs.PathError{Op: "readdir", Path: p, Err: fmt.Errorf("the file server listed %q, which isn't an entry of the directory", info.Path)}
		}
		out[i] = info
	}
	return out, nil
}

func (c *client) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	return c.OpenRange(ctx, p, 0, -1)
}

// OpenRange implements [RangeOpener] with an HTTP range request.
func (c *client) OpenRange(ctx context.Context, p string, offset, length int64) (io.ReadCloser, error) {
	if err := checkWirePath("open", p); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fmt.Errorf("%w: negative offset", fs.ErrInvalid)}
	}
	if length == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	header := http.Header{}
	if offset > 0 || length > 0 {
		r := "bytes=" + strconv.FormatInt(offset, 10) + "-"
		if length > 0 {
			r += strconv.FormatInt(offset+length-1, 10)
		}
		header.Set("Range", r)
	}
	resp, err := c.do(ctx, "open", p, http.MethodGet, "/file", pathQuery(p), header, nil, http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		closeBody(resp)
		return io.NopCloser(bytes.NewReader(nil)), nil
	case resp.StatusCode == http.StatusOK && offset > 0:
		// A server that ignored the range sends the whole file.
		rc, err := skipRange(resp.Body, offset, length)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: p, Err: err}
		}
		return rc, nil
	case length > 0:
		return limitedReadCloser{Reader: io.LimitReader(resp.Body, length), Closer: resp.Body}, nil
	}
	return resp.Body, nil
}

func skipRange(rc io.ReadCloser, offset, length int64) (io.ReadCloser, error) {
	if _, err := io.CopyN(io.Discard, rc, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.Join(err, rc.Close())
	}
	if length < 0 {
		return rc, nil
	}
	return limitedReadCloser{Reader: io.LimitReader(rc, length), Closer: rc}, nil
}

func (c *client) Write(ctx context.Context, p string, r io.Reader, mode fs.FileMode, ifVersion string) (Info, error) {
	if err := checkEntry("write", p); err != nil {
		return Info{}, err
	}
	if err := checkWirePath("write", p); err != nil {
		return Info{}, err
	}
	header := http.Header{}
	switch {
	case ifVersion == IfMissing:
		header.Set("If-None-Match", "*")
	case ifVersion != "":
		if strings.ContainsAny(ifVersion, "\"\r\n\x00") {
			return Info{}, &fs.PathError{Op: "write", Path: p, Err: fmt.Errorf("%w: version %q", fs.ErrInvalid, ifVersion)}
		}
		header.Set("If-Match", quote(ifVersion))
	}
	q := pathQuery(p)
	q.Set("mode", fmt.Sprintf("%04o", uint32(mode.Perm())))
	// The transport closes a body that is an io.Closer, and r is the
	// caller's.
	resp, err := c.do(ctx, "write", p, http.MethodPut, "/file", q, header, struct{ io.Reader }{r}, http.StatusOK)
	if err != nil {
		return Info{}, err
	}
	var w wireInfo
	if err := decode("write", p, resp, maxInfoJSON, &w); err != nil {
		return Info{}, err
	}
	return fromWire(w)
}

func (c *client) post(ctx context.Context, op, p, route string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return &fs.PathError{Op: op, Path: p, Err: err}
	}
	resp, err := c.do(ctx, op, p, http.MethodPost, route, nil, http.Header{"Content-Type": {"application/json"}}, bytes.NewReader(body), http.StatusNoContent, http.StatusOK)
	if err != nil {
		return err
	}
	closeBody(resp)
	return nil
}

func (c *client) Mkdir(ctx context.Context, p string, mode fs.FileMode) error {
	if err := checkWirePath("mkdir", p); err != nil {
		return err
	}
	return c.post(ctx, "mkdir", p, "/mkdir", wireMkdir{Path: p, Mode: uint32(mode.Perm())})
}

func (c *client) Symlink(ctx context.Context, target, p string) error {
	if err := checkEntry("symlink", p); err != nil {
		return err
	}
	if err := checkWirePath("symlink", p); err != nil {
		return err
	}
	if err := checkTarget("symlink", p, target); err != nil {
		return err
	}
	return c.post(ctx, "symlink", p, "/symlink", wireSymlink{Target: target, Path: p})
}

func (c *client) Rename(ctx context.Context, from, to string) error {
	for _, p := range []string{from, to} {
		if err := checkEntry("rename", p); err != nil {
			return err
		}
		if err := checkWirePath("rename", p); err != nil {
			return err
		}
	}
	return c.post(ctx, "rename", from, "/rename", wireRename{From: from, To: to})
}

func (c *client) Remove(ctx context.Context, p string) error {
	if err := checkEntry("remove", p); err != nil {
		return err
	}
	if err := checkWirePath("remove", p); err != nil {
		return err
	}
	resp, err := c.do(ctx, "remove", p, http.MethodDelete, "/file", pathQuery(p), nil, nil, http.StatusNoContent, http.StatusOK)
	if err != nil {
		return err
	}
	closeBody(resp)
	return nil
}

func (c *client) Changes(ctx context.Context, cursor string) ([]Info, string, error) {
	for {
		wait := MaxChangesWait
		if d, ok := ctx.Deadline(); ok {
			wait = min(wait, max(time.Until(d), 0))
		}
		changes, next, err := c.changesOnce(ctx, cursor, wait)
		if err != nil {
			if ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
				return nil, cursor, nil
			}
			return nil, next, err
		}
		if len(changes) > 0 || cursor == "" || next != cursor {
			return changes, next, nil
		}
		if _, ok := ctx.Deadline(); ok && wait < MaxChangesWait {
			return nil, cursor, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
	}
}

// changesOnce asks once for the changes after cursor, which the server
// waits for up to wait.
func (c *client) changesOnce(ctx context.Context, cursor string, wait time.Duration) ([]Info, string, error) {
	if wait == 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// A deadline that passed still returns the changes there are.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), pastDeadlineTimeout)
		defer cancel()
	}
	q := url.Values{"cursor": {cursor}, "wait": {strconv.FormatInt(wait.Milliseconds(), 10)}}
	resp, err := c.do(ctx, "changes", "", http.MethodGet, "/changes", q, nil, nil, http.StatusOK, http.StatusGone)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode == http.StatusGone {
		var w wireError
		if err := decode("changes", "", resp, maxInfoJSON, &w); err != nil {
			return nil, "", err
		}
		return nil, w.Cursor, errorOf("changes", "", resp.StatusCode, w)
	}
	var w wireChanges
	if err := decode("changes", "", resp, maxListJSON, &w); err != nil {
		return nil, "", err
	}
	if len(w.Changes) > MaxEntries {
		return nil, "", &fs.PathError{Op: "changes", Err: fmt.Errorf("%w: more than %d changes", errTooLarge, MaxEntries)}
	}
	out := make([]Info, len(w.Changes))
	for i, e := range w.Changes {
		info, err := fromWire(e)
		if err != nil {
			return nil, "", err
		}
		out[i] = info
	}
	return out, w.Cursor, nil
}
