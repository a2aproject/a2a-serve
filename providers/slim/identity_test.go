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
	"errors"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-serve/netprovider"
)

func TestNewIdentityTokenErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		token string
	}{
		{name: "missing", token: ""},
		{name: "too short", token: strings.Repeat("x", minSecretLen-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := newIdentity(tt.token)
			var e *netprovider.Error
			if !errors.As(err, &e) || e.Kind != netprovider.KindToken || !strings.Contains(e.Fix, "--network-token") {
				t.Fatalf("newIdentity() error = %v, want a token error with a fix that names --network-token", err)
			}
		})
	}
	if _, err := newIdentity(strings.Repeat("x", minSecretLen)); err != nil {
		t.Fatalf("newIdentity() error = %v, want nil", err)
	}
}

func TestSharedSecretCallerName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		subject string
		want    string
	}{
		{subject: "a2a/demo/reviewer_vDSmfCPv", want: "a2a/demo/reviewer"},
		{subject: "agntcy/demo/echo_agent_a1B2c3D4", want: "agntcy/demo/echo_agent"},
		{subject: "a2a/demo/reviewer", want: "a2a/demo/reviewer"},
		{subject: "a2a/demo/reviewer_short", want: "a2a/demo/reviewer_short"},
	}
	for _, tt := range tests {
		t.Run(tt.subject, func(t *testing.T) {
			t.Parallel()
			if got := sharedSecret("").callerName(tt.subject); got != tt.want {
				t.Fatalf("sharedSecret.callerName(%q) = %q, want %q", tt.subject, got, tt.want)
			}
		})
	}
}
