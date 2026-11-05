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

package mailboxapi

import "testing"

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url     string
		wantErr bool
	}{
		{url: "https://example.com/hook"},
		{url: "http://localhost:8080/hook"},
		{url: "ftp://example.com/hook", wantErr: true},
		{url: "example.com/hook", wantErr: true},
		{url: "https://", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			if err := ValidateWebhook(tc.url); (err != nil) != tc.wantErr {
				t.Fatalf("ValidateWebhook(%q) error = %v, want error %v", tc.url, err, tc.wantErr)
			}
		})
	}
}

func TestRedactURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url  string
		want string
	}{
		{url: "https://example.com/hook", want: "https://example.com/hook"},
		{url: "https://example.com/hook?key=secret", want: "https://example.com/hook?…"},
		{url: "https://user:pass@example.com/hook", want: "https://example.com/hook"},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			if got := RedactWebhookURL(tc.url); got != tc.want {
				t.Fatalf("RedactWebhookURL(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestOrigin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url  string
		want string
	}{
		{url: "https://discord.com/api/webhooks/123/s3cret", want: "https://discord.com"},
		{url: "https://user:pass@hooks.example.com:8443/hook?key=s3cret", want: "https://hooks.example.com:8443"},
		{url: "http://[::1]:8080/hook", want: "http://[::1]:8080"},
		{url: "::", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			if got := WebhookOrigin(tc.url); got != tc.want {
				t.Fatalf("WebhookOrigin(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}
