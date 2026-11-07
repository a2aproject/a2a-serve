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

package agentmesh

import (
	"errors"
	"testing"
)

func TestCheckRelayURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url          string
		wantErr      bool
		wantInsecure bool
	}{
		{url: "https://relay.example.com"},
		{url: "https://relay.example.com/"},
		{url: "https://127.0.0.1:8443"},
		{url: "http://127.0.0.1:8443", wantInsecure: true},
		{url: "http://localhost:8443", wantInsecure: true},
		{url: "http://[::1]:8443", wantInsecure: true},
		{url: "http://relay.example.com", wantErr: true},
		{url: "http://192.168.1.5:8443", wantErr: true},
		{url: "https://relay.example.com/x", wantErr: true},
		{url: "https://relay.example.com?a=b", wantErr: true},
		{url: "https://user@relay.example.com", wantErr: true},
		{url: "https://", wantErr: true},
		{url: "ftp://relay.example.com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			t.Parallel()
			if err := CheckRelayURL(tt.url); (err != nil) != tt.wantErr {
				t.Fatalf("CheckRelayURL(%q) error = %v, want error %v", tt.url, err, tt.wantErr)
			}
			if got := InsecureLoopback(tt.url); got != tt.wantInsecure {
				t.Fatalf("InsecureLoopback(%q) = %v, want %v", tt.url, got, tt.wantInsecure)
			}
		})
	}
}

func TestCheckRelayURLRefusesCleartext(t *testing.T) {
	t.Parallel()
	if err := CheckRelayURL("http://relay.example.com"); !errors.Is(err, ErrCleartextRelayURL) {
		t.Fatalf("CheckRelayURL() error = %v, want %v", err, ErrCleartextRelayURL)
	}
}
