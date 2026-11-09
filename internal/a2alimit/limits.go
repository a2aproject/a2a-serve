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

// Package a2alimit bounds the A2A messages the server takes and the size of
// the tasks it returns.
package a2alimit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

const (
	// MaxParts is the most parts a message may have.
	MaxParts = 1000
	// MaxText is the most bytes of text the text parts of a message may
	// have together.
	MaxText = 1 << 20
	// MaxMetadata is the most bytes of JSON the metadata of a message and
	// its parts may have together.
	MaxMetadata = 64 << 10
	// MaxHistoryLength is the most history messages a response returns for
	// a task.
	MaxHistoryLength = 100
	// MaxListPart is the most bytes of a part in a list of tasks. Longer
	// parts are cut.
	MaxListPart = 16 << 10
)

// Limits are the bounds of messages and responses.
type Limits struct {
	// Parts is the most parts a message may have.
	Parts int
	// Text is the most bytes of text the text parts of a message may have.
	Text int
	// Metadata is the most bytes of JSON the metadata of a message and its
	// parts may have.
	Metadata int
	// History is the most history messages a response returns for a task.
	History int
}

// Default returns the limits the server applies.
func Default() Limits {
	return Limits{Parts: MaxParts, Text: MaxText, Metadata: MaxMetadata, History: MaxHistoryLength}
}

// Check returns an invalid params error that names the limit msg exceeds,
// or nil.
func (l Limits) Check(msg *a2a.Message) error {
	return l.CheckParts("a message", msg.Parts, msg.Metadata)
}

// CheckParts is [Limits.Check] for what, named in the error, with parts
// and metadata. The metadata of the parts counts too.
func (l Limits) CheckParts(what string, parts []*a2a.Part, metadata ...map[string]any) error {
	if len(parts) > l.Parts {
		return a2a.NewError(a2a.ErrInvalidParams, fmt.Sprintf("%s may have at most %d parts, got %d", what, l.Parts, len(parts)))
	}
	text, meta := 0, 0
	for _, m := range metadata {
		n, err := metadataSize(m)
		if err != nil {
			return a2a.NewError(a2a.ErrInvalidParams, fmt.Sprintf("the metadata of %s isn't valid JSON: %v", what, err))
		}
		meta += n
	}
	for _, p := range parts {
		if p == nil {
			continue
		}
		if t, ok := p.Content.(a2a.Text); ok {
			text += len(t)
		}
		n, err := metadataSize(p.Metadata)
		if err != nil {
			return a2a.NewError(a2a.ErrInvalidParams, fmt.Sprintf("the metadata of a part isn't valid JSON: %v", err))
		}
		meta += n
	}
	if text > l.Text {
		return a2a.NewError(a2a.ErrInvalidParams, fmt.Sprintf("the text of %s may have at most %d bytes, got %d", what, l.Text, text))
	}
	if meta > l.Metadata {
		return a2a.NewError(a2a.ErrInvalidParams, fmt.Sprintf("the metadata of %s and its parts may have at most %d bytes, got %d", what, l.Metadata, meta))
	}
	return nil
}

func metadataSize(m map[string]any) (int, error) {
	if len(m) == 0 {
		return 0, nil
	}
	data, err := json.Marshal(m)
	return len(data), err
}

// MessageCheck is a call interceptor that refuses messages over the limits.
// Add it before the interceptors that admit a message, so that a refused
// message takes none of the caller's quota.
type MessageCheck struct {
	a2asrv.PassthroughCallInterceptor
	limits Limits
}

var _ a2asrv.CallInterceptor = (*MessageCheck)(nil)

// NewMessageCheck returns an interceptor that refuses messages over limits.
func NewMessageCheck(limits Limits) *MessageCheck {
	return &MessageCheck{limits: limits}
}

// Before implements [a2asrv.CallInterceptor].
func (c *MessageCheck) Before(ctx context.Context, _ *a2asrv.CallContext, req *a2asrv.Request) (context.Context, any, error) {
	send, ok := req.Payload.(*a2a.SendMessageRequest)
	if !ok || send.Message == nil {
		return ctx, nil, nil
	}
	return ctx, nil, c.limits.Check(send.Message)
}
