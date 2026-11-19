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

package fsys

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultMaxChanges is how many changes the feed of a tree keeps at least.
const defaultMaxChanges = 10_000

// changeLog is the feed of changes of a tree. It keeps the last max
// changes, and up to twice as many until it trims them. A cursor is the
// epoch of the log, unique to it, and the count of changes before it.
type changeLog struct {
	epoch string
	max   int

	mu sync.Mutex
	// first counts the changes dropped before entries.
	first   uint64
	entries []Info
	// wake is closed on every change.
	wake chan struct{}
}

func newChangeLog(max int) *changeLog {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return &changeLog{epoch: hex.EncodeToString(b), max: max, wake: make(chan struct{})}
}

// add records changes and wakes the waiters.
func (l *changeLog) add(changes ...Info) {
	if len(changes) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, changes...)
	if len(l.entries) >= 2*l.max {
		drop := len(l.entries) - l.max
		l.entries = append([]Info(nil), l.entries[drop:]...)
		l.first += uint64(drop)
	}
	l.notifyLocked()
}

// notify wakes the waiters without a change, so that they look again.
func (l *changeLog) notify() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.notifyLocked()
}

func (l *changeLog) notifyLocked() {
	close(l.wake)
	l.wake = make(chan struct{})
}

func (l *changeLog) cursor() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cursorLocked()
}

func (l *changeLog) cursorLocked() string {
	return l.epoch + "." + strconv.FormatUint(l.first+uint64(len(l.entries)), 10)
}

// since returns the latest state of each entry that changed after cursor,
// the next cursor, and the channel closed on the next change.
func (l *changeLog) since(cursor string) ([]Info, string, <-chan struct{}, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	next := l.cursorLocked()
	epoch, n, ok := strings.Cut(cursor, ".")
	seq, err := strconv.ParseUint(n, 10, 64)
	end := l.first + uint64(len(l.entries))
	if !ok || err != nil || epoch != l.epoch || seq < l.first || seq > end {
		return nil, next, nil, fmt.Errorf("%w: %q", ErrResync, cursor)
	}
	changed := l.entries[seq-l.first:]
	last := make(map[string]int, len(changed))
	for i, c := range changed {
		last[c.Path] = i
	}
	var out []Info
	for i, c := range changed {
		if last[c.Path] == i {
			out = append(out, c)
		}
	}
	return out, next, l.wake, nil
}

// waitChanges is Changes of a tree whose feed is l. refresh, if set, adds
// the changes made outside the tree's methods, and runs before every look
// at l and at least every poll while Changes waits.
func waitChanges(ctx context.Context, l *changeLog, cursor string, refresh func() error, poll time.Duration) ([]Info, string, error) {
	for {
		if refresh != nil {
			if err := refresh(); err != nil {
				return nil, "", err
			}
		}
		if cursor == "" {
			return nil, l.cursor(), nil
		}
		changes, next, wake, err := l.since(cursor)
		if err != nil || len(changes) > 0 {
			return changes, next, err
		}
		if err := wait(ctx, wake, poll); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, cursor, nil
			}
			return nil, "", err
		}
	}
}

// wait waits for wake, for poll if set, or for ctx to end.
func wait(ctx context.Context, wake <-chan struct{}, poll time.Duration) error {
	var tick <-chan time.Time
	if poll > 0 {
		t := time.NewTimer(poll)
		defer t.Stop()
		tick = t.C
	}
	select {
	case <-wake:
	case <-tick:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
