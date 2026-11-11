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

package speech

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
)

// checkRecording refuses a recording over limit bytes or longer than
// MaxDuration.
func checkRecording(a Audio, limit int) error {
	if len(a.Data) > limit {
		return &Error{Reason: fmt.Sprintf("the recording is over %d MiB", limit>>20)}
	}
	if d, ok := Duration(a); ok && d > MaxDuration {
		return &Error{Reason: fmt.Sprintf("the recording is longer than %d minutes", int(MaxDuration.Minutes()))}
	}
	return nil
}

// send makes the request and returns the response body of up to limit
// bytes.
func send(ctx context.Context, client *http.Client, req *http.Request, limit int64) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("closing a response of the speech service", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if int64(len(data)) > limit {
		return nil, &Error{Reason: fmt.Sprintf("the speech service sent over %d MiB", limit>>20)}
	}
	return data, nil
}

// transportError describes a failed request without its URL.
func transportError(ctx context.Context, err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || isTimeout(err) {
		return &Error{Reason: "the speech service timed out", err: err}
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Reason: "the call of the speech service was canceled", err: err}
	}
	return &Error{Reason: "couldn't reach the speech service", err: err}
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

const reasonRefusedKey = "the speech service refused the key"

// statusError describes a response other than 200, with the message of an
// OpenAI or Google API error body if it has one.
func statusError(resp *http.Response) error {
	e := &Error{status: resp.StatusCode}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		e.Reason = reasonRefusedKey
	case http.StatusTooManyRequests:
		e.Reason = "the speech service is over its rate limit or quota"
	default:
		e.Reason = "the speech service answered " + resp.Status
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return e
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &body) != nil || body.Error.Message == "" {
		return e
	}
	e.err = errors.New(body.Error.Message)
	// The Gemini API refuses an invalid key with 400 and says so in the
	// details.
	for _, d := range body.Error.Details {
		if d.Reason == "API_KEY_INVALID" {
			e.Reason = reasonRefusedKey
			return e
		}
	}
	// A refused request, such as one with an unknown voice, says what to
	// change. Refused keys and failures say nothing the owner can act on.
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
		e.Reason = "the speech service refused the request: " + Truncate(body.Error.Message, 200)
	}
	return e
}
