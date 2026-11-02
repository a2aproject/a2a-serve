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

// Package skills exposes agent skill documents bundled into the binary.
package skills

import _ "embed"

// A2AMailbox is the contents of the a2a-mailbox SKILL.md, teaching a webhook node
// how to answer wakes. {{BASE}} stands for the hosting agent's URL.
//
//go:embed a2a-mailbox/SKILL.md
var A2AMailbox string

// A2AAgents is the contents of the a2a-agents SKILL.md, teaching a harness
// run by `a2a serve` how to call other agents through the server.
//
//go:embed a2a-agents/SKILL.md
var A2AAgents string
