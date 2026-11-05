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

// Package ctlhttp is what the control APIs of agents and relays share: the
// HTTP transport over a control socket, and how failed calls carry their
// [agenterr] type across it.
package ctlhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"

	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/clierr"
	"github.com/a2aproject/a2a-serve/internal/fsutil"
	"github.com/a2aproject/a2a-serve/internal/jsonhttp"
)

// SocketTransport dials the control socket at path. It refuses a socket in
// a directory that others can open, since whoever controls that directory
// could stand in for the server.
func SocketTransport(path string) *http.Transport {
	return &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if err := fsutil.CheckPrivate(filepath.Dir(path)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, &net.OpError{Op: "dial", Net: "unix", Err: err}
			}
			return nil, fmt.Errorf("not trusting the control socket %s: %w", path, err)
		}
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
}

// IsDialError reports whether err means nothing answered. Errors after the
// dial come from a server that runs.
func IsDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// errorBody is the body of a failed control call. Kind and Name are set
// for an [agenterr.NotFoundError]. Code, Hint and A2ACode carry the class
// of the error for the CLI's exit code; servers before them sent none.
type errorBody struct {
	Error   string `json:"error"`
	Kind    string `json:"kind,omitempty"`
	Name    string `json:"name,omitempty"`
	Code    string `json:"code,omitempty"`
	Hint    string `json:"hint,omitempty"`
	A2ACode string `json:"a2aCode,omitempty"`
}

// WriteError writes err as the response of a failed control call, with the
// status of its [agenterr] type; [ResponseError] turns it back into that type.
func WriteError(w http.ResponseWriter, err error) {
	body := errorBody{Error: err.Error()}
	if ce := clierr.Classify(err); ce.Code != clierr.CodeInternal {
		body.Code, body.Hint, body.A2ACode = ce.Code, ce.Hint, ce.A2ACode
	}
	status := http.StatusInternalServerError
	var (
		nf *agenterr.NotFoundError
		in *agenterr.InvalidError
		co *agenterr.ConflictError
	)
	switch {
	case errors.As(err, &nf):
		status, body.Kind, body.Name = http.StatusNotFound, nf.Kind, nf.Name
	case errors.As(err, &in):
		status = http.StatusBadRequest
	case errors.As(err, &co):
		status = http.StatusConflict
	}
	jsonhttp.Write(w, status, body)
}

// ResponseError returns the error of a failed control call, as the server
// wrote it with [WriteError].
func ResponseError(status string, code int, data []byte) error {
	var body errorBody
	if err := json.Unmarshal(data, &body); err != nil || body.Error == "" {
		return errors.New(jsonhttp.ErrorMessage(status, data))
	}
	err := typedError(code, body)
	if body.Code == "" {
		return err
	}
	ce := clierr.Wrap(body.Code, err)
	ce.Hint, ce.A2ACode = body.Hint, body.A2ACode
	return ce
}

func typedError(code int, body errorBody) error {
	switch code {
	case http.StatusNotFound:
		if body.Kind != "" {
			return &agenterr.NotFoundError{Kind: body.Kind, Name: body.Name}
		}
	case http.StatusBadRequest:
		return agenterr.Invalid(errors.New(body.Error))
	case http.StatusConflict:
		return &agenterr.ConflictError{Err: errors.New(body.Error)}
	}
	return errors.New(body.Error)
}
