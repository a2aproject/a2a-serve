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

// Command mcpecho is a stdio MCP server with one tool, echo, which returns
// its text argument prefixed with "echo: ".
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 16<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var req request
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			fmt.Fprintln(os.Stderr, "mcpecho:", err)
			continue
		}
		if req.ID == nil {
			continue
		}
		result, rerr := handle(req)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rerr != nil {
			resp["error"] = rerr
		} else {
			resp["result"] = result
		}
		if err := out.Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "mcpecho:", err)
			os.Exit(1)
		}
	}
	if err := in.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "mcpecho:", err)
		os.Exit(1)
	}
}

func handle(req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mcpecho", "version": "1.0.0"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": []any{map[string]any{
			"name":        "echo",
			"description": "Returns the text.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
				"required":   []string{"text"},
			},
		}}}, nil
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name != "echo" {
			return nil, &rpcError{Code: -32602, Message: fmt.Sprintf("unknown tool call %s", req.Params)}
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + p.Arguments.Text}}}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
}
