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

// Package chat connects the agent to Telegram, Discord and email. It
// runs the messengers, turns their messages into turns of the agent, renders
// the turns' tasks in the chats, and asks and answers approvals there.
package chat

import "time"

// Timings are the delays of the messenger.
type Timings struct {
	// EditInterval throttles streaming edits; messengers rate-limit them.
	EditInterval time.Duration
	// SendRetry is the first delay before a message that must reach the
	// chat, such as a question, is sent again; it doubles on each failure.
	SendRetry time.Duration
	// AnonymousQuestionTimeout is how long the approvals of anonymous
	// callers wait for an owner.
	AnonymousQuestionTimeout time.Duration
}
