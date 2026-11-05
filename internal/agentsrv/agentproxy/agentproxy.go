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

// Package agentproxy lets the agent call other agents through the server.
// It serves the agents URLs the agent's a2a CLI calls, calls contacts with
// the server's credentials and webhook nodes in-process, and watches the
// tasks that finish later.
package agentproxy

import "time"

// Timings are the delays of the agents proxy.
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
}
