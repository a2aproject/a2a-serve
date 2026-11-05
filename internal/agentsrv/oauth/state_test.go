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

package oauth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestStateSigner(t *testing.T) {
	t.Parallel()
	s := newStateSigner()
	p := newPending(time.Minute)
	p.Issuer, p.ClientRedirect, p.ClientState, p.ClientID, p.Challenge = "https://coder.example.com", "http://127.0.0.1:9999/callback", "s1", "a2a-cli", "challenge"
	state, err := s.seal(p)
	if err != nil {
		t.Fatalf("seal() error = %v", err)
	}
	payload, sig, _ := strings.Cut(state, ".")
	forged := p
	forged.ClientRedirect = "http://127.0.0.1:1/evil"
	forgedState, err := s.seal(forged)
	if err != nil {
		t.Fatalf("seal() error = %v", err)
	}
	forgedPayload, _, _ := strings.Cut(forgedState, ".")
	otherState, err := newStateSigner().seal(p)
	if err != nil {
		t.Fatalf("seal() error = %v", err)
	}
	tests := []struct {
		name  string
		state string
		now   time.Time
		want  bool
	}{
		{name: "fresh state", state: state, now: time.Now(), want: true},
		{name: "expired state", state: state, now: time.Now().Add(time.Minute + time.Second)},
		{name: "payload of another state", state: forgedPayload + "." + sig, now: time.Now()},
		{name: "state of another key", state: otherState, now: time.Now()},
		{name: "no signature", state: payload, now: time.Now()},
		{name: "unsigned payload", state: base64.RawURLEncoding.EncodeToString([]byte(`{"exp":9999999999}`)) + ".", now: time.Now()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := s.open(tc.state, tc.now)
			if ok != tc.want {
				t.Fatalf("open() ok = %t, want %t", ok, tc.want)
			}
			if !tc.want {
				return
			}
			if diff := cmp.Diff(p, got); diff != "" {
				t.Fatalf("open() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestStateSignerSecrets(t *testing.T) {
	t.Parallel()
	s := newStateSigner()
	p := newPending(time.Minute)
	state, err := s.seal(p)
	if err != nil {
		t.Fatalf("seal() error = %v", err)
	}
	for name, secret := range map[string]string{"googleVerifier": s.googleVerifier(p), "nonce": s.nonce(p)} {
		if strings.Contains(state, secret) {
			t.Fatalf("seal() state contains the %s", name)
		}
		if other := newStateSigner(); secret == other.googleVerifier(p) || secret == other.nonce(p) {
			t.Fatalf("%s() is the same under another key", name)
		}
	}
	if len(s.googleVerifier(p)) < 43 {
		t.Fatalf("googleVerifier() length = %d, want at least 43 for PKCE", len(s.googleVerifier(p)))
	}
}
