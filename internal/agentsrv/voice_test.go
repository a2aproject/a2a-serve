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
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/messenger/messengertest"
	"github.com/a2aproject/a2a-serve/internal/speech"
	"github.com/a2aproject/a2a-serve/internal/speech/speechtest"
)

// scriptedTurn is an agent turn that records the message it got and
// replies with a text artifact, as a2aacp does for a new task.
type scriptedTurn struct {
	reply string
	state a2a.TaskState
	got   *a2a.Message
	calls int
}

func (s *scriptedTurn) run(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	s.calls++
	s.got = execCtx.Message
	state := s.state
	if state == "" {
		state = a2a.TaskStateCompleted
	}
	return eventsOf(
		a2a.NewSubmittedTask(execCtx, execCtx.Message),
		a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil),
		a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(s.reply)),
		a2a.NewStatusUpdateEvent(execCtx, state, nil),
	)
}

// describeVoice renders events as kind:detail lines.
func describeVoice(evs []a2a.Event) []string {
	var out []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case *a2a.Task:
			out = append(out, "task:"+stateWord(e.Status.State))
		case *a2a.TaskStatusUpdateEvent:
			line := "status:" + stateWord(e.Status.State)
			if e.Status.Message != nil {
				var texts []string
				for _, p := range e.Status.Message.Parts {
					texts = append(texts, p.Text())
				}
				line += ":" + strings.Join(texts, "")
			}
			out = append(out, line)
		case *a2a.TaskArtifactUpdateEvent:
			for _, p := range e.Artifact.Parts {
				if raw := p.Raw(); len(raw) > 0 {
					out = append(out, fmt.Sprintf("artifact:%s %s %s", p.Filename, p.MediaType, raw))
					continue
				}
				out = append(out, "artifact:"+p.Text())
			}
		}
	}
	return out
}

func stateWord(s a2a.TaskState) string {
	return strings.ToLower(strings.TrimPrefix(string(s), "TASK_STATE_"))
}

// voiceNote is an Ogg Opus recording of 20 ms.
func voiceNote() *a2a.Part {
	page := func(packet string) string {
		return "OggS\x00\x00" + strings.Repeat("\x00", 20) + "\x01" + string(rune(len(packet))) + packet
	}
	p := a2a.NewRawPart([]byte(page("OpusHead\x01\x01\x38\x01\x80\xbb\x00\x00\x00\x00\x00") + page("OpusTags") + page("\x08")))
	p.MediaType, p.Filename = "audio/ogg", "voice.ogg"
	return p
}

func m4aNote() *a2a.Part {
	p := a2a.NewRawPart([]byte("\x00\x00\x00\x20ftypM4A "))
	p.MediaType, p.Filename = "audio/mp4", "memo.m4a"
	return p
}

func TestVoiceTurn(t *testing.T) {
	t.Parallel()
	member := auth.Principal{ID: "pairing:anna", Role: auth.RoleMember}
	tests := []struct {
		name      string
		fake      *speechtest.Fake
		noSpeaker bool
		parts     []*a2a.Part
		accepted  []string
		want      []string
		wantAgent []string
		wantSpoke []string
	}{
		{
			name:      "voice note becomes text",
			fake:      &speechtest.Fake{Transcript: "Fix the flaky parser test."},
			parts:     []*a2a.Part{voiceNote()},
			want:      []string{"task:submitted", `status:working:🎤 "Fix the flaky parser test."`, "status:working", "artifact:Fixed it.", "status:completed"},
			wantAgent: []string{"Fix the flaky parser test."},
		},
		{
			name:      "voice note with a photo",
			fake:      &speechtest.Fake{Transcript: "What does this error mean?"},
			parts:     []*a2a.Part{voiceNote(), a2a.NewTextPart("see")},
			want:      []string{"task:submitted", `status:working:🎤 "What does this error mean?"`, "status:working", "artifact:Fixed it.", "status:completed"},
			wantAgent: []string{"What does this error mean?", "see"},
		},
		{
			name:  "failed transcription never reaches the agent",
			fake:  &speechtest.Fake{TranscribeErr: &speech.Error{Reason: "the speech service timed out"}},
			parts: []*a2a.Part{voiceNote()},
			want:  []string{"task:submitted", "status:failed:I couldn't transcribe the voice note: the speech service timed out. Send it again or type it."},
		},
		{
			name:  "unmeasured format from a member",
			fake:  &speechtest.Fake{Transcript: "hi"},
			parts: []*a2a.Part{m4aNote()},
			want:  []string{"task:submitted", "status:failed:I couldn't transcribe the voice note: the recording isn't Ogg Opus, MP3 or WAV. Send it again or type it."},
		},
		{
			name:      "reply spoken when accepted",
			fake:      &speechtest.Fake{},
			parts:     []*a2a.Part{a2a.NewTextPart("what changed?")},
			accepted:  []string{"audio/ogg"},
			want:      []string{"task:submitted", "status:working", "artifact:Fixed it.", "artifact:reply.ogg audio/ogg OggS", "status:completed"},
			wantAgent: []string{speakLinePrefix, "what changed?"},
			wantSpoke: []string{"Fixed it."},
		},
		{
			name:      "reply not spoken unless accepted",
			fake:      &speechtest.Fake{},
			parts:     []*a2a.Part{a2a.NewTextPart("what changed?")},
			accepted:  []string{"text/plain"},
			want:      []string{"task:submitted", "status:working", "artifact:Fixed it.", "status:completed"},
			wantAgent: []string{"what changed?"},
		},
		{
			name:      "service without speech",
			fake:      &speechtest.Fake{},
			noSpeaker: true,
			parts:     []*a2a.Part{a2a.NewTextPart("what changed?")},
			accepted:  []string{"audio/ogg"},
			want:      []string{"task:submitted", "status:working", "artifact:Fixed it.", "status:completed"},
			wantAgent: []string{"what changed?"},
		},
		{
			name:      "failed speech still completes",
			fake:      &speechtest.Fake{SpeakErr: &speech.Error{Reason: "the speech service refused the key"}},
			parts:     []*a2a.Part{a2a.NewTextPart("what changed?")},
			accepted:  []string{"audio/ogg"},
			want:      []string{"task:submitted", "status:working", "artifact:Fixed it.", "artifact:(The voice reply failed: the speech service refused the key.)", "status:completed"},
			wantAgent: []string{speakLinePrefix, "what changed?"},
			wantSpoke: []string{"Fixed it."},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var speaker speech.Speaker = tc.fake
			if tc.noSpeaker {
				speaker = nil
			}
			v := newVoiceTurns(tc.fake, speaker, time.Now, context.Background())
			msg := a2a.NewMessage(a2a.MessageRoleUser, tc.parts...)
			execCtx := &a2asrv.ExecutorContext{Message: msg, TaskID: "t1", ContextID: "c1"}
			agent := &scriptedTurn{reply: "Fixed it."}
			ctx := withAcceptedModes(auth.WithPrincipal(t.Context(), member), tc.accepted)
			var got []a2a.Event
			for ev, err := range v.turn(ctx, execCtx, agent.run) {
				if err != nil {
					t.Fatalf("voiceTurns.turn() error = %v", err)
				}
				got = append(got, ev)
			}
			if diff := cmp.Diff(tc.want, describeVoice(got)); diff != "" {
				t.Fatalf("voiceTurns.turn() wrong events (-want +got) diff = %s", diff)
			}
			var gotAgent []string
			if agent.got != nil {
				for _, p := range agent.got.Parts {
					text := p.Text()
					if strings.HasPrefix(text, speakLinePrefix) {
						text = speakLinePrefix
					}
					gotAgent = append(gotAgent, text)
				}
			}
			if diff := cmp.Diff(tc.wantAgent, gotAgent); diff != "" {
				t.Fatalf("voiceTurns.turn() wrong agent message (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantSpoke, tc.fake.Spoken()); diff != "" {
				t.Fatalf("voiceTurns.turn() wrong spoken texts (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestVoiceTurnKeepsAudioInTaskHistory(t *testing.T) {
	t.Parallel()
	v := newVoiceTurns(&speechtest.Fake{Transcript: "hello"}, nil, time.Now, context.Background())
	msg := a2a.NewMessage(a2a.MessageRoleUser, voiceNote())
	execCtx := &a2asrv.ExecutorContext{Message: msg, TaskID: "t1", ContextID: "c1"}
	agent := &scriptedTurn{reply: "hi"}
	for ev, err := range v.turn(auth.WithPrincipal(t.Context(), auth.Owner()), execCtx, agent.run) {
		if err != nil {
			t.Fatalf("voiceTurns.turn() error = %v", err)
		}
		task, ok := ev.(*a2a.Task)
		if !ok {
			continue
		}
		if len(task.History) != 1 || task.History[0] != msg {
			t.Fatalf("voiceTurns.turn() task history = %v, want the message with the voice note", task.History)
		}
	}
	if agent.calls != 1 {
		t.Fatalf("agent turns = %d, want 1", agent.calls)
	}
	if got := msg.Parts[0].MediaType; got != "audio/ogg" {
		t.Fatalf("message part media type = %q after the turn, want audio/ogg", got)
	}
}

func TestVoiceTurnFailsTaskWhenServerStops(t *testing.T) {
	t.Parallel()
	stopping, stop := context.WithCancel(t.Context())
	fake := &blockingTranscriber{started: make(chan struct{})}
	v := newVoiceTurns(fake, nil, time.Now, stopping)
	execCtx := &a2asrv.ExecutorContext{Message: a2a.NewMessage(a2a.MessageRoleUser, voiceNote()), TaskID: "t1", ContextID: "c1"}
	go func() {
		<-fake.started
		stop()
	}()
	var got []a2a.Event
	for ev, err := range v.turn(auth.WithPrincipal(t.Context(), auth.Owner()), execCtx, (&scriptedTurn{}).run) {
		if err != nil {
			t.Fatalf("voiceTurns.turn() error = %v", err)
		}
		got = append(got, ev)
	}
	want := []string{"task:submitted", "status:failed:" + stoppedMessage}
	if diff := cmp.Diff(want, describeVoice(got)); diff != "" {
		t.Fatalf("voiceTurns.turn() wrong events (-want +got) diff = %s", diff)
	}
}

// blockingTranscriber transcribes until its context ends.
type blockingTranscriber struct{ started chan struct{} }

func (b *blockingTranscriber) Transcribe(ctx context.Context, _ speech.Audio) (string, error) {
	close(b.started)
	<-ctx.Done()
	return "", &speech.Error{Reason: "the call of the speech service was canceled"}
}

func TestSpeechBudget(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	b := newSpeechBudget(func() time.Time { return now })
	member := auth.WithPrincipal(t.Context(), auth.Principal{ID: "pairing:anna", Role: auth.RoleMember})
	owner := auth.WithPrincipal(t.Context(), auth.Owner())
	for i := range speechCallsPerHour {
		if err := b.take(member); err != nil {
			t.Fatalf("speechBudget.take() call %d error = %v, want nil", i, err)
		}
	}
	var e *speech.Error
	if err := b.take(member); !errors.As(err, &e) {
		t.Fatalf("speechBudget.take() over the limit error = %v, want a speech error", err)
	}
	for range speechCallsPerHour + 1 {
		if err := b.take(owner); err != nil {
			t.Fatalf("speechBudget.take() for the owner error = %v, want nil", err)
		}
	}
	now = now.Add(time.Hour)
	if err := b.take(member); err != nil {
		t.Fatalf("speechBudget.take() an hour later error = %v, want nil", err)
	}
}

func TestChatVoiceTurn(t *testing.T) {
	t.Parallel()
	svc := speechtest.NewServer(t, speechtest.Options{Key: "sk-voice", Transcript: "scheduled check"})
	ts := startServer(t, serverOpts{
		messenger: messengertest.NewMessenger("fake"),
		secrets:   agentdir.Secrets{"VOICE_API_KEY": "sk-voice"},
		configure: func(c *agentdir.Config) {
			c.Voice = agentdir.VoiceConfig{Service: svc.URL(), Key: "secret:VOICE_API_KEY"}
		},
	})
	linkOwner(t, ts, "telegram:1", "dm1")

	note := ts.ch.Message("telegram:1", "dm1", "")
	note.Files = []messenger.File{{Name: "voice.ogg", MediaType: "audio/ogg", Data: []byte("OggS voice")}}
	ts.ch.Deliver(note)
	_, transcript := ts.ch.WaitFor(t, "dm1", messengertest.HasText(`🎤 "scheduled check"`))
	if transcript.ReplyTo.ID != note.MessageID {
		t.Fatalf("transcript replies to %q, want the voice note %q", transcript.ReplyTo.ID, note.MessageID)
	}
	ts.ch.WaitFor(t, "dm1", messengertest.HasText("ran scheduled check"))

	ts.ch.Write("telegram:1", "dm1", "/voice on")
	ts.ch.WaitFor(t, "dm1", messengertest.HasText("Voice replies are on."))
	ts.ch.Write("telegram:1", "dm1", "speak")
	_, long := ts.ch.WaitFor(t, "dm1", messengertest.HasText("The long answer, with details."))
	if strings.Contains(long.Text, "speak") || strings.Contains(long.Text, "short answer") {
		t.Fatalf("reply text = %q, want it without the <speak> block", long.Text)
	}
	ts.ch.WaitFor(t, "dm1", func(m messenger.Outbound) bool { return len(m.Files) > 0 })
	ts.ch.Write("telegram:1", "dm1", "scheduled again")
	ts.ch.WaitFor(t, "dm1", messengertest.HasText("ran scheduled again"))
	deadline := time.Now().Add(10 * time.Second)
	for {
		var voices [][]messenger.File
		for _, m := range ts.ch.Transcript() {
			if len(m.Files) > 0 {
				voices = append(voices, m.Files)
			}
		}
		if len(voices) == 2 {
			want := []messenger.File{{Name: "reply.ogg", MediaType: "audio/ogg", Data: speechtest.Speech, Voice: true}}
			if diff := cmp.Diff(want, voices[1]); diff != "" {
				t.Fatalf("voice reply wrong files (-want +got) diff = %s", diff)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("voice replies = %d, want 2", len(voices))
		}
		time.Sleep(10 * time.Millisecond)
	}
	var spoken []string
	for _, r := range svc.Requests() {
		if r.Path == "/v1/audio/speech" {
			spoken = append(spoken, r.Input)
		}
	}
	if diff := cmp.Diff([]string{"The short answer.", "ran scheduled again."}, spoken); diff != "" {
		t.Fatalf("spoken texts wrong result (-want +got) diff = %s", diff)
	}
}

// chunkedTurn is an agent turn that streams its reply in chunks and
// records the message it got.
type chunkedTurn struct {
	chunks []string
	got    *a2a.Message
}

func (c *chunkedTurn) run(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	c.got = execCtx.Message
	evs := []a2a.Event{a2a.NewSubmittedTask(execCtx, execCtx.Message)}
	first := a2a.NewArtifactEvent(execCtx, a2a.NewTextPart(c.chunks[0]))
	evs = append(evs, first)
	for _, chunk := range c.chunks[1:] {
		evs = append(evs, a2a.NewArtifactUpdateEvent(execCtx, first.Artifact.ID, a2a.NewTextPart(chunk)))
	}
	return eventsOf(append(evs, a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil))...)
}

func TestVoiceTurnSpeaksTheSpeakBlock(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		accepted  []string
		stored    *a2a.Task
		chunks    []string
		want      []string
		wantSpoke []string
		wantLine  bool
	}{
		{
			name:      "block split across chunks",
			accepted:  []string{"audio/ogg"},
			chunks:    []string{"Long answer.\n<spe", "ak>Short answer.</sp", "eak>"},
			want:      []string{"task:submitted", "artifact:Long answer.\n", "artifact:", "artifact:", "artifact:reply.ogg audio/ogg OggS", "status:completed"},
			wantSpoke: []string{"Short answer."},
			wantLine:  true,
		},
		{
			name:      "no block speaks the start",
			accepted:  []string{"audio/ogg"},
			chunks:    []string{"Long answer."},
			want:      []string{"task:submitted", "artifact:Long answer.", "artifact:reply.ogg audio/ogg OggS", "status:completed"},
			wantSpoke: []string{"Long answer."},
			wantLine:  true,
		},
		{
			name:   "block without voice reply is still removed",
			chunks: []string{"Long answer.\n<speak>Short.</speak>"},
			want:   []string{"task:submitted", "artifact:Long answer.\n", "status:completed"},
		},
		{
			name:   "block alone without voice reply is shown",
			chunks: []string{"<speak>Yes, the build passes.</speak>"},
			want:   []string{"task:submitted", "artifact:", "artifact:Yes, the build passes.", "status:completed"},
		},
		{
			name:      "block in code is kept",
			accepted:  []string{"audio/ogg"},
			chunks:    []string{"Wrap it in `<speak>` tags."},
			want:      []string{"task:submitted", "artifact:Wrap it in `<speak>` tags.", "artifact:reply.ogg audio/ogg OggS", "status:completed"},
			wantSpoke: []string{"Wrap it in <speak> tags."},
			wantLine:  true,
		},
		{
			name:      "unfinished tag start is shown at the end",
			accepted:  []string{"audio/ogg"},
			chunks:    []string{"Compare:\n<sp"},
			want:      []string{"task:submitted", "artifact:Compare:\n", "artifact:<sp", "artifact:reply.ogg audio/ogg OggS", "status:completed"},
			wantSpoke: []string{"Compare: <sp."},
			wantLine:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &speechtest.Fake{}
			v := newVoiceTurns(fake, fake, time.Now, context.Background())
			execCtx := &a2asrv.ExecutorContext{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("why?")), TaskID: "t1", ContextID: "c1", StoredTask: tc.stored}
			agent := &chunkedTurn{chunks: tc.chunks}
			ctx := withAcceptedModes(auth.WithPrincipal(t.Context(), auth.Owner()), tc.accepted)
			var got []a2a.Event
			for ev, err := range v.turn(ctx, execCtx, agent.run) {
				if err != nil {
					t.Fatalf("voiceTurns.turn() error = %v", err)
				}
				got = append(got, ev)
			}
			if diff := cmp.Diff(tc.want, describeVoice(got)); diff != "" {
				t.Fatalf("voiceTurns.turn() wrong events (-want +got) diff = %s", diff)
			}
			if diff := cmp.Diff(tc.wantSpoke, fake.Spoken()); diff != "" {
				t.Fatalf("voiceTurns.turn() wrong spoken texts (-want +got) diff = %s", diff)
			}
			gotLine := strings.HasPrefix(agent.got.Parts[0].Text(), speakLinePrefix)
			if gotLine != tc.wantLine {
				t.Fatalf("agent message has the speak line = %v, want %v", gotLine, tc.wantLine)
			}
		})
	}
}

func TestVoiceTurnShowsBlockWhenSpeechFails(t *testing.T) {
	t.Parallel()
	fake := &speechtest.Fake{SpeakErr: &speech.Error{Reason: "the speech service timed out"}}
	v := newVoiceTurns(fake, fake, time.Now, context.Background())
	execCtx := &a2asrv.ExecutorContext{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("does it build?")), TaskID: "t1", ContextID: "c1"}
	agent := &chunkedTurn{chunks: []string{"<speak>Yes, the build passes.</speak>"}}
	ctx := withAcceptedModes(auth.WithPrincipal(t.Context(), auth.Owner()), []string{"audio/ogg"})
	var got []a2a.Event
	for ev, err := range v.turn(ctx, execCtx, agent.run) {
		if err != nil {
			t.Fatalf("voiceTurns.turn() error = %v", err)
		}
		got = append(got, ev)
	}
	want := []string{"task:submitted", "artifact:", "artifact:(The voice reply failed: the speech service timed out.)", "artifact:Yes, the build passes.", "status:completed"}
	if diff := cmp.Diff(want, describeVoice(got)); diff != "" {
		t.Fatalf("voiceTurns.turn() wrong events (-want +got) diff = %s", diff)
	}
}

func TestVoiceTurnKeepsAnswersAlone(t *testing.T) {
	t.Parallel()
	fake := &speechtest.Fake{}
	v := newVoiceTurns(fake, fake, time.Now, context.Background())
	question := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which branch?"))
	stored := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: question}}
	execCtx := &a2asrv.ExecutorContext{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("main")), TaskID: "t1", ContextID: "c1", StoredTask: stored}
	agent := &chunkedTurn{chunks: []string{"Pushed to main."}}
	ctx := withAcceptedModes(auth.WithPrincipal(t.Context(), auth.Owner()), []string{"audio/ogg"})
	for _, err := range v.turn(ctx, execCtx, agent.run) {
		if err != nil {
			t.Fatalf("voiceTurns.turn() error = %v", err)
		}
	}
	if len(agent.got.Parts) != 1 || agent.got.Parts[0].Text() != "main" {
		t.Fatalf("agent message = %v, want the answer alone", agent.got.Parts)
	}
}

func TestVoiceAnswerThatFailsAsksAgain(t *testing.T) {
	t.Parallel()
	v := newVoiceTurns(&speechtest.Fake{TranscribeErr: &speech.Error{Reason: "the speech service timed out"}}, nil, time.Now, context.Background())
	question := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Which branch?"))
	stored := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: question}}
	execCtx := &a2asrv.ExecutorContext{Message: a2a.NewMessage(a2a.MessageRoleUser, voiceNote()), TaskID: "t1", ContextID: "c1", StoredTask: stored}
	agent := &scriptedTurn{reply: "unused"}
	var got []a2a.Event
	for ev, err := range v.turn(auth.WithPrincipal(t.Context(), auth.Owner()), execCtx, agent.run) {
		if err != nil {
			t.Fatalf("voiceTurns.turn() error = %v", err)
		}
		got = append(got, ev)
	}
	want := []string{"status:input_required:I couldn't transcribe the voice note: the speech service timed out. The question still waits for an answer; send it again or type it.\nWhich branch?"}
	if diff := cmp.Diff(want, describeVoice(got)); diff != "" {
		t.Fatalf("voiceTurns.turn() wrong events (-want +got) diff = %s", diff)
	}
	if agent.calls != 0 {
		t.Fatalf("agent turns = %d, want 0", agent.calls)
	}
}
