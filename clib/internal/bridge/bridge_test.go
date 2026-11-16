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

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestCallsAskReply(t *testing.T) {
	t.Parallel()
	c := NewCalls(nil)
	t.Cleanup(c.Close)
	go func() {
		e, ok := c.Next(5 * time.Second)
		if !ok {
			t.Errorf("Calls.Next() = false, want a question")
			return
		}
		if err := c.Reply(e.ID, json.RawMessage(`{"action":"allow"}`)); err != nil {
			t.Errorf("Calls.Reply() error = %v", err)
		}
	}()
	got, err := c.Ask(context.Background(), "connect", map[string]any{"host": "example.com"}, 5*time.Second)
	if err != nil {
		t.Fatalf("Calls.Ask() error = %v", err)
	}
	if diff := cmp.Diff(`{"action":"allow"}`, string(got)); diff != "" {
		t.Fatalf("Calls.Ask() wrong result (-want +got) diff = %s", diff)
	}
}

func TestCallsAskTimesOut(t *testing.T) {
	t.Parallel()
	c := NewCalls(nil)
	t.Cleanup(c.Close)
	if _, err := c.Ask(context.Background(), "connect", nil, 10*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Calls.Ask() error = %v, want %v", err, context.DeadlineExceeded)
	}
	e, ok := c.Next(time.Second)
	if !ok {
		t.Fatal("Calls.Next() = false, want the queued question")
	}
	if err := c.Reply(e.ID, json.RawMessage(`{}`)); err == nil {
		t.Fatal("Calls.Reply() after the timeout error = nil, want an error")
	}
}

func TestCallsCloseFailsQuestions(t *testing.T) {
	t.Parallel()
	c := NewCalls(nil)
	done := make(chan error, 1)
	go func() {
		_, err := c.Ask(context.Background(), "start", nil, time.Minute)
		done <- err
	}()
	if _, ok := c.Next(5 * time.Second); !ok {
		t.Fatal("Calls.Next() = false, want a question")
	}
	c.Close()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("Calls.Ask() after Close error = %v, want %v", err, ErrClosed)
	}
	if _, ok := c.Next(time.Millisecond); ok {
		t.Fatal("Calls.Next() after Close = true, want false")
	}
}

func TestHandles(t *testing.T) {
	t.Parallel()
	var h Handles[string]
	a, b := h.Add("a"), h.Add("b")
	if a == 0 || a == b {
		t.Fatalf("Handles.Add() = %d, %d, want distinct handles other than 0", a, b)
	}
	if v, ok := h.Get(b); !ok || v != "b" {
		t.Fatalf("Handles.Get(%d) = %q, %v, want %q, true", b, v, ok, "b")
	}
	if _, ok := h.Remove(a); !ok {
		t.Fatalf("Handles.Remove(%d) = false, want true", a)
	}
	if _, ok := h.Get(a); ok {
		t.Fatalf("Handles.Get(%d) after Remove = true, want false", a)
	}
}

func TestCallsDiscardsLateAnswers(t *testing.T) {
	t.Parallel()
	discarded := make(chan string, 1)
	c := NewCalls(func(typ string, answer json.RawMessage) { discarded <- typ + " " + string(answer) })
	t.Cleanup(c.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Ask(ctx, "start", nil, time.Minute)
		done <- err
	}()
	e, ok := c.Next(5 * time.Second)
	if !ok {
		t.Fatal("Calls.Next() = false, want a question")
	}
	c.mu.Lock()
	cancel()
	// The answer lands while Ask gives up, which it can't see before it
	// takes the lock that Reply holds here.
	select {
	case c.pending[e.ID] <- json.RawMessage(`{"fd":7}`):
	default:
		t.Fatal("the answer channel is full")
	}
	c.mu.Unlock()
	if err := <-done; err == nil {
		// Ask may take the answer before it sees the cancellation.
		return
	}
	select {
	case got := <-discarded:
		if got != `start {"fd":7}` {
			t.Fatalf("discarded %q, want the late answer to start", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the late answer wasn't discarded")
	}
}
