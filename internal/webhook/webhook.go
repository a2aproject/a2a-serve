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

// Package webhook checks and redacts the URLs of webhook nodes.
package webhook

import (
	"fmt"
	"net/url"
)

// Validate checks the URL of a webhook node.
func Validate(u string) error {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
		return fmt.Errorf("--webhook must be an http(s) URL, got %q", u)
	}
	return nil
}

// RedactURL drops the query, which webhook providers often use for secrets.
func RedactURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	if p.RawQuery != "" {
		p.RawQuery = "…"
	}
	p.User = nil
	return p.String()
}

// Origin returns the scheme and host of a webhook URL, which is all of it
// that is safe to log: providers such as Discord and Slack keep the secret
// in the path.
func Origin(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return (&url.URL{Scheme: p.Scheme, Host: p.Host}).String()
}
