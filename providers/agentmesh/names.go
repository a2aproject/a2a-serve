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

package main

import (
	"net/mail"
	"regexp"
	"strings"
)

// maxNameLen and namePattern are the rule of a2a-serve for agent names.
const maxNameLen = 64

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validName reports whether name is a valid agent name.
func validName(name string) bool {
	return len(name) <= maxNameLen && namePattern.MatchString(name)
}

// isEmail reports whether s is a plain email address with a dotted domain,
// such as a verified email, as opposed to a login name such as ben@github.
func isEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return false
	}
	_, domain, ok := strings.Cut(s, "@")
	return ok && strings.Contains(strings.Trim(domain, "."), ".")
}
