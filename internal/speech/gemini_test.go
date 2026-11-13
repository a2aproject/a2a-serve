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

package speech_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/pion/opus/pkg/oggreader"

	"github.com/a2aproject/a2a-serve/internal/speech"
)

type geminiBlob struct {
	MimeType string `json:"mimeType"`
	Data     []byte `json:"data"`
}

type geminiPart struct {
	Text       string      `json:"text,omitempty"`
	InlineData *geminiBlob `json:"inlineData,omitempty"`
}

type geminiSpeechConfig struct {
	VoiceConfig struct {
		PrebuiltVoiceConfig struct {
			VoiceName string `json:"voiceName"`
		} `json:"prebuiltVoiceConfig"`
	} `json:"voiceConfig"`
}

type geminiRequest struct {
	Path     string
	Key      string
	Contents []struct {
		Role  string       `json:"role"`
		Parts []geminiPart `json:"parts"`
	} `json:"contents"`
	GenerationConfig *struct {
		ResponseModalities []string           `json:"responseModalities"`
		SpeechConfig       geminiSpeechConfig `json:"speechConfig"`
	} `json:"generationConfig"`
}

// geminiServer fakes the Gemini API: it records requests and answers each
// with status and response.
type geminiServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []geminiRequest
}

func newGeminiServer(t *testing.T, status int, response string) *geminiServer {
	t.Helper()
	g := &geminiServer{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := geminiRequest{Path: r.URL.Path, Key: r.Header.Get("x-goog-api-key")}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.requests = append(g.requests, req)
		g.mu.Unlock()
		w.WriteHeader(status)
		if _, err := io.WriteString(w, response); err != nil {
			t.Logf("writing response: %v", err)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *geminiServer) URL() string { return g.srv.URL + "/v1beta" }

func (g *geminiServer) Requests() []geminiRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests
}

func textResponse(parts ...string) string {
	return `{"candidates":[{"content":{"role":"model","parts":[` + strings.Join(parts, ",") + `]},"finishReason":"STOP"}]}`
}

func audioResponse(mimeType string, data []byte) string {
	return textResponse(fmt.Sprintf(`{"inlineData":{"mimeType":%q,"data":%q}}`, mimeType, base64.StdEncoding.EncodeToString(data)))
}

const apiKeyInvalid = `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT",` +
	`"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID","domain":"googleapis.com"}]}}`

// tone returns d of a 440 Hz tone as 16-bit mono PCM at rate.
func tone(d time.Duration, rate int) []byte {
	n := int(d * time.Duration(rate) / time.Second)
	data := make([]byte, 2*n)
	for i := range n {
		s := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		binary.LittleEndian.PutUint16(data[2*i:], uint16(s))
	}
	return data
}

// toneWAV returns d of a 440 Hz tone as a WAV file at 24 kHz.
func toneWAV(d time.Duration) []byte {
	pcm := tone(d, 24000)
	data := make([]byte, 44, 44+len(pcm))
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(36+len(pcm)))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 24000)
	binary.LittleEndian.PutUint32(data[28:], 48000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(pcm)))
	return append(data, pcm...)
}

func TestGeminiTranscribe(t *testing.T) {
	t.Parallel()
	voiceNote := speech.Audio{MediaType: "audio/ogg", Data: ogg(14 * time.Second)}
	tests := []struct {
		name         string
		status       int
		response     string
		audio        speech.Audio
		want         string
		wantReason   string
		wantNoWords  bool
		wantMimeType string
	}{
		{
			name:         "voice note",
			status:       http.StatusOK,
			response:     textResponse(`{"text":" Fix the parser test. "}`),
			audio:        voiceNote,
			want:         "Fix the parser test.",
			wantMimeType: "audio/ogg",
		},
		{
			name:         "thoughts are left out",
			status:       http.StatusOK,
			response:     textResponse(`{"text":"The audio is speech.","thought":true}`, `{"text":"Fix it."}`),
			audio:        speech.Audio{MediaType: "audio/x-m4a", Data: []byte("\x00\x00\x00\x20ftypM4A ")},
			want:         "Fix it.",
			wantMimeType: "audio/m4a",
		},
		{
			name:         "no words",
			status:       http.StatusOK,
			response:     textResponse(`{"text":"\n"}`),
			audio:        speech.Audio{MediaType: "audio/wav", Data: wav(time.Second)},
			wantReason:   "the speech service heard no words",
			wantNoWords:  true,
			wantMimeType: "audio/wav",
		},
		{
			name:         "no speech",
			status:       http.StatusOK,
			response:     textResponse(`{"text":"NO_SPEECH\n"}`),
			audio:        voiceNote,
			wantReason:   "the speech service heard no words",
			wantNoWords:  true,
			wantMimeType: "audio/ogg",
		},
		{
			name:         "no speech with a period",
			status:       http.StatusOK,
			response:     textResponse(`{"text":"No_speech."}`),
			audio:        voiceNote,
			wantReason:   "the speech service heard no words",
			wantNoWords:  true,
			wantMimeType: "audio/ogg",
		},
		{
			name:         "words about no speech",
			status:       http.StatusOK,
			response:     textResponse(`{"text":"NO_SPEECH is the marker."}`),
			audio:        voiceNote,
			want:         "NO_SPEECH is the marker.",
			wantMimeType: "audio/ogg",
		},
		{
			name:       "refused key",
			status:     http.StatusBadRequest,
			response:   apiKeyInvalid,
			audio:      voiceNote,
			wantReason: "the speech service refused the key",
		},
		{
			name:       "forbidden",
			status:     http.StatusForbidden,
			response:   `{"error":{"code":403,"message":"Method doesn't allow unregistered callers.","status":"PERMISSION_DENIED"}}`,
			audio:      voiceNote,
			wantReason: "the speech service refused the key",
		},
		{
			name:       "quota",
			status:     http.StatusTooManyRequests,
			response:   `{"error":{"code":429,"message":"You exceeded your current quota.","status":"RESOURCE_EXHAUSTED"}}`,
			audio:      voiceNote,
			wantReason: "the speech service is over its rate limit or quota",
		},
		{
			name:       "blocked",
			status:     http.StatusOK,
			response:   `{"promptFeedback":{"blockReason":"SAFETY"}}`,
			audio:      voiceNote,
			wantReason: "the speech service blocked the request: SAFETY",
		},
		{
			name:       "invalid response",
			status:     http.StatusOK,
			response:   `{"candidates":`,
			audio:      voiceNote,
			wantReason: "the speech service sent an invalid response",
		},
		{
			name:       "too long",
			audio:      speech.Audio{MediaType: "audio/ogg", Data: ogg(6 * time.Minute)},
			wantReason: "the recording is longer than 5 minutes",
		},
		{
			name:       "too large",
			audio:      speech.Audio{MediaType: "audio/flac", Data: make([]byte, 15<<20)},
			wantReason: "the recording is over 14 MiB",
		},
		{
			name:       "unsupported type",
			audio:      speech.Audio{MediaType: "audio/midi", Data: []byte("MThd")},
			wantReason: "the speech service doesn't take audio/midi audio",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newGeminiServer(t, tc.status, tc.response)
			tr, _ := speech.NewGemini(srv.URL(), speech.Config{Service: speech.Gemini, Key: "gm-key"})
			got, err := tr.Transcribe(t.Context(), tc.audio)
			if tc.wantReason != "" {
				var e *speech.Error
				if !errors.As(err, &e) || e.Reason != tc.wantReason || e.NoWords != tc.wantNoWords {
					t.Fatalf("Transcribe() error = %#v, want reason %q, no words %v", err, tc.wantReason, tc.wantNoWords)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("Transcribe() = %q, %v, want %q, nil", got, err, tc.want)
			}
			if tc.wantMimeType == "" {
				return
			}
			reqs := srv.Requests()
			if len(reqs) != 1 {
				t.Fatalf("server got %d requests, want 1", len(reqs))
			}
			req := reqs[0]
			if req.Path != "/v1beta/models/gemini-3.5-flash-lite:generateContent" || req.Key != "gm-key" || req.GenerationConfig != nil {
				t.Fatalf("Transcribe() sent to %s with key %q and config %+v, want gemini-3.5-flash-lite with key gm-key and no config", req.Path, req.Key, req.GenerationConfig)
			}
			if len(req.Contents) != 1 || len(req.Contents[0].Parts) != 2 || !strings.Contains(req.Contents[0].Parts[0].Text, "Transcribe") {
				t.Fatalf("Transcribe() sent contents %+v, want the prompt and the audio", req.Contents)
			}
			want := &geminiBlob{MimeType: tc.wantMimeType, Data: tc.audio.Data}
			if diff := cmp.Diff(want, req.Contents[0].Parts[1].InlineData); diff != "" {
				t.Fatalf("Transcribe() wrong audio (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestGeminiSpeak(t *testing.T) {
	t.Parallel()
	const length = 1500 * time.Millisecond
	tests := []struct {
		name       string
		accepted   []string
		mimeType   string
		data       []byte
		wantType   string
		wantReason string
	}{
		{name: "ogg from wav", accepted: []string{"text/plain", "audio/ogg"}, mimeType: "audio/wav", data: toneWAV(length), wantType: "audio/ogg"},
		{name: "ogg from raw pcm", accepted: []string{"audio/opus"}, mimeType: "audio/L16;codec=pcm;rate=24000", data: tone(length, 24000), wantType: "audio/ogg"},
		{name: "any audio", accepted: []string{"audio/*"}, mimeType: "audio/wav", data: toneWAV(length), wantType: "audio/ogg"},
		{name: "wav", accepted: []string{"audio/mpeg", "audio/wav"}, mimeType: "audio/L16;codec=pcm;rate=24000", data: tone(length, 24000), wantType: "audio/wav"},
		{name: "mp3 only", accepted: []string{"audio/mpeg"}, wantReason: "the speech service can't speak in audio/mpeg"},
		{name: "no audio accepted", accepted: []string{"text/plain"}, wantReason: "the speech service can't speak in text/plain"},
		{name: "unknown audio", accepted: []string{"audio/ogg"}, mimeType: "audio/mpeg", data: mp3(3), wantReason: "the speech service sent audio the server can't read"},
		{name: "empty audio", accepted: []string{"audio/ogg"}, mimeType: "audio/wav", data: toneWAV(0), wantReason: "the speech service sent no audio"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newGeminiServer(t, http.StatusOK, audioResponse(tc.mimeType, tc.data))
			_, sp := speech.NewGemini(srv.URL(), speech.Config{Service: speech.Gemini, Key: "gm-key"})
			got, err := sp.Speak(t.Context(), "Done.", tc.accepted)
			if tc.wantReason != "" {
				if speech.Reason(err) != tc.wantReason {
					t.Fatalf("Speak() error = %v, want %q", err, tc.wantReason)
				}
				return
			}
			if err != nil {
				t.Fatalf("Speak() error = %v, want nil", err)
			}
			if got.MediaType != tc.wantType {
				t.Fatalf("Speak() media type = %q, want %q", got.MediaType, tc.wantType)
			}
			d, ok := speech.Duration(got)
			if !ok || d < length || d > length+40*time.Millisecond {
				t.Fatalf("Duration(Speak()) = %v, %v, want %v up to 40ms more", d, ok, length)
			}
			if tc.wantType == "audio/ogg" {
				checkOgg(t, got.Data, 120+uint64(length*48000/time.Second))
			}
			reqs := srv.Requests()
			if len(reqs) != 1 || reqs[0].Path != "/v1beta/models/gemini-3.8-flash-lite-tts:generateContent" || reqs[0].Key != "gm-key" {
				t.Fatalf("Speak() requests = %+v, want one of gemini-3.8-flash-lite-tts with key gm-key", reqs)
			}
			var cfg geminiSpeechConfig
			cfg.VoiceConfig.PrebuiltVoiceConfig.VoiceName = "Kore"
			if c := reqs[0].GenerationConfig; c == nil || cmp.Diff([]string{"AUDIO"}, c.ResponseModalities) != "" || c.SpeechConfig != cfg {
				t.Fatalf("Speak() config = %+v, want AUDIO in the voice Kore", c)
			}
			if p := reqs[0].Contents[0].Parts; len(p) != 1 || p[0].Text != "Done." {
				t.Fatalf("Speak() parts = %+v, want the text", p)
			}
		})
	}
}

// checkOgg reads data as Ogg Opus, which fails on a page with a wrong
// CRC, and checks its pre-skip and the granule position of its last page.
func checkOgg(t *testing.T, data []byte, wantGranule uint64) {
	t.Helper()
	r, head, err := oggreader.NewWith(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("oggreader.NewWith() error = %v, want nil", err)
	}
	if head.PreSkip != 120 || head.Channels != 1 {
		t.Fatalf("OpusHead pre-skip %d, channels %d, want 120, 1", head.PreSkip, head.Channels)
	}
	var granule uint64
	for {
		_, page, err := r.ParseNextPage()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ParseNextPage() error = %v, want nil", err)
		}
		if page.GranulePosition < granule {
			t.Fatalf("ParseNextPage() granule = %d after %d, want it to grow", page.GranulePosition, granule)
		}
		granule = page.GranulePosition
	}
	if granule != wantGranule {
		t.Fatalf("last granule position = %d, want %d", granule, wantGranule)
	}
}

func TestGeminiSpeakWithVoice(t *testing.T) {
	t.Parallel()
	srv := newGeminiServer(t, http.StatusOK, audioResponse("audio/wav", toneWAV(time.Second)))
	_, sp := speech.NewGemini(srv.URL(), speech.Config{Service: speech.Gemini, Key: "gm-key", Voice: "Puck"})
	if _, err := sp.Speak(t.Context(), "Done.", []string{"audio/ogg"}); err != nil {
		t.Fatalf("Speak() error = %v, want nil", err)
	}
	if got := srv.Requests()[0].GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName; got != "Puck" {
		t.Fatalf("Speak() voice = %q, want Puck", got)
	}
}

func TestGeminiBoundsResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		response string
		call     func(context.Context, speech.Transcriber, speech.Speaker) error
		want     string
	}{
		{
			name:     "transcript",
			response: textResponse(`{"text":"` + strings.Repeat("a", 2<<20) + `"}`),
			call: func(ctx context.Context, tr speech.Transcriber, _ speech.Speaker) error {
				_, err := tr.Transcribe(ctx, speech.Audio{MediaType: "audio/ogg", Data: ogg(time.Second)})
				return err
			},
			want: "the speech service sent over 1 MiB",
		},
		{
			name:     "speech",
			response: audioResponse("audio/L16;rate=24000", make([]byte, 17<<20)),
			call: func(ctx context.Context, _ speech.Transcriber, sp speech.Speaker) error {
				_, err := sp.Speak(ctx, "Done.", []string{"audio/ogg"})
				return err
			},
			want: "the speech service sent over 21 MiB",
		},
		{
			name:     "decoded speech",
			response: audioResponse("audio/L16;rate=24000", make([]byte, 16<<20+2)),
			call: func(ctx context.Context, _ speech.Transcriber, sp speech.Speaker) error {
				_, err := sp.Speak(ctx, "Done.", []string{"audio/ogg"})
				return err
			},
			want: "the speech service sent over 16 MiB",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newGeminiServer(t, http.StatusOK, tc.response)
			tr, sp := speech.NewGemini(srv.URL(), speech.Config{Service: speech.Gemini, Key: "gm-key"})
			if err := tc.call(t.Context(), tr, sp); speech.Reason(err) != tc.want {
				t.Fatalf("call error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestGeminiErrorsNameNoURLOrKey(t *testing.T) {
	t.Parallel()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)
	refusing := newGeminiServer(t, http.StatusBadRequest, apiKeyInvalid)
	echoing := newGeminiServer(t, http.StatusBadRequest, `{"error":{"code":400,"message":"Invalid request.","status":"INVALID_ARGUMENT"}}`)
	unreachable := unreachableURL(t)
	tests := []struct {
		name       string
		url        string
		timeout    time.Duration
		wantReason string
	}{
		{name: "timeout", url: slow.URL + "/v1beta", timeout: 50 * time.Millisecond, wantReason: "the speech service timed out"},
		{name: "refused", url: refusing.URL(), timeout: time.Minute, wantReason: "the speech service refused the key"},
		{name: "bad request", url: echoing.URL(), timeout: time.Minute, wantReason: "the speech service refused the request: Invalid request."},
		{name: "unreachable", url: unreachable + "/v1beta", timeout: time.Minute, wantReason: "couldn't reach the speech service"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr, sp := speech.NewGemini(tc.url, speech.Config{Service: speech.Gemini, Key: "gm-secret"})
			ctx, cancel := context.WithTimeout(t.Context(), tc.timeout)
			defer cancel()
			_, err := tr.Transcribe(ctx, speech.Audio{MediaType: "audio/ogg", Data: ogg(time.Second)})
			_, speakErr := sp.Speak(ctx, "Done.", []string{"audio/ogg"})
			for _, err := range []error{err, speakErr} {
				if err == nil || err.Error() != tc.wantReason {
					t.Fatalf("call error = %v, want %q", err, tc.wantReason)
				}
				for _, secret := range []string{tc.url, "gm-secret", "generateContent"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("call error = %q, which names %q", err, secret)
					}
				}
			}
		})
	}
}

func TestHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		service string
		want    string
	}{
		{service: "openai", want: "api.openai.com"},
		{service: "gemini", want: "generativelanguage.googleapis.com"},
		{service: "http://LOCALHOST:8000/v1", want: "localhost:8000"},
	}
	for _, tc := range tests {
		if got := speech.Host(tc.service); got != tc.want {
			t.Fatalf("Host(%q) = %q, want %q", tc.service, got, tc.want)
		}
	}
}
