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

package doctor

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/speech/speechtest"
)

func TestCheckVoice(t *testing.T) {
	t.Parallel()
	speaking := speechtest.NewServer(t, speechtest.Options{Key: "sk-right", Transcript: "Voice check."})
	transcribing := speechtest.NewServer(t, speechtest.Options{NoSpeech: true})
	tests := []struct {
		name    string
		voice   agentdir.VoiceConfig
		secrets agentdir.Secrets
		online  bool
		want    *Result
	}{
		{name: "voice off"},
		{
			name:    "offline",
			voice:   agentdir.VoiceConfig{Service: speaking.URL(), Key: "secret:VOICE_API_KEY"},
			secrets: agentdir.Secrets{"VOICE_API_KEY": "sk-right"},
			want:    &Result{ID: "voice", Status: StatusOK, Message: "voice service " + speaking.URL() + " is set up; --online checks that it transcribes and speaks"},
		},
		{
			name:    "transcribes and speaks",
			voice:   agentdir.VoiceConfig{Service: speaking.URL(), Key: "secret:VOICE_API_KEY"},
			secrets: agentdir.Secrets{"VOICE_API_KEY": "sk-right"},
			online:  true,
			want:    &Result{ID: "voice", Status: StatusOK, Message: "voice service " + speaking.URL() + " transcribes and speaks"},
		},
		{
			name:    "refused key",
			voice:   agentdir.VoiceConfig{Service: speaking.URL(), Key: "secret:VOICE_API_KEY"},
			secrets: agentdir.Secrets{"VOICE_API_KEY": "sk-wrong"},
			online:  true,
			want:    &Result{ID: "voice", Status: StatusFail, Message: "voice service " + speaking.URL() + " can't speak: the speech service refused the key", Fix: "a2a-serve config set --voice-key -"},
		},
		{
			name:   "only transcribes",
			voice:  agentdir.VoiceConfig{Service: transcribing.URL()},
			online: true,
			want:   &Result{ID: "voice", Status: StatusOK, Message: "voice service " + transcribing.URL() + " transcribes; it doesn't speak, so replies stay text"},
		},
		{
			name:  "key not stored",
			voice: agentdir.VoiceConfig{Service: "openai", Key: "secret:VOICE_API_KEY"},
			want:  &Result{ID: "voice", Status: StatusFail, Message: "secret VOICE_API_KEY is not set; pass it again with the flag that set it", Fix: "a2a-serve config set --voice-key -"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := checkVoice(t.Context(), &agentdir.Config{Voice: tc.voice}, tc.secrets, tc.online)
			if !ok {
				if tc.want != nil {
					t.Fatalf("checkVoice() = no result, want %+v", *tc.want)
				}
				return
			}
			if tc.want == nil {
				t.Fatalf("checkVoice() = %+v, want no result", got)
			}
			if diff := cmp.Diff(*tc.want, got, cmp.AllowUnexported(Result{})); diff != "" {
				t.Fatalf("checkVoice() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
