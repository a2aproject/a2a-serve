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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	geminiURL = "https://generativelanguage.googleapis.com/v1beta"
	// geminiTranscribeModel is a stable model that understands audio; a
	// prompt asks it for the transcript.
	geminiTranscribeModel = "gemini-3.5-flash-lite"
	geminiSpeakModel      = "gemini-3.8-flash-lite-tts"
	geminiVoice           = "Kore"
	// maxGeminiInput is the largest recording sent inline: base64 makes it
	// a third larger, and a request may have 20 MB.
	maxGeminiInput = 14 << 20
)

// noSpeech is what the model replies for a recording without words. The
// model makes up words for quiet recordings when asked for an empty reply.
const noSpeech = "NO_SPEECH"

const transcribePrompt = "Transcribe the speech in this recording word for word, in the language spoken. " +
	"Reply with the transcript only, without labels, timestamps or comments. " +
	"If the recording has no spoken words, reply with exactly " + noSpeech + "."

// geminiInputTypes are the media types the Gemini API takes for the audio
// types callers send.
var geminiInputTypes = map[string]string{
	"audio/ogg": "audio/ogg", "audio/opus": "audio/ogg", "audio/mpeg": "audio/mpeg", "audio/mp3": "audio/mp3",
	"audio/wav": "audio/wav", "audio/x-wav": "audio/wav", "audio/wave": "audio/wav", "audio/vnd.wave": "audio/wav",
	"audio/mp4": "audio/m4a", "audio/x-m4a": "audio/m4a", "audio/m4a": "audio/m4a", "audio/aac": "audio/aac",
	"audio/webm": "audio/webm", "audio/flac": "audio/flac", "audio/x-flac": "audio/flac", "audio/aiff": "audio/aiff",
}

// geminiClient calls the generateContent method of the Gemini API.
type geminiClient struct {
	base  string
	key   string
	voice string
	http  *http.Client
}

func newGeminiClient(base string, cfg Config) *geminiClient {
	c := &geminiClient{base: base, key: cfg.Key, voice: geminiVoice, http: &http.Client{Timeout: callTimeout}}
	if cfg.Voice != "" {
		c.voice = cfg.Voice
	}
	return c
}

type geminiRequest struct {
	Contents         []geminiContent `json:"contents"`
	GenerationConfig *geminiConfig   `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text       string      `json:"text,omitempty"`
	InlineData *geminiBlob `json:"inlineData,omitempty"`
	Thought    bool        `json:"thought,omitempty"`
}

type geminiBlob struct {
	MimeType string `json:"mimeType"`
	Data     []byte `json:"data"`
}

type geminiConfig struct {
	ResponseModalities []string            `json:"responseModalities,omitempty"`
	SpeechConfig       *geminiSpeechConfig `json:"speechConfig,omitempty"`
}

type geminiSpeechConfig struct {
	VoiceConfig struct {
		PrebuiltVoiceConfig struct {
			VoiceName string `json:"voiceName"`
		} `json:"prebuiltVoiceConfig"`
	} `json:"voiceConfig"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

// Transcribe implements [Transcriber].
func (c *geminiClient) Transcribe(ctx context.Context, a Audio) (string, error) {
	mediaType, ok := geminiInputTypes[lowerMediaType(a.MediaType)]
	if !ok {
		return "", &Error{Reason: fmt.Sprintf("the speech service doesn't take %s audio", a.MediaType)}
	}
	if err := checkRecording(a, maxGeminiInput); err != nil {
		return "", err
	}
	resp, err := c.generate(ctx, geminiTranscribeModel, geminiRequest{
		Contents: []geminiContent{{Role: "user", Parts: []geminiPart{
			{Text: transcribePrompt},
			{InlineData: &geminiBlob{MimeType: mediaType, Data: a.Data}},
		}}},
	}, maxTranscript)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		if !p.Thought {
			text.WriteString(p.Text)
		}
	}
	t := strings.TrimSpace(text.String())
	if t != "" && !strings.EqualFold(strings.TrimRight(t, "."), noSpeech) {
		return t, nil
	}
	return "", &Error{Reason: "the speech service heard no words", NoWords: true}
}

// Speak implements [Speaker]. It packages the PCM audio of the service as
// Ogg Opus, or as WAV when that is all accepted takes.
func (c *geminiClient) Speak(ctx context.Context, text string, accepted []string) (Audio, error) {
	mediaType, ok := pickGeminiFormat(accepted)
	if !ok {
		return Audio{}, &Error{Reason: "the speech service can't speak in " + strings.Join(accepted, ", ")}
	}
	if len(text) > MaxSpeechText {
		return Audio{}, &Error{Reason: fmt.Sprintf("the text is over %d bytes", MaxSpeechText)}
	}
	req := geminiRequest{
		Contents:         []geminiContent{{Role: "user", Parts: []geminiPart{{Text: text}}}},
		GenerationConfig: &geminiConfig{ResponseModalities: []string{"AUDIO"}, SpeechConfig: &geminiSpeechConfig{}},
	}
	req.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName = c.voice
	// The audio comes in base64 within JSON.
	resp, err := c.generate(ctx, geminiSpeakModel, req, int64(base64.StdEncoding.EncodedLen(maxSpeech))+64<<10)
	if err != nil {
		return Audio{}, err
	}
	var blob *geminiBlob
	for _, p := range resp.Candidates[0].Content.Parts {
		if p.InlineData != nil && len(p.InlineData.Data) > 0 {
			blob = p.InlineData
			break
		}
	}
	if blob == nil {
		return Audio{}, &Error{Reason: "the speech service sent no audio"}
	}
	if len(blob.Data) > maxSpeech {
		return Audio{}, &Error{Reason: fmt.Sprintf("the speech service sent over %d MiB", maxSpeech>>20)}
	}
	p, err := readPCM(blob.MimeType, blob.Data)
	if err != nil {
		return Audio{}, &Error{Reason: "the speech service sent audio the server can't read", err: err}
	}
	if len(p.samples) == 0 {
		return Audio{}, &Error{Reason: "the speech service sent no audio"}
	}
	data := p.wav()
	if mediaType == MediaOgg {
		if data, err = oggOpus(ctx, p); err != nil {
			return Audio{}, &Error{Reason: "couldn't encode the spoken reply", err: err}
		}
	}
	if len(data) > maxSpeech {
		return Audio{}, &Error{Reason: fmt.Sprintf("the speech service sent over %d MiB", maxSpeech>>20)}
	}
	return Audio{MediaType: mediaType, Data: data}, nil
}

// pickGeminiFormat returns the first media type of accepted that the
// Gemini client speaks: Ogg Opus or WAV.
func pickGeminiFormat(accepted []string) (string, bool) {
	for _, a := range accepted {
		if t, ok := pickFormat([]string{a}); ok && t != MediaMPEG {
			return t, true
		}
	}
	return "", false
}

// generate calls generateContent of model, and returns a response with at
// least one candidate.
func (c *geminiClient) generate(ctx context.Context, model string, req geminiRequest, limit int64) (*geminiResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/models/"+model+":generateContent", bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Reason: "the speech service URL is invalid"}
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-goog-api-key", c.key)
	data, err := send(ctx, c.http, r, limit)
	if err != nil {
		return nil, err
	}
	var resp geminiResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, &Error{Reason: "the speech service sent an invalid response", err: err}
	}
	if b := resp.PromptFeedback.BlockReason; b != "" {
		return nil, &Error{Reason: "the speech service blocked the request: " + Truncate(b, 40)}
	}
	if len(resp.Candidates) == 0 {
		return nil, &Error{Reason: "the speech service sent no answer"}
	}
	if f := resp.Candidates[0].FinishReason; f != "" && f != "STOP" && len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, &Error{Reason: "the speech service stopped: " + Truncate(f, 40)}
	}
	return &resp, nil
}
