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

package agentsrv

import (
	"cmp"
	"time"

	"github.com/a2aproject/a2a-serve/internal/agentsrv/agentproxy"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/messenger"
)

// Timings are the delays and timeouts of the server. Zero fields take their
// defaults.
type Timings struct {
	// WatchInterval is the first delay between checks of a watched task; it
	// grows to WatchMaxInterval.
	WatchInterval    time.Duration
	WatchMaxInterval time.Duration
	// WatchStale stops a watch whose task kept its state this long, not
	// counting the time it waits for the user's answer to its approval.
	WatchStale time.Duration
	// WatchMaxAge stops a watch this old.
	WatchMaxAge time.Duration
	// EditInterval throttles streaming edits; messengers rate-limit them.
	EditInterval time.Duration
	// SendRetry is the first delay before a message that must reach the
	// chat, such as a question, is sent again; it doubles on each failure.
	SendRetry time.Duration
	// ScheduleMinWait is the shortest wait between checks of the schedules.
	ScheduleMinWait time.Duration
	// WebhookRetries are the delays before each wake delivery attempt to a
	// webhook node; nil uses the defaults of the nodes package.
	WebhookRetries []time.Duration
	// ChannelRetryMin is the first delay before a failed channel restarts.
	ChannelRetryMin time.Duration
	// FirstBackupDelay keeps the first snapshot of a run, when one is due,
	// from slowing down the start.
	FirstBackupDelay time.Duration
	// AnonymousQuestionTimeout is how long the approvals of anonymous
	// callers wait for an owner.
	AnonymousQuestionTimeout time.Duration
	// UserQuestionTimeout is how long the approvals of authenticated users
	// wait for an answer. An open approval keeps the user's agent process
	// busy, so the pool of these processes fills up if nobody answers.
	UserQuestionTimeout time.Duration
	// TurnStallTimeout fails a turn in which the agent sent nothing this
	// long while none of its tool calls ran.
	TurnStallTimeout time.Duration
}

func (t Timings) messenger() messenger.Timings {
	return messenger.Timings{EditInterval: t.EditInterval, SendRetry: t.SendRetry, AnonymousQuestionTimeout: t.AnonymousQuestionTimeout}
}

func (t Timings) agentproxy() agentproxy.Timings {
	return agentproxy.Timings{WatchInterval: t.WatchInterval, WatchMaxInterval: t.WatchMaxInterval, WatchStale: t.WatchStale, WatchMaxAge: t.WatchMaxAge}
}

func (t Timings) withDefaults() Timings {
	t.WatchInterval = cmp.Or(t.WatchInterval, 5*time.Second)
	t.WatchMaxInterval = cmp.Or(t.WatchMaxInterval, 30*time.Second)
	t.WatchStale = cmp.Or(t.WatchStale, 24*time.Hour)
	t.WatchMaxAge = cmp.Or(t.WatchMaxAge, 7*24*time.Hour)
	t.EditInterval = cmp.Or(t.EditInterval, time.Second)
	t.SendRetry = cmp.Or(t.SendRetry, time.Second)
	t.ScheduleMinWait = cmp.Or(t.ScheduleMinWait, time.Second)
	t.ChannelRetryMin = cmp.Or(t.ChannelRetryMin, 5*time.Second)
	t.FirstBackupDelay = cmp.Or(t.FirstBackupDelay, time.Minute)
	t.AnonymousQuestionTimeout = cmp.Or(t.AnonymousQuestionTimeout, 10*time.Minute)
	t.UserQuestionTimeout = cmp.Or(t.UserQuestionTimeout, 4*time.Hour)
	t.TurnStallTimeout = cmp.Or(t.TurnStallTimeout, 10*time.Minute)
	return t
}
