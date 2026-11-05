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
)

// Timings are the delays and timeouts of the server. Zero fields take their
// defaults.
type Timings struct {
	// WatchInterval is the first delay between checks of a watched task; it
	// grows to WatchMaxInterval.
	WatchInterval    time.Duration
	WatchMaxInterval time.Duration
	// EditInterval throttles streaming edits; messengers rate-limit them.
	EditInterval time.Duration
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
}

func (t Timings) withDefaults() Timings {
	t.WatchInterval = cmp.Or(t.WatchInterval, 5*time.Second)
	t.WatchMaxInterval = cmp.Or(t.WatchMaxInterval, 30*time.Second)
	t.EditInterval = cmp.Or(t.EditInterval, time.Second)
	t.ScheduleMinWait = cmp.Or(t.ScheduleMinWait, time.Second)
	t.ChannelRetryMin = cmp.Or(t.ChannelRetryMin, 5*time.Second)
	t.FirstBackupDelay = cmp.Or(t.FirstBackupDelay, time.Minute)
	t.AnonymousQuestionTimeout = cmp.Or(t.AnonymousQuestionTimeout, 10*time.Minute)
	return t
}
