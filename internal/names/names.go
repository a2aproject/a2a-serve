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

// Package names holds the rule for the names people give pairings, relay
// invites and advertised nodes, which appear in URLs, principals and chat
// messages.
package names

import "regexp"

// MaxLen bounds a name.
const MaxLen = 64

// Rule describes a valid name, for error messages.
const Rule = "up to 64 letters, digits, dots, hyphens or underscores, and starts with a letter or digit"

var pattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Valid reports whether name follows the rule.
func Valid(name string) bool {
	return len(name) <= MaxLen && pattern.MatchString(name)
}
