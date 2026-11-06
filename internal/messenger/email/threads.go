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

	"github.com/a2aproject/a2a-serve/internal/messenger"
)

const (
	threadsKey = "email:threads"
	// maxThreads and maxSent bound the state kept in the messenger's KV. The
	// threads used least recently are forgotten first.
	maxThreads = 200
	maxSent    = 10
)

// threads remembers the subject of recent threads and the Message-IDs the
// agent sent in them, so a reply that references only the agent's mail
// stays in its conversation.
type threads struct {
	kv messenger.KV

	mu     sync.Mutex
	loaded bool
	state  threadState
}

type threadState struct {
	// Legacy is set for a mailbox the agent read before conversations were
	// keyed without the subject. Replies to threads it doesn't know get the
	// old key, so conversations started before keep their context.
	Legacy bool     `json:"legacy,omitempty"`
	List   []thread `json:"threads"` // least recently used first
}

type thread struct {
	Conv    messenger.ConvKey `json:"conv"`
	Subject string            `json:"subject,omitempty"`
	Sent    []string          `json:"sent,omitempty"`
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
		if err := json.Unmarshal([]byte(v), &t.state); err != nil {
			return fmt.Errorf("decoding email threads: %w", err)
		}
		t.loaded = true
		return nil
	}
	_, read, err := t.kv.Get(ctx, lastUIDKey)
	if err != nil {
		return fmt.Errorf("loading the last UID: %w", err)
	}
	t.state.Legacy = read
	if err := t.saveLocked(ctx); err != nil {
		return err
	}
	t.loaded = true
	return nil
}

// load reads the state. The messenger loads it before it first records a
// UID, so a new mailbox isn't taken for one read before the upgrade.
func (t *threads) load(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.loadLocked(ctx)
}

func (t *threads) saveLocked(ctx context.Context) error {
	if len(t.state.List) > maxThreads {
		t.state.List = slices.Clone(t.state.List[len(t.state.List)-maxThreads:])
	}
	v, err := json.Marshal(t.state)
	if err != nil {
		return err
	}
	return t.kv.Set(ctx, threadsKey, string(v))
}

// touchLocked moves the thread of conv to the end, adding it if needed.
func (t *threads) touchLocked(conv messenger.ConvKey) *thread {
	i := slices.IndexFunc(t.state.List, func(th thread) bool { return th.Conv == conv })
	th := thread{Conv: conv}
	if i >= 0 {
		th = t.state.List[i]
		t.state.List = slices.Delete(t.state.List, i, i+1)
	}
	t.state.List = append(t.state.List, th)
	return &t.state.List[len(t.state.List)-1]
}

// convOf returns the conversation with addr in which the agent sent one of
// ids, or else a known conversation with addr and root. Without one, a
// reply in a legacy mailbox gets the key with the base subject, as before
// the upgrade.
func (t *threads) convOf(ctx context.Context, addr, root string, ids []string, reply bool, subject string) (messenger.ConvKey, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.loadLocked(ctx); err != nil {
		return "", err
	}
	for i := len(t.state.List) - 1; i >= 0; i-- {
		th := t.state.List[i]
		if a, _, _ := parseConv(th.Conv); a != addr {
			continue
		}
		for _, id := range ids {
			if slices.Contains(th.Sent, id) {
				return th.Conv, nil
			}
		}
	}
	for i := len(t.state.List) - 1; i >= 0; i-- {
		th := t.state.List[i]
		if a, r, _ := parseConv(th.Conv); a == addr && r == root {
			return th.Conv, nil
		}
	}
	if reply && t.state.Legacy {
		return legacyConvKey(addr, root, subject), nil
	}
	return ConvKey(addr, root), nil
}

// received records the subject of the latest mail in conv.
func (t *threads) received(ctx context.Context, conv messenger.ConvKey, subject string) error {
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
func (t *threads) sent(ctx context.Context, conv messenger.ConvKey, msgID string) error {
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
func (t *threads) subject(ctx context.Context, conv messenger.ConvKey) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.loadLocked(ctx); err != nil {
		return "", err
	}
	for _, th := range t.state.List {
		if th.Conv == conv {
			return th.Subject, nil
		}
	}
	return "", nil
}
