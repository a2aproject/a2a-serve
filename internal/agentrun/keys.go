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

package agentrun

import (
	"context"
	"sync"

	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
)

// keys tells which agent process each conversation runs in, by its key.
type keys struct {
	sessions a2aacp.SessionStore
	// newKey returns the key of a new conversation.
	newKey func(ctx context.Context, contextID string) (string, error)

	mu sync.Mutex
	// pending are the keys of conversations without a stored session yet.
	pending map[string]string
}

func newKeys(sessions a2aacp.SessionStore, newKey func(ctx context.Context, contextID string) (string, error)) *keys {
	return &keys{sessions: sessions, newKey: newKey, pending: map[string]string{}}
}

// of returns the key of a conversation: the one stored with its session,
// or, for a new conversation, the one newKey gives.
func (k *keys) of(ctx context.Context, contextID string) (string, error) {
	sess, ok, err := k.sessions.Get(ctx, contextID)
	if err != nil {
		return "", err
	}
	k.mu.Lock()
	if ok {
		delete(k.pending, contextID)
		k.mu.Unlock()
		return sess.AgentKey, nil
	}
	key, ok := k.pending[contextID]
	k.mu.Unlock()
	if ok {
		return key, nil
	}
	return k.newKey(ctx, contextID)
}

// remember keeps the key of a conversation until forget, in case its
// session isn't stored yet.
func (k *keys) remember(contextID, key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pending[contextID] = key
}

// forget drops the key kept for a conversation once its turn ended: a
// conversation that has a session is looked up there, and one that has
// none after a whole turn starts over with its next caller.
func (k *keys) forget(contextID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.pending, contextID)
}
