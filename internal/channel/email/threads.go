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

package email

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-serve/internal/channel"
)

const (
	threadsKey = "email:threads"
	// maxThreads and maxSent bound the state kept in the channel's KV. The
	// threads used least recently are forgotten first.
	maxThreads = 200
	maxSent    = 10
)

// threads remembers the subject of recent threads and the Message-IDs the
// agent sent in them, so a reply that references only the agent's mail
// stays in its conversation.
type threads struct {
	kv channel.KV

	mu     sync.Mutex
	loaded bool
	list   []thread // least recently used first
}

type thread struct {
	Conv    channel.ConvKey `json:"conv"`
	Subject string          `json:"subject,omitempty"`
	Sent    []string        `json:"sent,omitempty"`
}

func (t *threads) loadLocked(ctx context.Context) error {
	if t.loaded {
		return nil
	}
	v, ok, err := t.kv.Get(ctx, threadsKey)
	if err != nil {
		return fmt.Errorf("loading email threads: %w", err)
	}
	if ok {
		if err := json.Unmarshal([]byte(v), &t.list); err != nil {
			return fmt.Errorf("decoding email threads: %w", err)
		}
	}
	t.loaded = true
	return nil
}

func (t *threads) saveLocked(ctx context.Context) error {
	if len(t.list) > maxThreads {
		t.list = slices.Clone(t.list[len(t.list)-maxThreads:])
	}
	v, err := json.Marshal(t.list)
	if err != nil {
		return err
	}
	return t.kv.Set(ctx, threadsKey, string(v))
}

// touchLocked moves the thread of conv to the end, adding it if needed.
func (t *threads) touchLocked(conv channel.ConvKey) *thread {
	i := slices.IndexFunc(t.list, func(th thread) bool { return th.Conv == conv })
	th := thread{Conv: conv}
	if i >= 0 {
		th = t.list[i]
		t.list = slices.Delete(t.list, i, i+1)
	}
	t.list = append(t.list, th)
	return &t.list[len(t.list)-1]
}

// convOf returns the conversation with addr in which the agent sent one of
// ids.
func (t *threads) convOf(ctx context.Context, addr string, ids []string) (channel.ConvKey, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.loadLocked(ctx); err != nil {
		return "", false, err
	}
	for i := len(t.list) - 1; i >= 0; i-- {
		th := t.list[i]
		if a, _, _ := parseConv(th.Conv); a != addr {
			continue
		}
		for _, id := range ids {
			if slices.Contains(th.Sent, id) {
				return th.Conv, true, nil
			}
		}
	}
	return "", false, nil
}

// received records the subject of the latest mail in conv.
func (t *threads) received(ctx context.Context, conv channel.ConvKey, subject string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.loadLocked(ctx); err != nil {
		return err
	}
	th := t.touchLocked(conv)
	if subject != "" {
		th.Subject = subject
	}
	return t.saveLocked(ctx)
}

// sent records a Message-ID the agent sends in conv.
func (t *threads) sent(ctx context.Context, conv channel.ConvKey, msgID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.loadLocked(ctx); err != nil {
		return err
	}
	th := t.touchLocked(conv)
	th.Sent = append(th.Sent, msgID)
	if len(th.Sent) > maxSent {
		th.Sent = slices.Clone(th.Sent[len(th.Sent)-maxSent:])
	}
	return t.saveLocked(ctx)
}

// subject returns the subject recorded for conv.
func (t *threads) subject(ctx context.Context, conv channel.ConvKey) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.loadLocked(ctx); err != nil {
		return "", err
	}
	for _, th := range t.list {
		if th.Conv == conv {
			return th.Subject, nil
		}
	}
	return "", nil
}
