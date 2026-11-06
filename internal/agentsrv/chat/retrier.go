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

package chat

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/a2aproject/a2a-serve/internal/backoff"
	"github.com/a2aproject/a2a-serve/internal/messenger"
)

const (
	// maxRetryWait is the longest the server waits for a rate limit to end.
	maxRetryWait = 30 * time.Second
	// maxSendAttempts bounds the attempts to send one message.
	maxSendAttempts = 4
)

// retryPolicy tells which failures of a messenger call are retried.
type retryPolicy int

const (
	// retryNever makes one attempt.
	retryNever retryPolicy = iota
	// retryRateLimits retries calls the messenger asked to slow down.
	retryRateLimits
	// retryAll also retries other failures, for calls whose repetition
	// can't do harm, as sending a message.
	retryAll
)

// retrier retries the messenger calls of one conversation and remembers the
// deadline of the messenger's last rate limit, so that calls wait for it.
type retrier struct {
	first time.Duration

	mu    sync.Mutex
	until time.Time
}

func newRetrier(first time.Duration) *retrier {
	return &retrier{first: cmp.Or(first, time.Second)}
}

// note records the rate limit in err, if it is one.
func (r *retrier) note(err error) {
	var limit *messenger.RateLimitError
	if !errors.As(err, &limit) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if until := time.Now().Add(limit.RetryAfter); until.After(r.until) {
		r.until = until
	}
}

// remaining is how long the messenger asked to wait.
func (r *retrier) remaining() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Until(r.until)
}

// wait blocks until the rate limit ends. It fails at once if that takes
// longer than maxRetryWait.
func (r *retrier) wait(ctx context.Context) error {
	d := r.remaining()
	if d <= 0 {
		return nil
	}
	if d > maxRetryWait {
		return fmt.Errorf("the messenger asked to wait %s", d.Round(time.Second))
	}
	return sleep(ctx, d)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// do calls op, waiting for rate limits first and retrying failures as policy
// says, with a backoff that ctx bounds.
func (r *retrier) do(ctx context.Context, policy retryPolicy, op func() error) error {
	bo := backoff.New(r.first, 8*r.first, time.Hour)
	var err error
	for attempt := range maxSendAttempts {
		if err = r.wait(ctx); err != nil {
			return err
		}
		if err = op(); err == nil {
			return nil
		}
		r.note(err)
		var limit *messenger.RateLimitError
		limited := errors.As(err, &limit)
		if policy == retryNever || !limited && policy != retryAll || attempt == maxSendAttempts-1 {
			return err
		}
		if limited {
			continue
		}
		if sleepErr := sleep(ctx, bo.Next(0)); sleepErr != nil {
			return errors.Join(err, sleepErr)
		}
	}
	return err
}
