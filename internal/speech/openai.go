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

package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Bounds of each call of the speech service.
const (
	// MaxInput is the largest audio sent for transcription, the limit of
	// the OpenAI API.
	MaxInput = 25 << 20
	// MaxDuration is the longest audio sent for transcription.
	MaxDuration = 5 * time.Minute
	// MaxSpeechText is the longest text sent to be spoken.
	MaxSpeechText = 4096
	// callTimeout bounds a call, the upload and the response included.
	callTimeout = 60 * time.Second
	// maxTranscript bounds the response of a transcription.
	maxTranscript = 1 << 20
	// maxSpeech bounds the audio of a spoken reply.
	maxSpeech = 16 << 20
)

const openAIURL = "https://api.openai.com/v1"

// openAIClient calls a server with the OpenAI audio API shape:
// /audio/transcriptions and /audio/speech.
type openAIClient struct {
	base       string
	key        string
	transcribe string
	speak      string
	voice      string
	http       *http.Client
}

func newOpenAIClient(cfg Config) *openAIClient {
	c := &openAIClient{
		base: strings.TrimSuffix(cfg.Service, "/"), key: cfg.Key,
		// Servers with the OpenAI shape, such as speaches, map OpenAI's
		// model and voice names to models of their own.
		transcribe: "whisper-1", speak: "tts-1", voice: "alloy",
		http: &http.Client{Timeout: callTimeout},
	}
	if cfg.Service == OpenAI {
		c.base, c.transcribe, c.speak = openAIURL, "gpt-4o-mini-transcribe", "gpt-4o-mini-tts"
	}
	return c
}

// extensions are the file extensions the OpenAI API reads the format of
// uploaded audio from.
var extensions = map[string]string{
	"audio/ogg": "ogg", "audio/opus": "ogg", "audio/mpeg": "mp3", "audio/mp3": "mp3",
	"audio/wav": "wav", "audio/x-wav": "wav", "audio/wave": "wav", "audio/vnd.wave": "wav",
	"audio/mp4": "m4a", "audio/x-m4a": "m4a", "audio/m4a": "m4a", "audio/aac": "m4a",
	"audio/webm": "webm", "audio/flac": "flac", "audio/x-flac": "flac",
}

// Transcribe implements [Transcriber].
func (c *openAIClient) Transcribe(ctx context.Context, a Audio) (string, error) {
	mediaType, _, err := mime.ParseMediaType(a.MediaType)
	if err != nil {
		mediaType = a.MediaType
	}
	ext, ok := extensions[strings.ToLower(mediaType)]
	if !ok {
		return "", &Error{Reason: fmt.Sprintf("the speech service doesn't take %s audio", a.MediaType)}
	}
	if len(a.Data) > MaxInput {
		return "", &Error{Reason: fmt.Sprintf("the recording is over %d MiB", MaxInput>>20)}
	}
	if d, ok := Duration(a); ok && d > MaxDuration {
		return "", &Error{Reason: fmt.Sprintf("the recording is longer than %d minutes", int(MaxDuration.Minutes()))}
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("file", "audio."+ext)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(a.Data); err != nil {
		return "", err
	}
	for k, v := range map[string]string{"model": c.transcribe, "response_format": "json"} {
		if err := w.WriteField(k, v); err != nil {
			return "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	data, err := c.post(ctx, "/audio/transcriptions", w.FormDataContentType(), &body, maxTranscript)
	if err != nil {
		return "", err
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", &Error{Reason: "the speech service sent an invalid transcript", err: err}
	}
	text := strings.TrimSpace(out.Text)
	if text == "" {
		return "", &Error{Reason: "the speech service heard no words", NoWords: true}
	}
	return text, nil
}

// formats are the response formats of /audio/speech, by media type.
var formats = map[string]string{MediaOgg: "opus", MediaMPEG: "mp3", MediaWAV: "wav"}

// Speak implements [Speaker]. It speaks in the first media type of
// accepted the service has, where audio/* takes any.
func (c *openAIClient) Speak(ctx context.Context, text string, accepted []string) (Audio, error) {
	mediaType, ok := pickFormat(accepted)
	if !ok {
		return Audio{}, &Error{Reason: "the speech service can't speak in " + strings.Join(accepted, ", ")}
	}
	if len(text) > MaxSpeechText {
		return Audio{}, &Error{Reason: fmt.Sprintf("the text is over %d bytes", MaxSpeechText)}
	}
	req, err := json.Marshal(map[string]string{"model": c.speak, "input": text, "voice": c.voice, "response_format": formats[mediaType]})
	if err != nil {
		return Audio{}, err
	}
	data, err := c.post(ctx, "/audio/speech", "application/json", bytes.NewReader(req), maxSpeech)
	var e *Error
	if errors.As(err, &e) && e.status == http.StatusNotFound {
		return Audio{}, &Error{Reason: "the speech service doesn't speak", NoSpeech: true, err: err}
	}
	if err != nil {
		return Audio{}, err
	}
	if len(data) == 0 {
		return Audio{}, &Error{Reason: "the speech service sent no audio"}
	}
	return Audio{MediaType: mediaType, Data: data}, nil
}

// pickFormat returns the first media type of accepted that the service
// speaks.
func pickFormat(accepted []string) (string, bool) {
	for _, a := range accepted {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "audio/*" || a == "audio/opus" {
			return MediaOgg, true
		}
		if a == "audio/mp3" {
			return MediaMPEG, true
		}
		if _, ok := formats[a]; ok {
			return a, true
		}
	}
	return "", false
}

// Accepts reports whether accepted, the acceptedOutputModes of a request,
// asks for audio the service can speak.
func Accepts(accepted []string) bool {
	_, ok := pickFormat(accepted)
	return ok
}

// post sends body to the endpoint path and returns the response body of up
// to limit bytes.
func (c *openAIClient) post(ctx context.Context, path, contentType string, body io.Reader, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, body)
	if err != nil {
		return nil, &Error{Reason: "the speech service URL is invalid"}
	}
	req.Header.Set("Content-Type", contentType)
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("closing a response of the speech service", "error", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if int64(len(data)) > limit {
		return nil, &Error{Reason: fmt.Sprintf("the speech service sent over %d MiB", limit>>20)}
	}
	return data, nil
}

// transportError describes a failed request without its URL.
func transportError(ctx context.Context, err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || isTimeout(err) {
		return &Error{Reason: "the speech service timed out", err: err}
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Reason: "the call of the speech service was canceled", err: err}
	}
	return &Error{Reason: "couldn't reach the speech service", err: err}
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// statusError describes a response other than 200, with the message of an
// OpenAI-style error body if it has one.
func statusError(resp *http.Response) error {
	e := &Error{status: resp.StatusCode}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		e.Reason = "the speech service refused the key"
	case http.StatusTooManyRequests:
		e.Reason = "the speech service is over its rate limit or quota"
	default:
		e.Reason = "the speech service answered " + resp.Status
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return e
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil && body.Error.Message != "" {
		e.err = errors.New(body.Error.Message)
	}
	return e
}
