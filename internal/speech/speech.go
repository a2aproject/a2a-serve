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

// Package speech turns voice notes into text and replies into voice notes,
// through the speech service the owner configured.
package speech

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Audio is a recording and its media type, such as audio/ogg.
type Audio struct {
	MediaType string
	Data      []byte
}

// Transcriber turns speech into text.
type Transcriber interface {
	// Transcribe returns the words of a, or an [*Error].
	Transcribe(ctx context.Context, a Audio) (string, error)
}

// Speaker turns text into speech in one of the accepted media types.
type Speaker interface {
	// Speak returns text as audio in a media type of accepted, or an
	// [*Error].
	Speak(ctx context.Context, text string, accepted []string) (Audio, error)
}

// OpenAI names the hosted OpenAI audio API as a service.
const OpenAI = "openai"

// Services are the service names --voice takes besides a URL.
var Services = []string{OpenAI}

// Media types of the audio the services speak, the preferred first.
const (
	MediaOgg  = "audio/ogg"
	MediaMPEG = "audio/mpeg"
	MediaWAV  = "audio/wav"
)

// InputModes are the media types of the audio callers can send.
var InputModes = []string{MediaOgg, MediaMPEG, MediaWAV}

// OutputModes are the media types of the audio replies.
var OutputModes = []string{MediaOgg}

// ParseService checks the value of --voice: a service name, or the http or
// https URL of a server with the OpenAI audio API shape.
func ParseService(v string) error {
	if v == OpenAI {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("want %s or the http(s) URL of a server with the OpenAI audio API, such as http://localhost:8000/v1", strings.Join(Services, ", "))
	}
	if u.User != nil {
		return errors.New("put the key in --voice-key, not in the URL")
	}
	return nil
}

// Describe names the service for logs and status: its name, or its URL
// without a query.
func Describe(service string) string {
	u, err := url.Parse(service)
	if err != nil || u.Host == "" {
		return service
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// Host returns the host that calls of the service go to, which is empty
// for an invalid service.
func Host(service string) string {
	if service == OpenAI {
		service = openAIURL
	}
	u, err := url.Parse(service)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// VoiceReplyKey is the metadata key that marks the file part of a spoken
// reply, which messengers send as a voice note.
const VoiceReplyKey = "a2a-serve/voice-reply"

// TranscriptKey is the metadata key of the status message that shows what a
// recording said, with the transcript as its value.
const TranscriptKey = "a2a-serve/transcript"

// Config selects a speech service.
type Config struct {
	// Service is a name of [Services] or a URL.
	Service string
	// Key is the API key of the service, empty if it needs none.
	Key string
	// Voice names the voice of the replies; empty takes alloy, which servers
	// with the OpenAI shape map to a voice of their own.
	Voice string
}

// New returns the transcriber and speaker of the configured service.
func New(cfg Config) (Transcriber, Speaker, error) {
	if err := ParseService(cfg.Service); err != nil {
		return nil, nil, err
	}
	c := newOpenAIClient(cfg)
	return c, c, nil
}

// Error is a failed call of the speech service. Its message names no URL or
// key, so it can be logged and shown to the person who sent the audio.
type Error struct {
	// Reason is what went wrong, such as "the speech service timed out".
	Reason string
	// NoSpeech is set when the service has no speech synthesis.
	NoSpeech bool
	// NoWords is set when the service heard no words in a recording.
	NoWords bool
	// status is the HTTP status of a refused call.
	status int
	err    error
}

func (e *Error) Error() string { return e.Reason }

func (e *Error) Unwrap() error { return e.err }

// Reason returns what err tells people about a failed call.
func Reason(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return "the speech service failed"
}
