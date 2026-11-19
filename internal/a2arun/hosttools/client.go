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

package hosttools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	// ProtocolVersion is the MCP version the client asks for.
	ProtocolVersion = "2025-06-18"
	// HooksCapability is the experimental capability of a host that
	// answers hooks.
	HooksCapability = "a2a-run/hooks"
	// MaxMessage bounds an answer of the endpoint.
	MaxMessage = 1 << 20
	// maxConns bounds the requests to the endpoint at once.
	maxConns = 16
	// maxErrorBody bounds the body of a failed answer in an error.
	maxErrorBody = 256
)

// Client is an MCP client of the endpoint, with one session.
type Client struct {
	ep Endpoint
	hc *http.Client
	// clientVersion is the version of a2a-run, which initialize sends.
	clientVersion string
	slots         chan struct{}
	ids           atomic.Int64

	mu      sync.Mutex
	session string
	version string
	init    initializeResult
}

// HookSupport is what the host advertises of the hooks.
type HookSupport struct {
	BeforeTool bool `json:"beforeTool"`
	AfterTool  bool `json:"afterTool"`
}

type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    struct {
		Experimental map[string]json.RawMessage `json:"experimental"`
	} `json:"capabilities"`
}

// RPCError is the JSON-RPC error of an answer.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message) }

// Dial starts a session with the endpoint ep: it sends initialize and
// notifications/initialized.
func Dial(ctx context.Context, ep Endpoint, version string) (*Client, error) {
	c := &Client{ep: ep, hc: &http.Client{Transport: ep.Transport(), CheckRedirect: noRedirects}, clientVersion: version, slots: make(chan struct{}, maxConns)}
	if err := c.initialize(ctx); err != nil {
		c.hc.CloseIdleConnections()
		return nil, err
	}
	return c, nil
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Hooks returns the hooks the host advertised in its initialize result.
func (c *Client) Hooks() HookSupport {
	c.mu.Lock()
	defer c.mu.Unlock()
	var h HookSupport
	if raw, ok := c.init.Capabilities.Experimental[HooksCapability]; ok {
		if err := json.Unmarshal(raw, &h); err != nil {
			return HookSupport{}
		}
	}
	return h
}

func (c *Client) initialize(ctx context.Context) error {
	c.mu.Lock()
	c.session, c.version = "", ""
	c.mu.Unlock()
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "a2a-run", "version": c.clientVersion},
	}
	var res initializeResult
	session, err := c.roundTrip(ctx, "initialize", params, true, &res)
	if err != nil {
		return err
	}
	if res.ProtocolVersion == "" {
		return c.ep.errorf("the initialize result has no protocolVersion")
	}
	c.mu.Lock()
	c.session, c.version, c.init = session, res.ProtocolVersion, res
	c.mu.Unlock()
	return c.Notify(ctx, "notifications/initialized", nil)
}

// Call sends the request method with params and decodes its result into
// result. When the endpoint forgot the session, it starts a new one and
// sends the request again.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	_, err := c.roundTrip(ctx, method, params, true, result)
	if !errors.Is(err, errSessionGone) {
		return err
	}
	if err := c.initialize(ctx); err != nil {
		return err
	}
	_, err = c.roundTrip(ctx, method, params, true, result)
	return err
}

// Notify sends the notification method with params.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	_, err := c.roundTrip(ctx, method, params, false, nil)
	return err
}

// Close ends the session.
func (c *Client) Close(ctx context.Context) error {
	defer c.hc.CloseIdleConnections()
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.ep.URL(), nil)
	if err != nil {
		return err
	}
	c.headers(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.ep.errorf("ending the session: %w", withoutURL(err))
	}
	return closeBody(resp)
}

var errSessionGone = errors.New("the endpoint forgot the session")

// headers sets the session headers of a request.
func (c *Client) headers(req *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.version != "" {
		req.Header.Set("Mcp-Protocol-Version", c.version)
	}
}

// roundTrip sends one message, a request if call, and decodes the result
// of the answer into result. It returns the session ID of the answer.
func (c *Client) roundTrip(ctx context.Context, method string, params any, call bool, result any) (string, error) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return "", c.ep.errorf("%s: waiting for a free connection: %w", method, context.Cause(ctx))
	}
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	var id int64
	if call {
		id = c.ids.Add(1)
		msg["id"] = id
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ep.URL(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.headers(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", c.ep.errorf("%s: %w", method, withoutURL(err))
	}
	defer func() {
		if cerr := closeBody(resp); cerr != nil && err == nil {
			err = cerr
		}
	}()
	session := resp.Header.Get("Mcp-Session-Id")
	switch {
	case resp.StatusCode == http.StatusNotFound && req.Header.Get("Mcp-Session-Id") != "":
		return "", errSessionGone
	case !call && (resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent):
		return session, nil
	case resp.StatusCode != http.StatusOK:
		snippet, rerr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		if rerr != nil {
			return "", c.ep.errorf("%s: %s", method, resp.Status)
		}
		return "", c.ep.errorf("%s: %s: %s", method, resp.Status, strings.ToValidUTF8(strings.TrimSpace(string(snippet)), "?"))
	}
	raw, err := readAnswer(resp, id)
	if err != nil {
		return "", c.ep.errorf("%s: %w", method, err)
	}
	var ans struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return "", c.ep.errorf("%s: the answer isn't JSON-RPC: %w", method, err)
	}
	if string(ans.ID) != strconv.FormatInt(id, 10) {
		return "", c.ep.errorf("%s: the answer has the ID %s, want %d", method, ans.ID, id)
	}
	if ans.Error != nil {
		return "", ans.Error
	}
	if len(ans.Result) == 0 {
		return "", c.ep.errorf("%s: the answer has neither result nor error", method)
	}
	if result != nil {
		if err := json.Unmarshal(ans.Result, result); err != nil {
			return "", c.ep.errorf("%s: decoding the result: %w", method, err)
		}
	}
	return session, nil
}

// readAnswer returns the JSON-RPC answer with id in resp, a JSON body or
// an event stream, which may carry other messages first. It reads at most
// MaxMessage bytes.
func readAnswer(resp *http.Response, id int64) (json.RawMessage, error) {
	body := io.LimitReader(resp.Body, MaxMessage+1)
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("the answer has no valid Content-Type")
	}
	switch mt {
	case "application/json":
		data, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		if len(data) > MaxMessage {
			return nil, fmt.Errorf("the answer is over %d bytes", MaxMessage)
		}
		return data, nil
	case "text/event-stream":
		return readEvents(body, id)
	}
	return nil, fmt.Errorf("the answer is %s, want application/json or text/event-stream", mt)
}

// readEvents reads server-sent events until one carries the answer with id.
func readEvents(r io.Reader, id int64) (json.RawMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxMessage)
	var data bytes.Buffer
	want := strconv.FormatInt(id, 10)
	for sc.Scan() {
		line := sc.Text()
		if line != "" {
			if d, ok := strings.CutPrefix(line, "data:"); ok {
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimPrefix(d, " "))
			}
			continue
		}
		if data.Len() == 0 {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(data.Bytes(), &msg); err == nil && msg.Method == "" && string(msg.ID) == want {
			return bytes.Clone(data.Bytes()), nil
		}
		data.Reset()
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("an event of the answer is over %d bytes", MaxMessage)
		}
		return nil, err
	}
	return nil, errors.New("the event stream ended without the answer")
}

func closeBody(resp *http.Response) error {
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, MaxMessage)); err != nil {
		return errors.Join(err, resp.Body.Close())
	}
	return resp.Body.Close()
}

// withoutURL drops the URL of a request error, which may carry a secret
// in its query.
func withoutURL(err error) error {
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		return uerr.Err
	}
	return err
}
