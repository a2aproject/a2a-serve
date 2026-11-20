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
	"strings"
	"sync"
	"time"
)

const (
	// strangerReplyWindow is how long a sender who isn't linked waits for
	// another reply, and the window of strangerRepliesPerWindow.
	strangerReplyWindow = time.Hour
	// strangerRepliesPerWindow caps the replies to senders who aren't
	// linked, so that they can't use up the mailbox's daily send quota.
	strangerRepliesPerWindow = 20
	// maxStrangers caps the senders remembered.
	maxStrangers = 1000
)

// strangerReplies limits the replies to senders who aren't linked: one for
// each sender and strangerRepliesPerWindow in all, per strangerReplyWindow.
// See https://github.com/a2aproject/a2a-serve/issues/575.
type strangerReplies struct {
	now func() time.Time

	mu sync.Mutex
	// last is the time of the last reply to each sender.
	last map[string]time.Time
	// recent are the times of the replies in the window, oldest first.
	recent []time.Time
}

func newStrangerReplies(now func() time.Time) *strangerReplies {
	return &strangerReplies{now: now, last: map[string]time.Time{}}
}

// allow reports whether the server may reply to the sender address, and
// counts the reply if it may.
func (s *strangerReplies) allow(address string) bool {
	address = strings.ToLower(address)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.forget(now)
	if _, ok := s.last[address]; ok {
		return false
	}
	if len(s.recent) >= strangerRepliesPerWindow || len(s.last) >= maxStrangers {
		return false
	}
	s.last[address] = now
	s.recent = append(s.recent, now)
	return true
}

// forget drops the replies older than the window.
func (s *strangerReplies) forget(now time.Time) {
	for address, at := range s.last {
		if now.Sub(at) >= strangerReplyWindow {
			delete(s.last, address)
		}
	}
	for len(s.recent) > 0 && now.Sub(s.recent[0]) >= strangerReplyWindow {
		s.recent = s.recent[1:]
	}
}

// senders returns how many senders are remembered.
func (s *strangerReplies) senders() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.last)
}
