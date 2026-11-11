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

// Package speechtest fakes speech services for tests: in process, and as a
// server with the OpenAI audio API shape.
package speechtest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-serve/internal/speech"
)

// Fake is a transcriber and speaker that records its calls and returns
// scripted results.
type Fake struct {
	// Transcript is what every recording says.
	Transcript string
	// TranscribeErr fails every transcription.
	TranscribeErr error
	// Speech is the audio of every reply; empty speaks "OggS" in audio/ogg.
	Speech speech.Audio
	// SpeakErr fails every reply.
	SpeakErr error

	mu      sync.Mutex
	heard   []speech.Audio
	spoken  []string
	accepts [][]string
}

var (
	_ speech.Transcriber = (*Fake)(nil)
	_ speech.Speaker     = (*Fake)(nil)
)

// Transcribe implements [speech.Transcriber].
func (f *Fake) Transcribe(_ context.Context, a speech.Audio) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heard = append(f.heard, a)
	return f.Transcript, f.TranscribeErr
}

// Speak implements [speech.Speaker].
func (f *Fake) Speak(_ context.Context, text string, accepted []string) (speech.Audio, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spoken = append(f.spoken, text)
	f.accepts = append(f.accepts, accepted)
	if f.SpeakErr != nil {
		return speech.Audio{}, f.SpeakErr
	}
	if f.Speech.Data == nil {
		return speech.Audio{MediaType: speech.MediaOgg, Data: []byte("OggS")}, nil
	}
	return f.Speech, nil
}

// Heard returns the recordings transcribed so far.
func (f *Fake) Heard() []speech.Audio {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]speech.Audio(nil), f.heard...)
}

// Spoken returns the texts spoken so far.
func (f *Fake) Spoken() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.spoken...)
}

// Options configure a [Server].
type Options struct {
	// Key is the API key the server takes; empty takes any.
	Key string
	// Transcript is what every recording says.
	Transcript string
	// NoSpeech answers /audio/speech with 404, as a server without speech
	// synthesis does.
	NoSpeech bool
}

// Request is a call the server received.
type Request struct {
	Path string
	// Model, Voice and Format are fields of the request.
	Model, Voice, Format string
	// Input is the text to speak, or File the uploaded audio.
	Input string
	File  []byte
	// Filename is the name of the uploaded audio.
	Filename string
}

// Server is a speech server with the OpenAI audio API shape.
type Server struct {
	*httptest.Server
	t    testing.TB
	opts Options

	mu       sync.Mutex
	requests []Request
}

// Speech is the audio the server speaks: the start of an Ogg page.
var Speech = []byte("OggS\x00fake speech")

// NewServer starts a server, which the test closes.
func NewServer(t testing.TB, opts Options) *Server {
	s := &Server{t: t, opts: opts}
	s.Server = httptest.NewServer(s)
	t.Cleanup(s.Close)
	return s
}

// URL returns the base URL of the API, which --voice takes.
func (s *Server) URL() string { return s.Server.URL + "/v1" }

// Requests returns the calls received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.opts.Key != "" && r.Header.Get("Authorization") != "Bearer "+s.opts.Key {
		s.writeError(w, http.StatusUnauthorized, "Incorrect API key provided")
		return
	}
	req := Request{Path: r.URL.Path}
	switch r.URL.Path {
	case "/v1/audio/transcriptions":
		f, h, err := r.FormFile("file")
		if err != nil {
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.File, err = io.ReadAll(f)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Filename, req.Model, req.Format = h.Filename, r.FormValue("model"), r.FormValue("response_format")
		s.record(req)
		s.writeJSON(w, map[string]string{"text": s.opts.Transcript})
	case "/v1/audio/speech":
		if s.opts.NoSpeech {
			s.record(req)
			s.writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		var body struct {
			Model, Input, Voice string
			Format              string `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Model, req.Input, req.Voice, req.Format = body.Model, body.Input, body.Voice, body.Format
		s.record(req)
		w.Header().Set("Content-Type", "audio/ogg")
		if _, err := w.Write(Speech); err != nil {
			s.t.Logf("speechtest: writing speech: %v", err)
		}
	default:
		s.writeError(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) record(r Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r)
}

func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.t.Logf("speechtest: writing response: %v", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg}}); err != nil {
		s.t.Logf("speechtest: writing response: %v", err)
	}
}
