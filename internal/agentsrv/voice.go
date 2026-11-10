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

package agentsrv

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/speech"
)

// voiceTurns transcribes the audio of a turn's message before the agent
// gets it, and speaks the reply of a completed turn to callers who accept
// audio. The agent sees only text; the task store keeps the audio.
type voiceTurns struct {
	transcriber speech.Transcriber
	// speaker is nil for a service that only transcribes.
	speaker speech.Speaker
	budget  *speechBudget
	// stopping ends when the server stops, which ends transcriptions.
	stopping context.Context
}

// newVoiceTurns returns the voice turns of a speech service. speaker may be
// nil. Transcriptions end when stopping does.
func newVoiceTurns(transcriber speech.Transcriber, speaker speech.Speaker, now func() time.Time, stopping context.Context) *voiceTurns {
	return &voiceTurns{transcriber: transcriber, speaker: speaker, budget: newSpeechBudget(now), stopping: stopping}
}

// maxAudioParts bounds the recordings of one message.
const maxAudioParts = 10

// turn runs the agent's turn with run on execCtx, with its recordings as
// text, and adds the transcript and the spoken reply to its events. The
// recordings are transcribed once the task exists, so that it shows as
// working and can be stopped meanwhile.
func (v *voiceTurns) turn(ctx context.Context, execCtx *a2asrv.ExecutorContext, run func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	accepted := acceptedModes(ctx)
	speak := v.speaker != nil && speech.Accepts(accepted)
	if audioParts(execCtx.Message) == 0 {
		if !speak {
			return run(ctx, execCtx)
		}
		return v.speaking(ctx, execCtx, run(ctx, execCtx), accepted)
	}
	return func(yield func(a2a.Event, error) bool) {
		created := execCtx.StoredTask == nil
		if created && !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		textCtx, transcript, err := v.transcribeUntilStop(ctx, execCtx)
		if err != nil {
			if v.stopping.Err() != nil {
				for ev, err := range failStopped(execCtx, true) {
					if !yield(ev, err) {
						return
					}
				}
				return
			}
			// A canceled task ends with the cancel.
			if ctx.Err() != nil {
				return
			}
			slog.Info("transcribing a recording", "task", execCtx.TaskID, "error", err)
			yield(transcriptionFailed(execCtx, err), nil)
			return
		}
		msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("🎤 \""+shownTranscript(transcript)+"\""))
		msg.Metadata = map[string]any{speech.TranscriptKey: transcript}
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, msg), nil) {
			return
		}
		events := run(ctx, textCtx)
		if speak {
			events = v.speaking(ctx, execCtx, events, accepted)
		}
		first := true
		for ev, err := range events {
			t, isTask := ev.(*a2a.Task)
			// The task was sent above, before the transcription.
			if first && created && isTask && err == nil && t.Status.State == a2a.TaskStateSubmitted {
				first = false
				continue
			}
			first = false
			if isTask {
				ev = withOriginalMessage(t, textCtx.Message, execCtx.Message)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// transcribeUntilStop is transcribe, which a stop of the server ends.
func (v *voiceTurns) transcribeUntilStop(ctx context.Context, execCtx *a2asrv.ExecutorContext) (*a2asrv.ExecutorContext, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(v.stopping, cancel)
	defer stop()
	return v.transcribe(ctx, execCtx)
}

// transcriptionFailed is the event that ends a turn whose recordings
// couldn't be transcribed. A reply to a question asks it again, so that the
// agent, which waits for the answer, still gets one.
func transcriptionFailed(execCtx *a2asrv.ExecutorContext, err error) a2a.Event {
	text := "I couldn't transcribe the voice note: " + speech.Reason(err) + "."
	if answersWaiting(execCtx) && execCtx.StoredTask.Status.Message != nil {
		return reask(execCtx, text+" The question still waits for an answer; send it again or type it.\n")
	}
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart(text+" Send it again or type it.")))
}

// maxShownTranscript is about how many characters of a transcript the
// status shows; its metadata has all of it.
const maxShownTranscript = 500

func shownTranscript(t string) string {
	r := []rune(t)
	if len(r) <= maxShownTranscript {
		return t
	}
	return string(r[:maxShownTranscript]) + "…"
}

// speaking adds the spoken reply before the event that completes the
// task.
func (v *voiceTurns) speaking(ctx context.Context, execCtx *a2asrv.ExecutorContext, events iter.Seq2[a2a.Event, error], accepted []string) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		var reply turnReply
		for ev, err := range events {
			if err == nil {
				reply.add(ev)
			}
			if err == nil && completes(ev) {
				if extra, ok := v.speak(ctx, execCtx, reply.String(), accepted); ok && !yield(extra, nil) {
					return
				}
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// audioParts counts the recordings of msg.
func audioParts(msg *a2a.Message) int {
	if msg == nil {
		return 0
	}
	n := 0
	for _, p := range msg.Parts {
		if isAudio(p) {
			n++
		}
	}
	return n
}

// transcribe returns execCtx with the audio parts of its message replaced
// by their transcripts, and the transcripts. It returns execCtx itself if
// the message has no audio.
func (v *voiceTurns) transcribe(ctx context.Context, execCtx *a2asrv.ExecutorContext) (*a2asrv.ExecutorContext, string, error) {
	n := audioParts(execCtx.Message)
	if n == 0 {
		return execCtx, "", nil
	}
	if n > maxAudioParts {
		return nil, "", &speech.Error{Reason: fmt.Sprintf("the message has %d recordings, more than %d", n, maxAudioParts)}
	}
	msg := *execCtx.Message
	msg.Parts = slices.Clone(execCtx.Message.Parts)
	var transcripts []string
	for i, p := range msg.Parts {
		if !isAudio(p) {
			continue
		}
		audio := speech.Audio{MediaType: p.MediaType, Data: p.Raw()}
		// Only the length of these formats bounds what a recording costs.
		if _, ok := speech.Duration(audio); !ok && !callerIsOwner(ctx) {
			return nil, "", &speech.Error{Reason: "the recording isn't Ogg Opus, MP3 or WAV"}
		}
		if err := v.budget.take(ctx); err != nil {
			return nil, "", err
		}
		text, err := v.transcriber.Transcribe(ctx, audio)
		if err != nil {
			return nil, "", err
		}
		msg.Parts[i] = a2a.NewTextPart(text)
		transcripts = append(transcripts, text)
	}
	out := *execCtx
	out.Message = &msg
	return &out, strings.Join(transcripts, " "), nil
}

// speak returns the event that adds the spoken reply to the task, or a note
// that it failed. It reports false when there is nothing to speak.
func (v *voiceTurns) speak(ctx context.Context, execCtx *a2asrv.ExecutorContext, reply string, accepted []string) (a2a.Event, bool) {
	text := speech.Prose(reply)
	if text == "" {
		return nil, false
	}
	audio, err := v.say(ctx, text, accepted)
	if err != nil {
		slog.Info("speaking a reply", "task", execCtx.TaskID, "error", err)
		note := fmt.Sprintf("(The voice reply failed: %s.)", speech.Reason(err))
		return a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(note)), true
	}
	part := a2a.NewRawPart(audio.Data)
	part.MediaType = audio.MediaType
	part.Filename = "reply" + audioExtension(audio.MediaType)
	part.Metadata = map[string]any{speech.VoiceReplyKey: true}
	ev := a2a.NewArtifactEvent(execCtx, part)
	ev.Artifact.Name = "Voice reply"
	ev.LastChunk = true
	return ev, true
}

func (v *voiceTurns) say(ctx context.Context, text string, accepted []string) (speech.Audio, error) {
	if err := v.budget.take(ctx); err != nil {
		return speech.Audio{}, err
	}
	return v.speaker.Speak(ctx, text, accepted)
}

func audioExtension(mediaType string) string {
	switch mediaType {
	case speech.MediaOgg:
		return ".ogg"
	case speech.MediaMPEG:
		return ".mp3"
	case speech.MediaWAV:
		return ".wav"
	}
	return ""
}

// isAudio reports whether p is a recording sent with the message.
func isAudio(p *a2a.Part) bool {
	return p != nil && len(p.Raw()) > 0 && strings.HasPrefix(strings.ToLower(p.MediaType), "audio/")
}

// completes reports whether ev completes its task.
func completes(ev a2a.Event) bool {
	state, ok := eventState(ev)
	return ok && state == a2a.TaskStateCompleted
}

// turnReply collects the text of the artifacts of a turn, as chats show
// it.
type turnReply struct {
	ids   []a2a.ArtifactID
	texts map[a2a.ArtifactID]*strings.Builder
}

func (r *turnReply) add(ev a2a.Event) {
	switch e := ev.(type) {
	case *a2a.TaskArtifactUpdateEvent:
		r.artifact(e.Artifact, e.Append)
	case *a2a.Task:
		for _, a := range e.Artifacts {
			r.artifact(a, false)
		}
	}
}

func (r *turnReply) artifact(a *a2a.Artifact, appendTo bool) {
	if a == nil {
		return
	}
	if r.texts == nil {
		r.texts = map[a2a.ArtifactID]*strings.Builder{}
	}
	b, ok := r.texts[a.ID]
	if !ok {
		b = &strings.Builder{}
		r.texts[a.ID] = b
		r.ids = append(r.ids, a.ID)
	} else if !appendTo {
		b.Reset()
	}
	for _, p := range a.Parts {
		// The reply is cut for speech anyway, so the rest isn't kept.
		if b.Len() < 4*speech.MaxSpeechText {
			b.WriteString(p.Text())
		}
	}
}

func (r *turnReply) String() string {
	var parts []string
	for _, id := range r.ids {
		if t := r.texts[id].String(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// acceptedModesKey is the context key of the acceptedOutputModes of a
// request.
type acceptedModesKey struct{}

// withAcceptedModes returns ctx with the output modes a request accepts.
func withAcceptedModes(ctx context.Context, modes []string) context.Context {
	if len(modes) == 0 {
		return ctx
	}
	return context.WithValue(ctx, acceptedModesKey{}, slices.Clone(modes))
}

// acceptedModes returns the output modes the request of ctx accepts.
func acceptedModes(ctx context.Context) []string {
	modes, _ := ctx.Value(acceptedModesKey{}).([]string)
	return modes
}

// Bounds of the speech calls of one caller.
const (
	speechCallsPerHour = 60
	// maxSpeechCallers bounds the callers counted at once.
	maxSpeechCallers = 1000
)

// speechBudget limits the speech calls of each caller other than owners,
// so that one caller can't use up the speech service or the owner's
// quota.
type speechBudget struct {
	now func() time.Time

	mu      sync.Mutex
	windows map[string]*speechWindow
}

type speechWindow struct {
	start time.Time
	calls int
}

func newSpeechBudget(now func() time.Time) *speechBudget {
	return &speechBudget{now: now, windows: map[string]*speechWindow{}}
}

// callerIsOwner reports whether the caller of ctx is an owner.
func callerIsOwner(ctx context.Context) bool {
	p, ok := auth.Caller(ctx)
	return ok && p.IsOwner()
}

// take counts a call of the caller of ctx, or refuses it.
func (b *speechBudget) take(ctx context.Context) error {
	if callerIsOwner(ctx) {
		return nil
	}
	p, _ := auth.Caller(ctx)
	id := p.ID
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	w, ok := b.windows[id]
	if ok && now.Sub(w.start) >= time.Hour {
		delete(b.windows, id)
		ok = false
	}
	if !ok {
		if len(b.windows) >= maxSpeechCallers {
			b.prune(now)
		}
		if len(b.windows) >= maxSpeechCallers {
			return &speech.Error{Reason: "too many people use voice right now; try again later"}
		}
		w = &speechWindow{start: now}
		b.windows[id] = w
	}
	if w.calls >= speechCallsPerHour {
		return &speech.Error{Reason: fmt.Sprintf("you reached the limit of %d speech calls an hour", speechCallsPerHour)}
	}
	w.calls++
	return nil
}

// prune removes the windows that ended.
func (b *speechBudget) prune(now time.Time) {
	for id, w := range b.windows {
		if now.Sub(w.start) >= time.Hour {
			delete(b.windows, id)
		}
	}
}

// buildVoice returns the voice turns of the configured speech service, or
// nil when voice isn't set up.
func (s *Server) buildVoice(ctx context.Context, now func() time.Time) (*voiceTurns, error) {
	v := s.rt.Config().Voice
	if !v.On() {
		return nil, nil
	}
	key, err := s.rt.Secrets().Resolve(v.Key)
	if err != nil {
		return nil, fmt.Errorf("--voice-key: %w", err)
	}
	transcriber, speaker, err := speech.New(speech.Config{Service: v.Service, Key: key})
	if err != nil {
		return nil, fmt.Errorf("--voice: %w", err)
	}
	s.logf("✓ voice    %s", speech.Describe(v.Service))
	return newVoiceTurns(transcriber, speaker, now, ctx), nil
}
