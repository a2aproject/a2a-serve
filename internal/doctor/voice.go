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
	"context"
	"encoding/binary"
	"errors"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/speech"
)

const fixVoiceKey = "a2a-serve config set --voice-key -"

// checkVoice checks the speech service of --voice, and with online that it
// speaks a short sample and transcribes it back.
func checkVoice(ctx context.Context, cfg *agentdir.Config, secrets agentdir.Secrets, online bool) (Result, bool) {
	v := cfg.Voice
	if !v.On() {
		return Result{}, false
	}
	name := "voice service " + speech.Describe(v.Service)
	key, err := secrets.Resolve(v.Key)
	if err != nil {
		return Result{ID: "voice", Status: StatusFail, Message: err.Error(), Fix: fixVoiceKey}, true
	}
	transcriber, speaker, err := speech.New(speech.Config{Service: v.Service, Key: key})
	if err != nil {
		return Result{ID: "voice", Status: StatusFail, Message: err.Error(), Fix: "a2a-serve config set --voice <service>"}, true
	}
	if !online {
		return Result{ID: "voice", Status: StatusOK, Message: name + " is set up; --online checks that it transcribes and speaks"}, true
	}
	sample, err := speaker.Speak(ctx, "Voice check.", speech.OutputModes)
	var e *speech.Error
	noSpeech := errors.As(err, &e) && e.NoSpeech
	if err != nil && !noSpeech {
		return failVoice(name+" can't speak: "+speech.Reason(err), err), true
	}
	if noSpeech {
		sample = silence()
	}
	// A sample of silence has no words to hear.
	if _, err := transcriber.Transcribe(ctx, sample); err != nil && !(errors.As(err, &e) && e.NoWords) {
		return failVoice(name+" can't transcribe: "+speech.Reason(err), err), true
	}
	if noSpeech {
		return Result{ID: "voice", Status: StatusOK, Message: name + " transcribes; it doesn't speak, so replies stay text"}, true
	}
	return Result{ID: "voice", Status: StatusOK, Message: name + " transcribes and speaks"}, true
}

// failVoice is a failed voice check, with the command that replaces a
// refused key.
func failVoice(msg string, err error) Result {
	r := Result{ID: "voice", Status: StatusFail, Message: msg}
	if speech.Reason(err) == "the speech service refused the key" {
		r.Fix = fixVoiceKey
	}
	return r
}

// silence is half a second of a 16 kHz, 16-bit mono WAV file, which checks
// transcription with a service that doesn't speak.
func silence() speech.Audio {
	const rate, size = 16000, 16000
	data := make([]byte, 44+size)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], 36+size)
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], rate)
	binary.LittleEndian.PutUint32(data[28:], rate*2)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], size)
	return speech.Audio{MediaType: speech.MediaWAV, Data: data}
}
