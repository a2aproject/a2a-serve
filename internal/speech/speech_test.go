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
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/speech"
	"github.com/a2aproject/a2a-serve/internal/speech/speechtest"
)

func TestParseService(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value   string
		wantErr string
	}{
		{value: "openai"},
		{value: "http://localhost:8000/v1"},
		{value: "https://speech.example.com/v1"},
		{value: "gemini"},
		{value: "local", wantErr: "want openai, gemini or the http(s) URL"},
		{value: "localhost:8000", wantErr: "want openai, gemini or the http(s) URL"},
		{value: "ftp://localhost/v1", wantErr: "want openai, gemini or the http(s) URL"},
		{value: "https://user:key@speech.example.com/v1", wantErr: "--voice-key"},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			err := speech.ParseService(tc.value)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("ParseService(%q) error = %v, want nil", tc.value, err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("ParseService(%q) error = %v, want %q", tc.value, err, tc.wantErr)
			}
		})
	}
}

// ogg returns an Ogg Opus stream of d in 20 ms packets, whose last page
// claims granule.
func oggWithGranule(d time.Duration, granule int64) []byte {
	page := func(granule int64, packets [][]byte) []byte {
		p := make([]byte, 27)
		copy(p, "OggS")
		binary.LittleEndian.PutUint64(p[6:14], uint64(granule))
		p[26] = byte(len(packets))
		for _, pk := range packets {
			p = append(p, byte(len(pk)))
		}
		for _, pk := range packets {
			p = append(p, pk...)
		}
		return p
	}
	data := page(0, [][]byte{[]byte("OpusHead\x01\x01\x38\x01\x80\xbb\x00\x00\x00\x00\x00")})
	data = append(data, page(0, [][]byte{[]byte("OpusTags")})...)
	var audio [][]byte
	for range d / (20 * time.Millisecond) {
		audio = append(audio, []byte{0x08}) // SILK, 20 ms, one frame
	}
	for len(audio) > 0 {
		n := min(len(audio), 255)
		data = append(data, page(granule, audio[:n])...)
		audio = audio[n:]
	}
	return data
}

func ogg(d time.Duration) []byte { return oggWithGranule(d, int64(d/time.Second)*48000) }

// mp3 returns n MPEG-1 layer III frames at 128 kbit/s and 44.1 kHz, after
// an ID3v2 tag.
func mp3(n int) []byte {
	data := []byte("ID3\x04\x00\x00\x00\x00\x00\x02ab")
	for range n {
		frame := make([]byte, 417)
		copy(frame, []byte{0xFF, 0xFB, 0x90, 0x00})
		data = append(data, frame...)
	}
	return data
}

// wav returns a WAV file of d at 8 kHz, 8 bits, mono.
func wav(d time.Duration) []byte { return wavWithFormat(d, 1) }

func wavWithByteRate(d time.Duration, byteRate uint32) []byte {
	data := wav(d)
	binary.LittleEndian.PutUint32(data[28:], byteRate)
	return data
}

func wavWithFormat(d time.Duration, format uint16) []byte {
	n := int(d / time.Second * 8000)
	data := make([]byte, 44, 44+n)
	copy(data, "RIFF")
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], format)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 8000)
	binary.LittleEndian.PutUint32(data[28:], 8000)
	binary.LittleEndian.PutUint16(data[32:], 1)
	binary.LittleEndian.PutUint16(data[34:], 8)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(n))
	return append(data, make([]byte, n)...)
}

func TestDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		audio  speech.Audio
		want   time.Duration
		wantOK bool
	}{
		{name: "ogg opus", audio: speech.Audio{MediaType: "audio/ogg", Data: ogg(14 * time.Second)}, want: 14 * time.Second, wantOK: true},
		{name: "ogg opus whose granule claims less", audio: speech.Audio{MediaType: "audio/ogg", Data: oggWithGranule(6*time.Minute, 48000)}, want: 6 * time.Minute, wantOK: true},
		{name: "wav", audio: speech.Audio{MediaType: "audio/wav", Data: wav(3 * time.Second)}, want: 3 * time.Second, wantOK: true},
		{name: "mp3", audio: speech.Audio{MediaType: "audio/mpeg", Data: mp3(100)}, want: 100 * (1152 * time.Second / 44100), wantOK: true},
		{name: "mp3 without frames", audio: speech.Audio{MediaType: "audio/mpeg", Data: []byte("ID3\x03")}},
		{name: "truncated ogg", audio: speech.Audio{MediaType: "audio/ogg", Data: []byte("OggS")}},
		{name: "ogg with segments past its end", audio: speech.Audio{MediaType: "audio/ogg", Data: append([]byte("OggS"), append(make([]byte, 22), 200)...)}},
		{name: "wav whose byte rate claims more", audio: speech.Audio{MediaType: "audio/wav", Data: wavWithByteRate(3*time.Second, 0xFFFFFFFF)}, want: 3 * time.Second, wantOK: true},
		{name: "wav of an unknown format", audio: speech.Audio{MediaType: "audio/wav", Data: wavWithFormat(3*time.Second, 0x55)}},
		{name: "mp3 frame among other data", audio: speech.Audio{MediaType: "audio/mpeg", Data: append([]byte{0xFF, 0xF1, 0x50, 0x80}, append(make([]byte, 1000), mp3(1)[12:]...)...)}},
		{name: "mp3 with an id3v1 tag", audio: speech.Audio{MediaType: "audio/mpeg", Data: append(mp3(10), append([]byte("TAG"), make([]byte, 125)...)...)}, want: 10 * (1152 * time.Second / 44100), wantOK: true},
		{name: "m4a", audio: speech.Audio{MediaType: "audio/mp4", Data: []byte("\x00\x00\x00\x20ftypM4A ")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := speech.Duration(tc.audio)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("Duration() = %v, %v, want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func FuzzDuration(f *testing.F) {
	f.Add(ogg(time.Second))
	f.Add(wav(time.Second))
	f.Add(mp3(3))
	f.Add(wavWithByteRate(time.Second, 0xFFFFFFFF))
	f.Add(append([]byte("OggS"), append(make([]byte, 42), 200)...))
	f.Add(wavWithDataSize(time.Second, 0))
	f.Add(concat(mp3(3), apeTag(apeItems, true), mp3(3)[12:]))
	f.Add(concat(append(oggOpusStream(1, time.Second), oggPage(2, []byte{0x03, 0x00}))...))
	f.Fuzz(func(t *testing.T, data []byte) {
		speech.Duration(speech.Audio{Data: data})
	})
}

func TestTranscribe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		opts       speechtest.Options
		key        string
		audio      speech.Audio
		want       string
		wantReason string
		wantFile   string
	}{
		{
			name:     "voice note",
			opts:     speechtest.Options{Transcript: " Fix the parser test. "},
			audio:    speech.Audio{MediaType: "audio/ogg", Data: ogg(14 * time.Second)},
			want:     "Fix the parser test.",
			wantFile: "audio.ogg",
		},
		{
			name:     "media type with parameters",
			opts:     speechtest.Options{Transcript: "hi"},
			audio:    speech.Audio{MediaType: "audio/mpeg; rate=44100", Data: []byte("ID3")},
			want:     "hi",
			wantFile: "audio.mp3",
		},
		{
			name:       "refused key",
			opts:       speechtest.Options{Key: "right", Transcript: "hi"},
			key:        "wrong",
			audio:      speech.Audio{MediaType: "audio/ogg", Data: ogg(time.Second)},
			wantReason: "the speech service refused the key",
		},
		{
			name:       "too long",
			opts:       speechtest.Options{Transcript: "hi"},
			audio:      speech.Audio{MediaType: "audio/ogg", Data: ogg(6 * time.Minute)},
			wantReason: "the recording is longer than 5 minutes",
		},
		{
			name:       "unsupported type",
			opts:       speechtest.Options{Transcript: "hi"},
			audio:      speech.Audio{MediaType: "audio/midi", Data: []byte("MThd")},
			wantReason: "the speech service doesn't take audio/midi audio",
		},
		{
			name:       "silence",
			audio:      speech.Audio{MediaType: "audio/wav", Data: wav(time.Second)},
			wantReason: "the speech service heard no words",
			wantFile:   "audio.wav",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := speechtest.NewServer(t, tc.opts)
			tr, _, err := speech.New(speech.Config{Service: srv.URL(), Key: tc.key})
			if err != nil {
				t.Fatalf("New() error = %v, want nil", err)
			}
			got, err := tr.Transcribe(t.Context(), tc.audio)
			if tc.wantReason != "" {
				if speech.Reason(err) != tc.wantReason {
					t.Fatalf("Transcribe() error = %v, want %q", err, tc.wantReason)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("Transcribe() = %q, %v, want %q, nil", got, err, tc.want)
			}
			if tc.wantFile == "" {
				return
			}
			reqs := srv.Requests()
			if len(reqs) != 1 {
				t.Fatalf("server got %d requests, want 1", len(reqs))
			}
			want := speechtest.Request{Path: "/v1/audio/transcriptions", Model: "whisper-1", Format: "json", File: tc.audio.Data, Filename: tc.wantFile}
			if diff := cmp.Diff(want, reqs[0]); diff != "" {
				t.Fatalf("Transcribe() wrong request (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestSpeak(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		opts         speechtest.Options
		accepted     []string
		want         speech.Audio
		wantFormat   string
		wantReason   string
		wantNoSpeech bool
	}{
		{
			name:       "ogg",
			accepted:   []string{"text/plain", "audio/ogg"},
			want:       speech.Audio{MediaType: "audio/ogg", Data: speechtest.Speech},
			wantFormat: "opus",
		},
		{
			name:       "mp3",
			accepted:   []string{"audio/mpeg", "audio/ogg"},
			want:       speech.Audio{MediaType: "audio/mpeg", Data: speechtest.Speech},
			wantFormat: "mp3",
		},
		{
			name:       "any audio",
			accepted:   []string{"audio/*"},
			want:       speech.Audio{MediaType: "audio/ogg", Data: speechtest.Speech},
			wantFormat: "opus",
		},
		{
			name:       "no audio accepted",
			accepted:   []string{"text/plain"},
			wantReason: "the speech service can't speak in text/plain",
		},
		{
			name:         "service without speech",
			opts:         speechtest.Options{NoSpeech: true},
			accepted:     []string{"audio/ogg"},
			wantReason:   "the speech service doesn't speak",
			wantNoSpeech: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := speechtest.NewServer(t, tc.opts)
			_, sp, err := speech.New(speech.Config{Service: srv.URL()})
			if err != nil {
				t.Fatalf("New() error = %v, want nil", err)
			}
			got, err := sp.Speak(t.Context(), "Done.", tc.accepted)
			if tc.wantReason != "" {
				var e *speech.Error
				if !errors.As(err, &e) || e.Reason != tc.wantReason || e.NoSpeech != tc.wantNoSpeech {
					t.Fatalf("Speak() error = %#v, want reason %q, no speech %v", err, tc.wantReason, tc.wantNoSpeech)
				}
				return
			}
			if err != nil {
				t.Fatalf("Speak() error = %v, want nil", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("Speak() wrong result (-want +got) diff = %s", diff)
			}
			want := []speechtest.Request{{Path: "/v1/audio/speech", Model: "tts-1", Voice: "alloy", Format: tc.wantFormat, Input: "Done."}}
			if diff := cmp.Diff(want, srv.Requests()); diff != "" {
				t.Fatalf("Speak() wrong requests (-want +got) diff = %s", diff)
			}
		})
	}
}

// unreachableURL returns the URL of a listener that closes every connection.
// It stays open for the test, so a parallel test can't take its port.
func unreachableURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing the listener: %v", err)
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if err := c.Close(); err != nil {
				t.Logf("closing a connection: %v", err)
			}
		}
	}()
	return "http://" + ln.Addr().String()
}

func TestErrorsNameNoURLOrKey(t *testing.T) {
	t.Parallel()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server notices a closed connection only once the body is read.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)
	refusing := speechtest.NewServer(t, speechtest.Options{Key: "sk-right"})
	unreachable := unreachableURL(t)
	tests := []struct {
		name       string
		url        string
		timeout    time.Duration
		wantReason string
	}{
		{name: "timeout", url: slow.URL + "/v1", timeout: 50 * time.Millisecond, wantReason: "the speech service timed out"},
		{name: "refused", url: refusing.URL(), timeout: time.Minute, wantReason: "the speech service refused the key"},
		{name: "unreachable", url: unreachable + "/v1", timeout: time.Minute, wantReason: "couldn't reach the speech service"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr, _, err := speech.New(speech.Config{Service: tc.url, Key: "sk-secret"})
			if err != nil {
				t.Fatalf("New() error = %v, want nil", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), tc.timeout)
			defer cancel()
			_, err = tr.Transcribe(ctx, speech.Audio{MediaType: "audio/ogg", Data: ogg(time.Second)})
			if err == nil || err.Error() != tc.wantReason {
				t.Fatalf("Transcribe() error = %v, want %q", err, tc.wantReason)
			}
			for _, secret := range []string{tc.url, "sk-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("Transcribe() error = %q, which names %q", err, secret)
				}
			}
		})
	}
}

func TestTranscribeBoundsResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"text":"` + strings.Repeat("a", 2<<20) + `"}`)); err != nil {
			t.Logf("writing response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	tr, _, err := speech.New(speech.Config{Service: srv.URL})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	_, err = tr.Transcribe(t.Context(), speech.Audio{MediaType: "audio/ogg", Data: ogg(time.Second)})
	if want := "the speech service sent over 1 MiB"; speech.Reason(err) != want {
		t.Fatalf("Transcribe() error = %v, want %q", err, want)
	}
}

func TestAccepts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		accepted []string
		want     bool
	}{
		{accepted: []string{"audio/ogg"}, want: true},
		{accepted: []string{"text/plain", "audio/mpeg"}, want: true},
		{accepted: []string{"audio/*"}, want: true},
		{accepted: []string{"*/*"}},
		{accepted: []string{"text/plain"}},
		{},
	}
	for _, tc := range tests {
		if got := speech.Accepts(tc.accepted); got != tc.want {
			t.Fatalf("Accepts(%q) = %v, want %v", tc.accepted, got, tc.want)
		}
	}
}

func TestProse(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("This sentence is ten words long and then it ends. ", 30)
	tests := []struct {
		name     string
		markdown string
		want     string
	}{
		{
			name:     "plain",
			markdown: "The test raced on a shared buffer.",
			want:     "The test raced on a shared buffer.",
		},
		{
			name:     "code, tables and links",
			markdown: "I changed **parser_test.go**:\n\n```go\nbuf := new(bytes.Buffer)\n```\n\n| a | b |\n|---|---|\n\nSee [the docs](https://example.com) or https://example.com/x.\n- Ran `go test`",
			want:     "I changed parser_test.go: See the docs or. Ran go test.",
		},
		{
			name:     "long",
			markdown: long,
			want:     strings.TrimSpace(strings.Repeat("This sentence is ten words long and then it ends. ", 19)) + " The rest is in the text.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := speech.Prose(tc.markdown, speech.MaxSpoken); got != tc.want {
				t.Fatalf("Prose() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProseFitsTheLimit(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("Ünïcödé wörds without a full stop ", 300)
	for _, limit := range []int{speech.MaxSpoken, speech.MaxSpeechText} {
		got := speech.Prose(text, limit)
		if utf8.RuneCountInString(got) > limit || len(got) > speech.MaxSpeechText || !strings.HasSuffix(got, "The rest is in the text.") {
			t.Fatalf("Prose(%d) = %d characters, %d bytes, want at most %d characters and %d bytes, ending with the rest note", limit, utf8.RuneCountInString(got), len(got), limit, speech.MaxSpeechText)
		}
	}
}

func TestSpeakFilter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		chunks     []string
		want       string
		wantSpoken string
		wantFound  bool
	}{
		{name: "no block", chunks: []string{"Fixed ", "it."}, want: "Fixed it."},
		{name: "block", chunks: []string{"Long.\n<speak>Short.</speak>"}, want: "Long.\n", wantSpoken: "Short.", wantFound: true},
		{name: "tags split across chunks", chunks: []string{"Long.\n  <spe", "ak>Sho", "rt.</sp", "eak>", "\nEnd."}, want: "Long.\n\nEnd.", wantSpoken: "Short.", wantFound: true},
		{name: "closing tag inside a line", chunks: []string{"<speak> is the SSML root.\nIt ends with </speak>, and more text."}, want: "<speak> is the SSML root.\nIt ends with </speak>, and more text."},
		{name: "indented code", chunks: []string{"Example:\n\n    <speak>Hello</speak>\n"}, want: "Example:\n\n    <speak>Hello</speak>\n"},
		{name: "nested fences", chunks: []string{"````md\n```xml\n<speak>Hi</speak>\n```\n````\n<speak>Short.</speak>"}, want: "````md\n```xml\n<speak>Hi</speak>\n```\n````\n", wantSpoken: "Short.", wantFound: true},
		{name: "block past the limit is text", chunks: []string{"<speak>" + strings.Repeat("x", 20000)}, want: "<speak>" + strings.Repeat("x", 20000)},
		{name: "last block wins", chunks: []string{"<speak>One.</speak>\n<speak>Two.</speak>\nb"}, want: "\n\nb", wantSpoken: "Two.", wantFound: true},
		{name: "space after the closing tag", chunks: []string{"Long.\n<speak>Short.</speak> \t", "\nEnd."}, want: "Long.\n\nEnd.", wantSpoken: "Short.", wantFound: true},
		{name: "CRLF after the closing tag", chunks: []string{"Long.\r\n<speak>Short.</speak>\r\nEnd."}, want: "Long.\r\n\nEnd.", wantSpoken: "Short.", wantFound: true},
		{name: "space after the closing tag at the end", chunks: []string{"Long.\n<speak>Short.</speak> "}, want: "Long.\n", wantSpoken: "Short.", wantFound: true},
		{name: "unclosed block stays text", chunks: []string{"Long.\n<speak>Short"}, want: "Long.\n<speak>Short"},
		{name: "tag in a sentence", chunks: []string{"Wrap SSML in `<speak>`. Then call the API."}, want: "Wrap SSML in `<speak>`. Then call the API."},
		{name: "tag in a code fence", chunks: []string{"Use:\n``", "`xml\n<speak>Hi</speak>\n```\nDone."}, want: "Use:\n```xml\n<speak>Hi</speak>\n```\nDone."},
		{name: "block after a code fence", chunks: []string{"```\nx\n```\n<speak>Short.</speak>"}, want: "```\nx\n```\n", wantSpoken: "Short.", wantFound: true},
		{name: "text that looks like a tag start", chunks: []string{"a <", " b <sp", "an>"}, want: "a < b <span>"},
		{name: "non-ascii lines", chunks: []string{"Привет\nмир\n<speak>Да.</speak>"}, want: "Привет\nмир\n", wantSpoken: "Да.", wantFound: true},
		{name: "tag start at the end of the stream", chunks: []string{"x\n<spe"}, want: "x\n<spe"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var f speech.SpeakFilter
			var got strings.Builder
			for _, c := range tc.chunks {
				got.WriteString(f.Write(c))
			}
			got.WriteString(f.Flush())
			spoken, found := f.Spoken()
			if got.String() != tc.want || spoken != tc.wantSpoken || found != tc.wantFound {
				t.Fatalf("SpeakFilter = %q, spoken %q, %v, want %q, spoken %q, %v", got.String(), spoken, found, tc.want, tc.wantSpoken, tc.wantFound)
			}
		})
	}
}

func TestSpeakWithVoice(t *testing.T) {
	t.Parallel()
	srv := speechtest.NewServer(t, speechtest.Options{})
	_, sp, err := speech.New(speech.Config{Service: srv.URL(), Voice: "am_michael"})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if _, err := sp.Speak(t.Context(), "Done.", []string{"audio/ogg"}); err != nil {
		t.Fatalf("Speak() error = %v, want nil", err)
	}
	if got := srv.Requests()[0].Voice; got != "am_michael" {
		t.Fatalf("Speak() voice = %q, want am_michael", got)
	}
}

func TestSpeakRefusedRequestSaysWhy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write([]byte(`{"error":{"message":"Invalid value: 'robot'. Supported values are: 'alloy' and 'nova'."}}`)); err != nil {
			t.Logf("writing response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	_, sp, err := speech.New(speech.Config{Service: srv.URL, Voice: "robot"})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	_, err = sp.Speak(t.Context(), "Done.", []string{"audio/ogg"})
	if want := "the speech service refused the request: Invalid value: 'robot'. Supported values are: 'alloy' and 'nova'."; speech.Reason(err) != want {
		t.Fatalf("Speak() error = %v, want %q", err, want)
	}
}

func TestSpeakOggWhenServiceRefusesOpus(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var formats []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Format string `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		mu.Lock()
		formats = append(formats, req.Format)
		mu.Unlock()
		if req.Format != "wav" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			if _, err := w.Write([]byte(`{"detail":[{"type":"literal_error","loc":["body","response_format"],"msg":"Input should be 'mp3', 'flac', 'wav' or 'pcm'"}]}`)); err != nil {
				t.Errorf("writing response: %v", err)
			}
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		if _, err := w.Write(toneWAV(time.Second)); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	_, sp, err := speech.New(speech.Config{Service: srv.URL})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	for range 2 {
		got, err := sp.Speak(t.Context(), "Done.", []string{"audio/ogg"})
		if err != nil {
			t.Fatalf("Speak() error = %v, want nil", err)
		}
		if got.MediaType != speech.MediaOgg {
			t.Fatalf("Speak() media type = %q, want %q", got.MediaType, speech.MediaOgg)
		}
		if d, ok := speech.Duration(got); !ok || d < 900*time.Millisecond || d > 1100*time.Millisecond {
			t.Fatalf("Duration(Speak()) = %v, %v, want about 1s", d, ok)
		}
	}
	if diff := cmp.Diff([]string{"opus", "wav", "wav"}, formats); diff != "" {
		t.Fatalf("Speak() wrong response formats (-want +got) diff = %s", diff)
	}
}

func TestSpeakRefusedRequestSaysWhyInDetail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "detail string",
			body: `{"detail":"Model 'tts-1' is not installed locally."}`,
			want: "the speech service refused the request: Model 'tts-1' is not installed locally.",
		},
		{
			name: "detail list",
			body: `{"detail":[{"loc":["body","voice"],"msg":"Unknown voice 'robot'"}]}`,
			want: "the speech service refused the request: Unknown voice 'robot'",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				if _, err := w.Write([]byte(tc.body)); err != nil {
					t.Errorf("writing response: %v", err)
				}
			}))
			t.Cleanup(srv.Close)
			_, sp, err := speech.New(speech.Config{Service: srv.URL})
			if err != nil {
				t.Fatalf("New() error = %v, want nil", err)
			}
			_, err = sp.Speak(t.Context(), "Done.", []string{"audio/mpeg"})
			if speech.Reason(err) != tc.want {
				t.Fatalf("Speak() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func FuzzSpeakFilterIgnoresChunking(f *testing.F) {
	f.Add("Long.\n<speak>Short.</speak> End.", uint(9))
	f.Add("```\n<speak>x</speak>\n```\n<speak>y", uint(4))
	f.Fuzz(func(t *testing.T, text string, split uint) {
		run := func(chunks ...string) (string, string, bool) {
			var sf speech.SpeakFilter
			var out strings.Builder
			for _, c := range chunks {
				out.WriteString(sf.Write(c))
			}
			out.WriteString(sf.Flush())
			spoken, found := sf.Spoken()
			return out.String(), spoken, found
		}
		at := int(split % uint(len(text)+1))
		wantOut, wantSpoken, wantFound := run(text)
		gotOut, gotSpoken, gotFound := run(text[:at], text[at:])
		if gotOut != wantOut || gotSpoken != wantSpoken || gotFound != wantFound {
			t.Fatalf("SpeakFilter split at %d = %q, %q, %v, want %q, %q, %v", at, gotOut, gotSpoken, gotFound, wantOut, wantSpoken, wantFound)
		}
		if !strings.Contains(text, "<speak>") && wantOut != text {
			t.Fatalf("SpeakFilter(%q) = %q, want the text unchanged", text, wantOut)
		}
	})
}
