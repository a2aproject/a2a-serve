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

func voiceNote() *a2a.Part {
	p := a2a.NewRawPart([]byte("OggS voice"))
	p.MediaType, p.Filename = "audio/ogg", "voice.ogg"
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
			name:      "reply spoken when accepted",
			fake:      &speechtest.Fake{},
			parts:     []*a2a.Part{a2a.NewTextPart("what changed?")},
			accepted:  []string{"audio/ogg"},
			want:      []string{"task:submitted", "status:working", "artifact:Fixed it.", "artifact:reply.ogg audio/ogg OggS", "status:completed"},
			wantAgent: []string{"what changed?"},
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
			wantAgent: []string{"what changed?"},
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
			v := newVoiceTurns(tc.fake, speaker, time.Now)
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
					gotAgent = append(gotAgent, p.Text())
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
	v := newVoiceTurns(&speechtest.Fake{Transcript: "hello"}, nil, time.Now)
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
	ts.ch.Write("telegram:1", "dm1", "scheduled again")
	_, voice := ts.ch.WaitFor(t, "dm1", func(m messenger.Outbound) bool { return len(m.Files) > 0 })
	want := []messenger.File{{Name: "reply.ogg", MediaType: "audio/ogg", Data: speechtest.Speech}}
	if diff := cmp.Diff(want, voice.Files); diff != "" {
		t.Fatalf("voice reply wrong files (-want +got) diff = %s", diff)
	}
	var spoken []string
	for _, r := range svc.Requests() {
		if r.Path == "/v1/audio/speech" {
			spoken = append(spoken, r.Input)
		}
	}
	if diff := cmp.Diff([]string{"ran scheduled again."}, spoken); diff != "" {
		t.Fatalf("spoken texts wrong result (-want +got) diff = %s", diff)
	}
}
