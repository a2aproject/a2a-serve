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

package chat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/speech"
)

// renderer shows one task in a conversation. Messengers which stream get one
// message edited as the agent writes; others get questions and the result.
type renderer struct {
	a       *ChatApprovals
	ch      messenger.Messenger
	caps    messenger.Caps
	conv    messenger.ConvKey
	replyTo messenger.Ref
	p       auth.Principal

	// lim retries messenger calls and holds the messenger's rate limit.
	lim *retrier

	// mu guards the text received so far. It is never held during a
	// messenger call, so that events keep being consumed.
	mu        sync.Mutex
	taskID    a2a.TaskID
	contextID string
	artifacts []*artifactText
	files     []messenger.File
	progress  string
	timer     *time.Timer
	done      bool
	// waitsForOwner is set once the task waits for an owner to answer its
	// question elsewhere, as for guests' conversations.
	waitsForOwner bool

	// showMu serializes what is shown in the conversation, and guards the
	// fields below. It is taken before mu.
	showMu    sync.Mutex
	ref       messenger.Ref
	segStart  int
	lastShown string
	lastEdit  time.Time
	// lostFrom is the offset in the text from which what the user sees is
	// stale because an edit or send failed, or -1.
	lostFrom int

	// onWaitForOwner, if set, is called when the task starts waiting for an
	// owner, before the conversation is told, so that an answer given
	// meanwhile isn't missed.
	onWaitForOwner func(a2a.TaskID)
}

type artifactText struct {
	id   a2a.ArtifactID
	text strings.Builder
}

func newRenderer(a *ChatApprovals, ch messenger.Messenger, conv messenger.ConvKey, replyTo messenger.Ref, p auth.Principal) *renderer {
	caps := ch.Caps()
	if caps.MaxText <= 0 {
		caps.MaxText = 4000
	}
	return &renderer{a: a, ch: ch, caps: caps, conv: conv, replyTo: replyTo, p: p, lostFrom: -1, lim: newRetrier(a.timings.SendRetry)}
}

// onEvent renders an event and reports whether the task stopped: it
// finished or waits for input.
func (rd *renderer) onEvent(ctx context.Context, ev a2a.Event) bool {
	switch e := ev.(type) {
	case *a2a.Task:
		rd.setTask(e.ID, e.ContextID)
		for _, a := range e.Artifacts {
			rd.onArtifact(a, false)
		}
		return rd.onStatus(ctx, e.Status)
	case *a2a.TaskArtifactUpdateEvent:
		rd.setTask(e.TaskID, e.ContextID)
		rd.onArtifact(e.Artifact, e.Append)
		rd.flush(ctx, false)
	case *a2a.TaskStatusUpdateEvent:
		rd.setTask(e.TaskID, e.ContextID)
		return rd.onStatus(ctx, e.Status)
	case *a2a.Message:
		rd.send(ctx, a2atext.Parts(e.Parts))
		return true
	}
	return false
}

func (rd *renderer) setTask(id a2a.TaskID, contextID string) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	rd.taskID, rd.contextID = id, contextID
}

func (rd *renderer) onArtifact(a *a2a.Artifact, appendTo bool) {
	if a == nil {
		return
	}
	rd.mu.Lock()
	defer rd.mu.Unlock()
	var at *artifactText
	for _, x := range rd.artifacts {
		if x.id == a.ID {
			at = x
		}
	}
	if at == nil {
		at = &artifactText{id: a.ID}
		rd.artifacts = append(rd.artifacts, at)
	} else if !appendTo {
		at.text.Reset()
	}
	for _, p := range a.Parts {
		if t := p.Text(); t != "" {
			at.text.WriteString(t)
			continue
		}
		if raw := p.Raw(); len(raw) > 0 {
			name := p.Filename
			if name == "" {
				name = "file"
			}
			rd.files = append(rd.files, messenger.File{Name: name, MediaType: p.MediaType, Data: raw})
		}
	}
	rd.progress = ""
}

func (rd *renderer) onStatus(ctx context.Context, st a2a.TaskStatus) bool {
	switch st.State {
	case a2a.TaskStateSubmitted:
		return false
	case a2a.TaskStateWorking:
		if transcript, ok := transcriptOf(st.Message); ok {
			// A reply to the voice note, which stays in the chat after the
			// result replaces the progress. Mail gets only the result.
			if rd.caps.Stream && transcript != "" {
				rd.send(ctx, a2atext.Parts(st.Message.Parts))
			}
			return false
		}
		if st.Message != nil {
			if text := a2atext.Parts(st.Message.Parts); text != "" {
				rd.mu.Lock()
				rd.progress = text
				rd.mu.Unlock()
				rd.flush(ctx, false)
			}
		}
		return false
	case a2a.TaskStateInputRequired:
		rd.flush(ctx, true)
		rd.question(ctx, st.Message)
		return true
	case a2a.TaskStateAuthRequired:
		rd.flush(ctx, true)
		rd.send(ctx, "The agent needs authorization: "+a2atext.Message(st.Message))
		return true
	case a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected:
		rd.final(ctx, st)
		return true
	}
	return false
}

// transcriptOf returns the transcript of a status message that shows what
// a recording said.
func transcriptOf(msg *a2a.Message) (string, bool) {
	if msg == nil {
		return "", false
	}
	t, ok := msg.Metadata[speech.TranscriptKey].(string)
	return t, ok
}

func (rd *renderer) text() string {
	var sb strings.Builder
	for _, a := range rd.artifacts {
		// Artifacts of files only, such as a voice reply, add no text.
		if a.text.Len() == 0 {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(a.text.String())
	}
	return sb.String()
}

// flush shows the current text. Unless force, edits are throttled, wait for
// the messenger's rate limit, and the latest text is shown when the interval
// passes.
func (rd *renderer) flush(ctx context.Context, force bool) {
	if !rd.caps.Stream {
		return
	}
	if force {
		rd.showMu.Lock()
	} else if !rd.showMu.TryLock() {
		rd.schedule(ctx, rd.a.timings.EditInterval)
		return
	}
	defer rd.showMu.Unlock()
	if !force {
		if wait := max(rd.a.timings.EditInterval-time.Since(rd.lastEdit), rd.lim.remaining()); wait > 0 {
			rd.schedule(ctx, wait)
			return
		}
	}
	rd.mu.Lock()
	done, text, progress := rd.done, rd.text(), rd.progress
	rd.mu.Unlock()
	if done {
		return
	}
	rd.showLocked(ctx, text, progress, force)
}

// schedule flushes again after wait, unless a flush is already scheduled.
func (rd *renderer) schedule(ctx context.Context, wait time.Duration) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rd.done || rd.timer != nil {
		return
	}
	rd.timer = time.AfterFunc(max(wait, time.Millisecond), func() {
		rd.mu.Lock()
		rd.timer = nil
		rd.mu.Unlock()
		rd.flush(ctx, false)
	})
}

// showLocked edits or sends the streaming message, starting a new message
// when the text outgrows the messenger's limit. It returns the offset in the
// text from which the user doesn't see the current text, or -1. A reliable
// call retries what the messenger refused.
func (rd *renderer) showLocked(ctx context.Context, text, progress string, reliable bool) int {
	limit := max(rd.caps.MaxText-200, rd.caps.MaxText/2, 1)
	for len(text)-rd.segStart > limit {
		seg := text[rd.segStart:]
		cut := strings.LastIndex(seg[:limit], "\n")
		if cut < limit/2 {
			cut = len(messenger.Truncate(seg, limit))
		}
		if cut == 0 {
			_, cut = utf8.DecodeRuneInString(seg)
		}
		if !rd.showSegmentLocked(ctx, seg[:cut], reliable) && rd.lostFrom < 0 {
			rd.lostFrom = rd.segStart
		}
		rd.segStart += cut
		rd.ref, rd.lastShown = messenger.Ref{}, ""
	}
	display := text[rd.segStart:]
	if progress != "" {
		if display != "" {
			display += "\n\n"
		}
		display += "⚙ " + progress
	}
	if display != "" && !rd.showSegmentLocked(ctx, display, reliable) && rd.lostFrom < 0 {
		return rd.segStart
	}
	return rd.lostFrom
}

// showSegmentLocked reports whether display is shown.
func (rd *renderer) showSegmentLocked(ctx context.Context, display string, reliable bool) bool {
	if display == rd.lastShown {
		return true
	}
	rd.lastEdit = time.Now()
	out := messenger.Outbound{Conv: rd.conv, Text: display, ReplyTo: rd.replyTo}
	if rd.ref.ID != "" && rd.caps.Edit {
		policy := retryNever
		if reliable {
			policy = retryRateLimits
		}
		err := rd.lim.do(ctx, policy, func() error { return rd.ch.Edit(ctx, rd.ref, out) })
		if err != nil {
			slog.Debug("editing message", "messenger", rd.ch.Name(), "error", err)
			return false
		}
		rd.lastShown = display
		return true
	}
	policy := retryNever
	if reliable {
		policy = retryAll
	}
	var ref messenger.Ref
	err := rd.lim.do(ctx, policy, func() error {
		var err error
		ref, err = rd.ch.Send(ctx, out)
		return err
	})
	if err != nil {
		slog.Warn("sending message", "messenger", rd.ch.Name(), "error", err)
		return false
	}
	rd.ref, rd.lastShown = ref, display
	return true
}

// send sends a standalone message, split to the messenger's limit, and retries
// pieces that fail. It stops at the first piece that doesn't get through, so
// that the text doesn't arrive with a gap.
func (rd *renderer) send(ctx context.Context, text string) messenger.Ref {
	var ref messenger.Ref
	for _, piece := range messenger.Split(text, rd.caps.MaxText) {
		out := messenger.Outbound{Conv: rd.conv, Text: piece, ReplyTo: rd.replyTo, Subject: rd.subject()}
		err := rd.lim.do(ctx, retryAll, func() error {
			var err error
			ref, err = rd.ch.Send(ctx, out)
			return err
		})
		if err != nil {
			slog.Warn("sending message", "messenger", rd.ch.Name(), "error", err)
			return ref
		}
	}
	return ref
}

func (rd *renderer) subject() string { return rd.a.config().Name }

func (rd *renderer) final(ctx context.Context, st a2a.TaskStatus) {
	rd.showMu.Lock()
	defer rd.showMu.Unlock()
	rd.mu.Lock()
	if rd.timer != nil {
		rd.timer.Stop()
		rd.timer = nil
	}
	text := rd.text()
	status := a2atext.Message(st.Message)
	switch st.State {
	case a2a.TaskStateCompleted:
		if text == "" {
			text = status
		}
		if text == "" && len(rd.files) == 0 {
			text = "Done."
		}
	case a2a.TaskStateCanceled:
		text = joinText(text, "Stopped.")
	default:
		if status == "" {
			status = "The task failed."
		}
		text = joinText(text, "⚠ "+status)
	}
	files := rd.files
	rd.done = true
	rd.mu.Unlock()

	lost := -1
	if rd.caps.Stream {
		lost = rd.showLocked(ctx, text, "", true)
	}
	if !rd.caps.Stream && text != "" {
		rd.send(ctx, text)
	}
	if lost >= 0 {
		rd.send(ctx, text[lost:])
	}
	if len(files) > 0 {
		out := messenger.Outbound{Conv: rd.conv, Files: files, ReplyTo: rd.replyTo, Subject: rd.subject()}
		err := rd.lim.do(ctx, retryAll, func() error {
			_, err := rd.ch.Send(ctx, out)
			return err
		})
		if err != nil {
			slog.Warn("sending files", "messenger", rd.ch.Name(), "error", err)
		}
	}
}

// question shows a question the agent asked.
func (rd *renderer) question(ctx context.Context, msg *a2a.Message) {
	rd.mu.Lock()
	taskID, contextID := rd.taskID, rd.contextID
	pending := ""
	if !rd.caps.Stream {
		pending = rd.text()
	}
	rd.done = true
	rd.mu.Unlock()
	if pending != "" {
		rd.send(ctx, pending)
	}
	if msg == nil {
		rd.send(ctx, "The agent needs more input.")
		return
	}
	q := approvals.FromMessage(msg)
	if q == nil || q.Type != approvals.TypePermission {
		rd.send(ctx, a2atext.Message(msg))
		return
	}
	if rd.p.Role == auth.RoleGuest || rd.a.guest(ctx, contextID) {
		rd.mu.Lock()
		rd.waitsForOwner = true
		rd.mu.Unlock()
		if rd.onWaitForOwner != nil {
			rd.onWaitForOwner(taskID)
		}
		rd.send(ctx, fmt.Sprintf("⏳ This needs the owner's approval, which I asked for. Without an answer in %s, it is rejected:\n\n%s",
			approvals.ApprovalTimeout(rd.a.timings.GuestQuestionTimeout), q.Display()))
		return
	}
	// An owner who answered an earlier question may be rendering the task,
	// and its starter must still be able to answer this one.
	user := rd.a.records.Starter(ctx, taskID)
	if user == "" {
		user = rd.p.ID
	}
	rec, err := rd.a.records.NewRecord(ctx, taskID, contextID, "", user, q)
	if err != nil {
		slog.Error("storing question", "error", err)
		rd.send(ctx, a2atext.Message(msg))
		return
	}
	text := q.Display()
	out := messenger.Outbound{Conv: rd.conv, Text: text, ReplyTo: rd.replyTo, Subject: rd.subject()}
	if rd.caps.Buttons {
		out.Question = &messenger.Question{ID: rec.ID, Options: q.Labels()}
	} else {
		out.Text += rd.a.answerHint(ctx, rec, q)
	}
	rd.a.sendQuestion(ctx, rd.ch, out, rec.ID, text, rd.lim)
}

// waitingForOwner returns the task which waits for an owner's answer.
func (rd *renderer) waitingForOwner() (a2a.TaskID, bool) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	return rd.taskID, rd.waitsForOwner
}

func (rd *renderer) fail(ctx context.Context, err error) {
	rd.mu.Lock()
	rd.done = true
	rd.mu.Unlock()
	rd.send(ctx, "⚠ "+err.Error())
}

// finish handles a stream which ended without a final event: the task's
// stored state tells whether it finished or the stream was cut off.
func (rd *renderer) finish(ctx context.Context) {
	rd.mu.Lock()
	done, taskID := rd.done, rd.taskID
	rd.mu.Unlock()
	if done {
		return
	}
	interrupted := a2a.TaskStatus{State: a2a.TaskStateFailed, Message: a2a.NewMessage(a2a.MessageRoleAgent,
		a2a.NewTextPart("The task was interrupted before it finished."))}
	if taskID == "" {
		rd.final(ctx, interrupted)
		return
	}
	stored, err := rd.a.tasks.Header(auth.WithPrincipal(context.WithoutCancel(ctx), auth.Owner()), taskID)
	if err != nil {
		slog.Warn("loading the task of an ended stream", "task", taskID, "error", err)
		rd.final(ctx, interrupted)
		return
	}
	if !stored.Task.Status.State.Terminal() {
		rd.final(ctx, interrupted)
		return
	}
	rd.final(ctx, stored.Task.Status)
}

func joinText(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n\n" + b
}
