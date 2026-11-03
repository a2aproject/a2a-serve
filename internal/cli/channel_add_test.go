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

package cli

import (
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
)

func TestChannelAddTokenFromStdin(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

	_, err := runCMDWithStdin(t, pipeWith(t, ""), "channel", "add", "telegram", "--token", "-", "--dir", d.Root)

	if !isUsage(err) || !strings.Contains(err.Error(), "--token: empty secret") {
		t.Fatalf("channel add telegram --token - with empty stdin error = %v, want a usage error %q", err, "--token: empty secret")
	}
}

func TestChannelAddEmail(t *testing.T) {
	t.Parallel()
	d := newTestAgent(t, &agentdir.Config{Name: "coder", Harness: "opencode"})

	_, err := runCMD(t, "channel", "add", "email", "--dir", d.Root,
		"--email-address", "coder@example.com", "--email-imap", "127.0.0.1:1", "--email-smtp", "127.0.0.1:2",
		"--email-username", "coder", "--email-password", "pw", "--email-trusted-authserv", "mx.example.com")

	if err == nil || !strings.Contains(err.Error(), "IMAP 127.0.0.1:1") {
		t.Fatalf("channel add email error = %v, want the IMAP login of the given server to fail", err)
	}
}
