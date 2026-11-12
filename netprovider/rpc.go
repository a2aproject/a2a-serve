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

package netprovider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxMessage bounds one JSON-RPC message, a line on stdin or stdout.
const MaxMessage = 1 << 20

// JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	// CodeProviderError is an error of the provider, with an [ErrorData].
	CodeProviderError = -32000
)

// Message is a JSON-RPC 2.0 request, response or notification.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the error of a JSON-RPC response.
type RPCError struct {
	Code    int        `json:"code"`
	Message string     `json:"message"`
	Data    *ErrorData `json:"data,omitempty"`
}

// ErrorData says what kind of provider error a response carries.
type ErrorData struct {
	Kind    string `json:"kind,omitempty"`
	Problem string `json:"problem,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// Err returns the error a response carries: an [*Error] for provider errors
// with a kind, and the RPCError otherwise.
func (e *RPCError) Err() error {
	if e.Data != nil && e.Data.Kind != "" {
		return &Error{Kind: e.Data.Kind, Problem: e.Data.Problem, Fix: e.Data.Fix}
	}
	return e
}

// RPCErrorOf returns the JSON-RPC error that reports err.
func RPCErrorOf(err error) *RPCError {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	var e *Error
	if errors.As(err, &e) {
		return &RPCError{Code: CodeProviderError, Message: e.Error(), Data: &ErrorData{Kind: e.Kind, Problem: e.Problem, Fix: e.Fix}}
	}
	return &RPCError{Code: CodeProviderError, Message: err.Error()}
}

// ErrMessageTooLong is returned by [ReadMessage] for a line longer than
// [MaxMessage].
var ErrMessageTooLong = errors.New("JSON-RPC message longer than 1 MiB")

// ReadMessage reads the next message, a line of r. It skips empty lines.
func ReadMessage(r *bufio.Reader) (*Message, error) {
	for {
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, &RPCError{Code: CodeParseError, Message: "invalid JSON: " + err.Error()}
		}
		if m.JSONRPC != "2.0" {
			return &m, &RPCError{Code: CodeInvalidRequest, Message: `want "jsonrpc": "2.0"`}
		}
		return &m, nil
	}
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > MaxMessage {
			// Drop the rest of the line, so that the next read starts at
			// the next message.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			return nil, ErrMessageTooLong
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return line, nil
		default:
			return nil, err
		}
	}
}

// EncodeMessage returns m as a line.
func EncodeMessage(m *Message) ([]byte, error) {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(b) >= MaxMessage {
		return nil, fmt.Errorf("%w: %d bytes", ErrMessageTooLong, len(b))
	}
	return append(b, '\n'), nil
}
