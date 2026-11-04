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

// Package jsonhttp reads and writes the JSON bodies of the agent server's
// HTTP APIs, including their error bodies.
package jsonhttp

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
)

// MaxBody is the largest request body Read accepts.
const MaxBody = 32 << 20

// APIError is the body of an error response.
type APIError struct {
	Error string `json:"error"`
}

// Read decodes the JSON body of r into v. On failure it writes a 400
// response and returns false. An empty body leaves v unchanged.
func Read(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		Error(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// Write writes v as an indented JSON response with status code.
func Write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		slog.Debug("writing response", "error", err)
	}
}

// Error writes an [APIError] response.
func Error(w http.ResponseWriter, code int, msg string) {
	Write(w, code, APIError{Error: msg})
}

// RequireJSON refuses state-changing requests that a browser could send
// cross-site without a CORS preflight: they need a JSON content type or the
// A2A-Version header.
func RequireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("A2A-Version") != "" || isJSON(r.Header.Get("Content-Type")) {
			next.ServeHTTP(w, r)
			return
		}
		Error(w, http.StatusUnsupportedMediaType, "send the request body as application/json")
	})
}

func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && (mediaType == "application/json" || mediaType == "application/a2a+json")
}

// maxErrorBody is how much of a non-JSON error body ErrorMessage includes.
const maxErrorBody = 300

// ErrorMessage returns the message of an error response: the error of an
// [APIError] body, or the status followed by the start of the body.
func ErrorMessage(status string, body []byte) string {
	var apiErr APIError
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
		return apiErr.Error
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return status
	}
	if len(text) > maxErrorBody {
		text = text[:maxErrorBody] + "…"
	}
	return status + ": " + text
}
