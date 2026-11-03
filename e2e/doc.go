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

// Package e2e holds end-to-end tests of the core user journeys. They build
// a2a-serve and the scripted fakeacp agent, install the pinned a2a CLI, and
// drive them as separate processes, each user with its own HOME.
//
// The tests are behind the e2e build tag:
//
//	go test -tags e2e ./e2e/...
//
// Set E2E_A2A_CLI to the path of an a2a binary to skip installing the CLI.
package e2e
