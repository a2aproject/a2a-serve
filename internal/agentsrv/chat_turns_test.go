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
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/a2aproject/a2a-serve/internal/a2atext"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/pairings"
	"github.com/a2aproject/a2a-serve/internal/store"
)

const agentReply = "Hello from the agent."

type senderCall struct {
	req       *a2a.SendMessageRequest
	caller    auth.Principal
	inProcess bool
}

// fakeTurnSender records what ChatTurns asks the agent to do and answers
// with scripted events.
type fakeTurnSender struct {
	// reply returns the events the agent streams for a request.
	reply     func(*a2a.SendMessageRequest) []a2a.Event
	streamErr error
	// hold, if set, keeps a stream from yielding until it is closed.
	hold      chan struct{}
	sendErr   error
	cancelErr error

	streams chan senderCall
	sends   chan senderCall
	cancels chan *a2a.CancelTaskRequest
}

func newFakeTurnSender() *fakeTurnSender {
	return &fakeTurnSender{
		reply: func(*a2a.SendMessageRequest) []a2a.Event {
			return []a2a.Event{a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(agentReply))}
		},
		streams: make(chan senderCall, 32),
		sends:   make(chan senderCall, 32),
		cancels: make(chan *a2a.CancelTaskRequest, 32),
	}
}

func callOf(ctx context.Context, req *a2a.SendMessageRequest) senderCall {
	caller, _ := auth.Caller(ctx)
	return senderCall{req: req, caller: caller, inProcess: auth.IsInProcess(ctx)}
}

func (s *fakeTurnSender) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	s.sends <- callOf(ctx, req)
	return nil, s.sendErr
}

func (s *fakeTurnSender) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	s.streams <- callOf(ctx, req)
	return func(yield func(a2a.Event, error) bool) {
		if s.hold != nil {
			select {
			case <-s.hold:
			case <-ctx.Done():
				return
			}
		}
		if s.streamErr != nil {
			yield(nil, s.streamErr)
			return
		}
		for _, ev := range s.reply(req) {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (s *fakeTurnSender) CancelTask(_ context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	s.cancels <- req
	return nil, s.cancelErr
}

func (s *fakeTurnSender) nextStream(t *testing.T) senderCall {
	t.Helper()
	select {
	case call := <-s.streams:
		return call
	case <-time.After(waitTimeout):
		t.Fatal("SendStreamingMessage() was not called")
		return senderCall{}
	}
}

func (s *fakeTurnSender) nextSend(t *testing.T) senderCall {
	t.Helper()
	select {
	case call := <-s.sends:
		return call
	case <-time.After(waitTimeout):
		t.Fatal("SendMessage() was not called")
		return senderCall{}
	}
}

func (s *fakeTurnSender) nextCancel(t *testing.T) *a2a.CancelTaskRequest {
	t.Helper()
	select {
	case req := <-s.cancels:
		return req
	case <-time.After(waitTimeout):
		t.Fatal("CancelTask() was not called")
		return nil
	}
}

// wantIdle fails if the agent is asked to do anything soon.
func (s *fakeTurnSender) wantIdle(t *testing.T) {
	t.Helper()
	select {
	case call := <-s.streams:
		t.Fatalf("SendStreamingMessage(%s) called, want no call", sentMessageOf(call.req.Message).Text)
	case call := <-s.sends:
		t.Fatalf("SendMessage(%s) called, want no call", sentMessageOf(call.req.Message).Text)
	case req := <-s.cancels:
		t.Fatalf("CancelTask(%s) called, want no call", req.ID)
	case <-time.After(20 * time.Millisecond):
	}
}

// sentMessage is what the tests check of a message sent to the agent.
type sentMessage struct {
	ID        string
	ContextID string
	TaskID    a2a.TaskID
	Text      string
	Metadata  map[string]any
}

func sentMessageOf(m *a2a.Message) sentMessage {
	return sentMessage{ID: m.ID, ContextID: m.ContextID, TaskID: m.TaskID, Text: a2atext.Parts(m.Parts), Metadata: m.Metadata}
}

// turnsFixture is a ChatTurns that has a linked owner in conversation dm1
// of the channel fake.
type turnsFixture struct {
	*chatFixture
	ch         *fakeChannel
	sender     *fakeTurnSender
	unattended *contextSet
	turns      *ChatTurns
}

func newTurnsFixture(t *testing.T) *turnsFixture {
	t.Helper()
	ch := newFakeChannel("fake")
	fx := newChatFixture(t, "", ch)
	sender := newFakeTurnSender()
	unattended := &contextSet{}
	agentName := func() string { return "coder" }
	svc := approvals.New(approvals.Deps{
		Records:          fx.records,
		Sender:           sender,
		Notifier:         fx.chat,
		Unattended:       unattended.has,
		Anonymous:        func(context.Context, string) bool { return false },
		AnonymousTimeout: fx.timings.AnonymousQuestionTimeout,
		AgentName:        agentName,
	})
	turns := newChatTurns(ChatTurnsDeps{
		Store:      fx.store,
		Tasks:      fx.tasks,
		Chain:      fx.chain,
		Pairings:   &pairings.Service{Store: fx.store, Limiter: pairings.NewLimiter()},
		Approvals:  svc,
		Chat:       fx.chat,
		Supervisor: fx.supervisor,
		Queue:      fx.queue,
		Sender:     sender,
		Bus:        fx.bus,
		Unattended: unattended,
		AgentName:  agentName,
	})
	turns.start(fx.ctx)
	fx.link(t, linkSpec{pairing: auth.LocalPairing, channel: "fake", identity: "telegram:1", conv: "dm1"})
	return &turnsFixture{chatFixture: fx, ch: ch, sender: sender, unattended: unattended, turns: turns}
}

func (fx *turnsFixture) setContext(t *testing.T, conv, contextID string) {
	t.Helper()
	if err := fx.store.SetContext(fx.ctx, "fake", conv, contextID); err != nil {
		t.Fatalf("SetContext() error = %v", err)
	}
}

func (fx *turnsFixture) contextOf(t *testing.T, conv string) string {
	t.Helper()
	id, err := fx.store.ContextOf(fx.ctx, "fake", conv)
	if err != nil {
		t.Fatalf("ContextOf() error = %v", err)
	}
	return id
}

// askPendingQuestion makes the task t1 of context c1 wait for the answer to
// a question that was sent to chat conv, which is the conversation of c1.
func (fx *turnsFixture) askPendingQuestion(t *testing.T, conv string, state a2a.TaskState) store.Question {
	t.Helper()
	fx.putTask(t, "t1", "c1", state)
	fx.setContext(t, conv, "c1")
	rec := fx.newQuestion(t, "t1", "c1", auth.OwnerID)
	fx.chat.Ask(fx.ctx, approvals.Chat{Channel: "fake", Conv: channel.ConvKey(conv)}, "coder asks", rec)
	return rec
}

// askInDM1 sends the question of task taskID of context contextID to chat
// dm1, as the owner chat gets it, and returns it with the ID of its message.
func (fx *turnsFixture) askInDM1(t *testing.T, taskID a2a.TaskID, contextID string) (store.Question, string) {
	t.Helper()
	fx.putTask(t, taskID, contextID, a2a.TaskStateInputRequired)
	rec := fx.newQuestion(t, taskID, contextID, auth.OwnerID)
	fx.chat.Ask(fx.ctx, approvals.Chat{Channel: "fake", Conv: "dm1"}, "coder asks", rec)
	ref, _ := fx.ch.waitFor(t, "dm1", func(m channel.Outbound) bool { return m.Question != nil && m.Question.ID == rec.ID })
	return rec, ref
}

func (fx *turnsFixture) question(t *testing.T, id string) store.Question {
	t.Helper()
	rec, err := fx.store.QuestionByID(fx.ctx, id)
	if err != nil {
		t.Fatalf("QuestionByID() error = %v", err)
	}
	return rec
}

func (fx *turnsFixture) wantNoPendingQuestion(t *testing.T, conv string) {
	t.Helper()
	if ids, err := fx.store.PendingQuestions(fx.ctx, "fake", conv); err != nil || len(ids) != 0 {
		t.Fatalf("PendingQuestions() = %q, %v, want none", ids, err)
	}
}

func partsJSON(t *testing.T, m *a2a.Message) string {
	t.Helper()
	b, err := json.Marshal(m.Parts)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return string(b)
}

func TestChatTurnsStreamsMessageOfLinkedSender(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	in := fx.ch.message("telegram:1", "dm1", "hello")
	in.Name = "Anna"

	fx.ch.deliver(in)

	call := fx.sender.nextStream(t)
	want := sentMessage{
		ID:        "fake:" + in.MessageID,
		ContextID: fx.contextOf(t, "dm1"),
		Text:      "hello",
		Metadata:  map[string]any{"channel": "fake", "identity": "telegram:1", "sender": "Anna"},
	}
	if want.ContextID == "" {
		t.Fatal("ContextOf() = \"\", want the context of the conversation")
	}
	if diff := cmp.Diff(want, sentMessageOf(call.req.Message)); diff != "" {
		t.Fatalf("SendStreamingMessage() message wrong result (-want +got) diff = %s", diff)
	}
	if call.req.Message.Role != a2a.MessageRoleUser {
		t.Errorf("SendStreamingMessage() message role = %v, want %v", call.req.Message.Role, a2a.MessageRoleUser)
	}
	if !call.caller.IsOwner() || !call.inProcess {
		t.Errorf("SendStreamingMessage() caller = %+v, in process = %v, want the owner in process", call.caller, call.inProcess)
	}
	fx.ch.waitFor(t, "dm1", hasText(agentReply))
}

func TestChatTurnsKeepsContextUntilNewConversation(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	var contexts []string
	hello := func() {
		fx.ch.write("telegram:1", "dm1", "hello")
		contexts = append(contexts, fx.sender.nextStream(t).req.Message.ContextID)
	}

	hello()
	hello()
	fx.ch.write("telegram:1", "dm1", "/new")
	fx.ch.waitFor(t, "dm1", hasText("Started a new conversation."))
	hello()

	if contexts[0] == "" || contexts[0] != contexts[1] {
		t.Errorf("contexts of follow-up messages = %q, want the same one", contexts[:2])
	}
	if contexts[2] == "" || contexts[2] == contexts[0] {
		t.Errorf("context after /new = %q, want a new one, not %q", contexts[2], contexts[0])
	}
	if got := fx.contextOf(t, "dm1"); got != contexts[2] {
		t.Errorf("ContextOf() = %q, want %q", got, contexts[2])
	}
}

func TestChatTurnsReplyWithoutAskingAgent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		identity string
		conv     string
		text     string
		want     string
	}{
		{
			name:     "start greets",
			identity: "telegram:1", conv: "dm1", text: "/start",
			want: "Hi! I'm coder. Write what you need; /new starts a new conversation, /stop stops the current task.",
		},
		{
			name:     "start addressed to the bot greets",
			identity: "telegram:1", conv: "dm1", text: "/START@coder_bot",
			want: "Hi! I'm coder. Write what you need; /new starts a new conversation, /stop stops the current task.",
		},
		{
			name:     "new tells it started a conversation",
			identity: "telegram:1", conv: "dm1", text: "/new",
			want: "Started a new conversation.",
		},
		{
			name:     "unknown sender is told how to link",
			identity: "telegram:9", conv: "dm9", text: "hello",
			want: "I don't know you yet. Ask my owner for a code (`a2a-serve pair invite <you>`), then send: join <code>",
		},
		{
			name:     "wrong code is refused",
			identity: "telegram:9", conv: "dm9", text: "join AAAA-BBBB",
			want: "That code is invalid or has expired. Ask the owner for a new one.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newTurnsFixture(t)

			fx.ch.write(tc.identity, tc.conv, tc.text)

			_, got := fx.ch.waitFor(t, tc.conv, hasText(tc.want))
			if got.Text != tc.want {
				t.Fatalf("reply = %q, want %q", got.Text, tc.want)
			}
			fx.sender.wantIdle(t)
		})
	}
}

func TestChatTurnsLinkCodeLinksSender(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	code, _, err := auth.MintCode(fx.ctx, fx.store, auth.LocalPairing)
	if err != nil {
		t.Fatalf("MintCode() error = %v", err)
	}

	fx.ch.write("telegram:9", "dm9", "join "+code)
	fx.ch.waitFor(t, "dm9", hasText("Linked as owner. Write to talk to coder."))
	fx.ch.write("telegram:9", "dm9", "hello")

	call := fx.sender.nextStream(t)
	if got := sentMessageOf(call.req.Message).Text; got != "hello" {
		t.Fatalf("SendStreamingMessage() text = %q, want %q", got, "hello")
	}
	if !call.caller.IsOwner() {
		t.Fatalf("SendStreamingMessage() caller = %+v, want an owner", call.caller)
	}
}

func TestChatTurnsLinkCodeLimitsFailures(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	for range 11 {
		fx.ch.write("telegram:9", "dm9", "join WRNG-C0DE")
	}
	fx.ch.waitFor(t, "dm9", hasText("Too many invalid codes."))
}

func TestChatTurnsStop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		text       string
		hasContext bool
		state      a2a.TaskState
		cancelErr  error
		wantReply  string
		wantCancel bool
	}{
		{name: "no conversation yet", text: "/stop", wantReply: "Nothing is running."},
		{name: "no task in the conversation", text: "/stop", hasContext: true, wantReply: "Nothing is running."},
		{name: "latest task finished", text: "/stop", hasContext: true, state: a2a.TaskStateCompleted, wantReply: "Nothing is running."},
		{name: "latest task running", text: "/stop", hasContext: true, state: a2a.TaskStateWorking, wantReply: "Stopped.", wantCancel: true},
		{name: "latest task waits for input", text: "/stop", hasContext: true, state: a2a.TaskStateInputRequired, wantReply: "Stopped.", wantCancel: true},
		{name: "command addressed to the bot", text: "/stop@coder_bot", hasContext: true, state: a2a.TaskStateWorking, wantReply: "Stopped.", wantCancel: true},
		{name: "task can't be canceled", text: "/stop", hasContext: true, state: a2a.TaskStateWorking, cancelErr: errors.New("boom"), wantReply: "Couldn't stop the task: boom", wantCancel: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newTurnsFixture(t)
			fx.sender.cancelErr = tc.cancelErr
			if tc.hasContext {
				fx.setContext(t, "dm1", "c1")
			}
			if tc.state != "" {
				fx.putTask(t, "t1", "c1", tc.state)
			}

			fx.ch.write("telegram:1", "dm1", tc.text)

			fx.ch.waitFor(t, "dm1", func(m channel.Outbound) bool { return m.Text == tc.wantReply })
			if tc.wantCancel {
				if got := fx.sender.nextCancel(t).ID; got != "t1" {
					t.Fatalf("CancelTask() task = %q, want %q", got, "t1")
				}
			}
			fx.sender.wantIdle(t)
		})
	}
}

func TestChatTurnsStopDoesNotWaitForRunningTurn(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	fx.sender.hold = make(chan struct{})
	fx.setContext(t, "dm1", "c1")
	fx.putTask(t, "t1", "c1", a2a.TaskStateWorking)
	fx.ch.write("telegram:1", "dm1", "slow")
	fx.sender.nextStream(t)

	fx.ch.write("telegram:1", "dm1", "/stop")

	fx.ch.waitFor(t, "dm1", hasText("Stopped."))
	if got := fx.sender.nextCancel(t).ID; got != "t1" {
		t.Fatalf("CancelTask() task = %q, want %q", got, "t1")
	}
	close(fx.sender.hold)
	fx.ch.waitFor(t, "dm1", hasText(agentReply))
}

func TestChatTurnsInjectTurnWithoutConversationPostsUnattendedMessage(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)

	if err := fx.turns.injectTurn(fx.ctx, postedTurn{ContextID: "c-schedule", User: auth.Owner(), Text: "run it"}); err != nil {
		t.Fatalf("turns.injectTurn() error = %v", err)
	}

	call := fx.sender.nextSend(t)
	want := sentMessage{ContextID: "c-schedule", Text: "run it"}
	if diff := cmp.Diff(want, sentMessageOf(call.req.Message), cmpopts.IgnoreFields(sentMessage{}, "ID")); diff != "" {
		t.Fatalf("SendMessage() message wrong result (-want +got) diff = %s", diff)
	}
	if call.req.Config == nil || !call.req.Config.ReturnImmediately {
		t.Errorf("SendMessage() config = %+v, want ReturnImmediately", call.req.Config)
	}
	if !call.caller.IsOwner() || !call.inProcess {
		t.Errorf("SendMessage() caller = %+v, in process = %v, want the owner in process", call.caller, call.inProcess)
	}
	if !fx.unattended.has("c-schedule") {
		t.Error("unattended.has(c-schedule) = false, want true")
	}
	fx.sender.wantIdle(t)
}

func TestChatTurnsInjectTurnInConversationShowsReplyInChat(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	fx.setContext(t, "dm1", "c1")

	if err := fx.turns.injectTurn(fx.ctx, postedTurn{ContextID: "c1", User: auth.Owner(), Text: "run it"}); err != nil {
		t.Fatalf("turns.injectTurn() error = %v", err)
	}

	call := fx.sender.nextStream(t)
	want := sentMessage{ContextID: "c1", Text: "run it"}
	if diff := cmp.Diff(want, sentMessageOf(call.req.Message), cmpopts.IgnoreFields(sentMessage{}, "ID")); diff != "" {
		t.Fatalf("SendStreamingMessage() message wrong result (-want +got) diff = %s", diff)
	}
	fx.ch.waitFor(t, "dm1", hasText(agentReply))
	if fx.unattended.has("c1") {
		t.Error("unattended.has(c1) = true, want false for a conversation in a chat")
	}
}

func TestChatTurnsInjectTurnWaitsForPendingQuestion(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	fx.setContext(t, "dm1", "c1")
	fx.putTask(t, "t1", "c1", a2a.TaskStateInputRequired)

	if err := fx.turns.injectTurn(fx.ctx, postedTurn{ContextID: "c1", User: auth.Owner(), Text: "run it"}); err != nil {
		t.Fatalf("turns.injectTurn() error = %v", err)
	}
	fx.sender.wantIdle(t)
	fx.endExecution(t, "t1", "c1", a2a.TaskStateCompleted)

	call := fx.sender.nextStream(t)
	if got := sentMessageOf(call.req.Message).Text; got != "run it" {
		t.Fatalf("SendStreamingMessage() text = %q, want %q", got, "run it")
	}
}

func TestChatTurnsInjectTurnWaitsForExecutionToEnd(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	fx.setContext(t, "dm1", "c1")
	fx.putTask(t, "t1", "c1", a2a.TaskStateInputRequired)
	if err := fx.turns.injectTurn(fx.ctx, postedTurn{ContextID: "c1", User: auth.Owner(), Text: "run it"}); err != nil {
		t.Fatalf("turns.injectTurn() error = %v", err)
	}

	fx.endExecution(t, "t1", "c1", a2a.TaskStateInputRequired)
	fx.sender.wantIdle(t)
	fx.setTaskState(t, "t1", "c1", a2a.TaskStateWorking)
	fx.sender.wantIdle(t)
	fx.endExecution(t, "t1", "c1", a2a.TaskStateCompleted)

	fx.sender.nextStream(t)
}

func TestChatTurnsInjectTurnMergesRunsOfAScheduleWhileHeld(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	fx.setContext(t, "dm1", "c1")
	fx.putTask(t, "t1", "c1", a2a.TaskStateInputRequired)
	for _, turn := range []postedTurn{
		{Text: "run", Key: "schedule:brief"},
		{Text: "result a"},
		{Text: "run", Key: "schedule:brief"},
		{Text: "result b"},
		{Text: "run", Key: "schedule:brief"},
	} {
		turn.ContextID, turn.User = "c1", auth.Owner()
		if err := fx.turns.injectTurn(fx.ctx, turn); err != nil {
			t.Fatalf("turns.injectTurn() error = %v", err)
		}
	}
	fx.sender.wantIdle(t)

	fx.endExecution(t, "t1", "c1", a2a.TaskStateCompleted)

	var got []string
	for range 3 {
		got = append(got, sentMessageOf(fx.sender.nextStream(t).req.Message).Text)
	}
	want := []string{
		"run\n\n(Runs skipped while this one waited for an answer in this conversation: 2.)",
		"result a",
		"result b",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("SendStreamingMessage() texts wrong result (-want +got) diff = %s", diff)
	}
	fx.sender.wantIdle(t)
}

func TestChatTurnsInjectTurnWithoutConversationWaitsForPendingQuestion(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	fx.putTask(t, "t1", "c-schedule", a2a.TaskStateInputRequired)
	accepted := make(chan error, 1)
	turn := postedTurn{ContextID: "c-schedule", User: auth.Owner(), Text: "run it", Accepted: func(err error) { accepted <- err }}
	if err := fx.turns.injectTurn(fx.ctx, turn); err != nil {
		t.Fatalf("turns.injectTurn() error = %v", err)
	}
	fx.sender.wantIdle(t)

	fx.endExecution(t, "t1", "c-schedule", a2a.TaskStateCompleted)

	if got := sentMessageOf(fx.sender.nextSend(t).req.Message).Text; got != "run it" {
		t.Fatalf("SendMessage() text = %q, want %q", got, "run it")
	}
	if err := <-accepted; err != nil {
		t.Fatalf("postedTurn.Accepted() error = %v, want nil", err)
	}
}

func TestChatTurnsDropPostedRemovesWaitingTurns(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	fx.setContext(t, "dm1", "c1")
	fx.putTask(t, "t1", "c1", a2a.TaskStateInputRequired)
	accepted := make(chan error, 1)
	turn := postedTurn{ContextID: "c1", User: auth.Owner(), Text: "run", Key: "schedule:brief", Accepted: func(err error) { accepted <- err }}
	if err := fx.turns.injectTurn(fx.ctx, turn); err != nil {
		t.Fatalf("turns.injectTurn() error = %v", err)
	}

	fx.turns.dropPosted("schedule:brief")

	if err := <-accepted; !errors.Is(err, errTurnDropped) {
		t.Fatalf("postedTurn.Accepted() error = %v, want %v", err, errTurnDropped)
	}
	fx.endExecution(t, "t1", "c1", a2a.TaskStateCompleted)
	fx.sender.wantIdle(t)
}

func TestChatTurnsDropPostedOfRemovesWaitingTurnsOfPrincipals(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	fx.setContext(t, "dm1", "c1")
	fx.putTask(t, "t1", "c1", a2a.TaskStateInputRequired)
	accepted := make(chan error, 1)
	user := auth.Principal{ID: "pair:bob"}
	turn := postedTurn{ContextID: "c1", User: user, Text: "run", Accepted: func(err error) { accepted <- err }}
	if err := fx.turns.injectTurn(fx.ctx, turn); err != nil {
		t.Fatalf("turns.injectTurn() error = %v", err)
	}

	fx.turns.dropPostedOf("pair:bob")

	if err := <-accepted; !errors.Is(err, errTurnDropped) {
		t.Fatalf("postedTurn.Accepted() error = %v, want %v", err, errTurnDropped)
	}
	fx.endExecution(t, "t1", "c1", a2a.TaskStateCompleted)
	fx.sender.wantIdle(t)
}

func TestChatTurnsTypedAnswer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		text       string
		wantOption string
		wantLabel  string
	}{
		{text: "approve", wantOption: "once", wantLabel: "Allow"},
		{text: "yes", wantOption: "once", wantLabel: "Allow"},
		{text: "Always.", wantOption: "always", wantLabel: "Always allow"},
		{text: "deny", wantOption: "reject", wantLabel: "Deny"},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			t.Parallel()
			fx := newTurnsFixture(t)
			rec := fx.askPendingQuestion(t, "dm1", a2a.TaskStateInputRequired)

			fx.ch.write("telegram:1", "dm1", tc.text)

			call := fx.sender.nextStream(t)
			wantMsg, err := approvals.AnswerMessage("t1", "c1", "tool1", tc.wantOption)
			if err != nil {
				t.Fatalf("AnswerMessage() error = %v", err)
			}
			if got := call.req.Message; got.TaskID != "t1" || got.ContextID != "c1" || partsJSON(t, got) != partsJSON(t, wantMsg) {
				t.Fatalf("SendStreamingMessage() message = %+v, want the answer %q to task t1 in context c1", got, tc.wantOption)
			}
			got := fx.question(t, rec.ID)
			if got.AnsweredBy != auth.OwnerID || got.Answer != tc.wantOption {
				t.Errorf("question answered by %q with %q, want %q with %q", got.AnsweredBy, got.Answer, auth.OwnerID, tc.wantOption)
			}
			fx.wantNoPendingQuestion(t, "dm1")
			fx.ch.waitFor(t, "dm1", hasText("✓ "+tc.wantLabel+" by telegram:1"))
		})
	}
}

func TestChatTurnsTypedAnswerWithSeveralOpenQuestions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		replyTo   int
		wantTask  a2a.TaskID
		wantReply string
	}{
		{name: "reply to the first question", replyTo: 1, wantTask: "t1"},
		{name: "reply to the second question", replyTo: 2, wantTask: "t2"},
		{name: "no reply", wantReply: "Several questions wait for an answer here. Reply to the message of the question you answer."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newTurnsFixture(t)
			first, firstRef := fx.askInDM1(t, "t1", "c1")
			second, secondRef := fx.askInDM1(t, "t2", "c2")
			in := fx.ch.message("telegram:1", "dm1", "approve")
			switch tc.replyTo {
			case 1:
				in.ReplyTo = channel.Ref{Conv: "dm1", ID: firstRef}
			case 2:
				in.ReplyTo = channel.Ref{Conv: "dm1", ID: secondRef}
			}

			fx.ch.deliver(in)

			if tc.wantReply != "" {
				fx.ch.waitFor(t, "dm1", hasText(tc.wantReply))
				fx.sender.wantIdle(t)
			} else if got := fx.sender.nextSend(t).req.Message.TaskID; got != tc.wantTask {
				t.Fatalf("SendMessage() task = %q, want %q", got, tc.wantTask)
			}
			answered := map[a2a.TaskID]string{}
			for _, rec := range []store.Question{first, second} {
				if by := fx.question(t, rec.ID).AnsweredBy; by != "" {
					answered[a2a.TaskID(rec.TaskID)] = by
				}
			}
			want := map[a2a.TaskID]string{}
			if tc.wantTask != "" {
				want[tc.wantTask] = auth.OwnerID
			}
			if diff := cmp.Diff(want, answered); diff != "" {
				t.Fatalf("answered questions wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestChatTurnsTypedAnswerAfterButtonAnswersTheOtherQuestion(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	first, firstRef := fx.askInDM1(t, "t1", "c1")
	second, _ := fx.askInDM1(t, "t2", "c2")

	fx.ch.click("telegram:1", "dm1", firstRef, &channel.Question{ID: first.ID}, 0)
	if got := fx.sender.nextSend(t).req.Message.TaskID; got != "t1" {
		t.Fatalf("SendMessage() task = %q, want t1", got)
	}
	fx.ch.write("telegram:1", "dm1", "deny")

	if got := fx.sender.nextSend(t).req.Message.TaskID; got != "t2" {
		t.Fatalf("SendMessage() task = %q, want t2", got)
	}
	if got := fx.question(t, second.ID).Answer; got != "reject" {
		t.Fatalf("question answer = %q, want %q", got, "reject")
	}
}

func TestChatTurnsReplyToQuestionOfOtherTextContinuesWaitingTask(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	rec := fx.askPendingQuestion(t, "dm1", a2a.TaskStateInputRequired)

	fx.ch.write("telegram:1", "dm1", "what would it push?")

	call := fx.sender.nextStream(t)
	want := sentMessage{ContextID: "c1", TaskID: "t1", Text: "what would it push?"}
	got := sentMessageOf(call.req.Message)
	if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(sentMessage{}, "ID", "Metadata")); diff != "" {
		t.Fatalf("SendStreamingMessage() message wrong result (-want +got) diff = %s", diff)
	}
	if by := fx.question(t, rec.ID).AnsweredBy; by != "" {
		t.Fatalf("question answered by %q, want nobody", by)
	}
}

func TestChatTurnsTypedAnswerToClosedQuestionIsAMessage(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	rec := fx.askPendingQuestion(t, "dm1", a2a.TaskStateCompleted)

	fx.ch.write("telegram:1", "dm1", "deny")

	call := fx.sender.nextStream(t)
	want := sentMessage{ContextID: "c1", Text: "deny"}
	if diff := cmp.Diff(want, sentMessageOf(call.req.Message), cmpopts.IgnoreFields(sentMessage{}, "ID", "Metadata")); diff != "" {
		t.Fatalf("SendStreamingMessage() message wrong result (-want +got) diff = %s", diff)
	}
	fx.wantNoPendingQuestion(t, "dm1")
	fx.ch.waitFor(t, "dm1", hasText("✗ The task no longer waits for an answer."))
	if by := fx.question(t, rec.ID).AnsweredBy; by != "" {
		t.Fatalf("question answered by %q, want nobody", by)
	}
}

func TestChatTurnsMemberCantAnswerPermissionOfOthers(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	fx.link(t, linkSpec{pairing: "anna", role: auth.RoleAuthenticated, channel: "fake", identity: "telegram:2", conv: "dm2"})
	rec := fx.askPendingQuestion(t, "dm2", a2a.TaskStateInputRequired)

	fx.ch.write("telegram:2", "dm2", "approve")

	fx.ch.waitFor(t, "dm2", hasText("Only the person who started the task or an owner can answer this."))
	if by := fx.question(t, rec.ID).AnsweredBy; by != "" {
		t.Fatalf("question answered by %q, want nobody", by)
	}
	fx.sender.wantIdle(t)
}

func TestChatTurnsButtonAnswersQuestionOnce(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	rec := fx.askPendingQuestion(t, "dm1", a2a.TaskStateInputRequired)
	ref, q := fx.ch.waitFor(t, "dm1", hasQuestion)

	fx.ch.click("telegram:1", "dm1", ref, q.Question, 2)

	call := fx.sender.nextStream(t)
	wantMsg, err := approvals.AnswerMessage("t1", "c1", "tool1", "reject")
	if err != nil {
		t.Fatalf("AnswerMessage() error = %v", err)
	}
	if partsJSON(t, call.req.Message) != partsJSON(t, wantMsg) {
		t.Fatalf("SendStreamingMessage() message = %+v, want the answer reject", call.req.Message)
	}
	if got := fx.question(t, rec.ID).Answer; got != "reject" {
		t.Errorf("question answer = %q, want %q", got, "reject")
	}

	fx.ch.click("telegram:1", "dm1", ref, q.Question, 0)

	fx.ch.waitFor(t, "dm1", hasText("Already answered."))
	fx.sender.wantIdle(t)
}

func TestChatTurnsDropsRedeliveredMessages(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	first := fx.ch.message("telegram:1", "dm1", "hello")
	second := fx.ch.message("telegram:1", "dm1", "again")

	fx.ch.deliver(first)
	fx.ch.deliver(first)
	fx.ch.deliver(second)

	var ids []string
	for range 2 {
		ids = append(ids, fx.sender.nextStream(t).req.Message.ID)
	}
	want := []string{"fake:" + first.MessageID, "fake:" + second.MessageID}
	if diff := cmp.Diff(want, ids); diff != "" {
		t.Fatalf("SendStreamingMessage() message IDs wrong result (-want +got) diff = %s", diff)
	}
	fx.sender.wantIdle(t)
}

func TestChatTurnsHandlesConversationInArrivalOrder(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	gate := make(chan struct{})
	slow := fx.ch.message("telegram:1", "dm1", "first")
	slow.Prepare = func(context.Context, *channel.Inbound) error {
		<-gate
		return nil
	}

	fx.ch.deliver(slow)
	fx.ch.write("telegram:1", "dm1", "second")
	fx.sender.wantIdle(t)
	close(gate)

	var texts []string
	for range 2 {
		texts = append(texts, sentMessageOf(fx.sender.nextStream(t).req.Message).Text)
	}
	if diff := cmp.Diff([]string{"first", "second"}, texts); diff != "" {
		t.Fatalf("SendStreamingMessage() texts wrong result (-want +got) diff = %s", diff)
	}
}

func TestChatTurnsTellsSenderWhenPreparingTheMessageFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		text       string
		prepareErr error
		wantNotice string
		wantTurn   bool
	}{
		{
			name:       "attachment that failed to download",
			text:       "look at this",
			prepareErr: &channel.AttachmentError{Name: "report.pdf", Err: errors.New("timeout")},
			wantNotice: "⚠ I couldn't download report.pdf, so the agent doesn't see it.",
			wantTurn:   true,
		},
		{
			name: "several attachments that failed to download",
			text: "look at this",
			prepareErr: errors.Join(
				&channel.AttachmentError{Name: "a.png", Err: errors.New("timeout")},
				errors.New("recording thread"),
				&channel.AttachmentError{Name: "b.png", Err: errors.New("timeout")},
			),
			wantNotice: "⚠ I couldn't download a.png, b.png, so the agent doesn't see them.",
			wantTurn:   true,
		},
		{
			name:       "message with only a failed attachment",
			prepareErr: &channel.AttachmentError{Name: "report.pdf", Err: errors.New("timeout")},
			wantNotice: "⚠ I couldn't download report.pdf, so the agent doesn't see it.",
		},
		{
			name:       "message that can't be handled",
			text:       "hello",
			prepareErr: &channel.DroppedError{Reason: "⚠ I couldn't start a thread, so I ignored your message.", Err: errors.New("missing access")},
			wantNotice: "⚠ I couldn't start a thread, so I ignored your message.",
		},
		{name: "other error", text: "hello", prepareErr: errors.New("recording thread"), wantTurn: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newTurnsFixture(t)
			in := fx.ch.message("telegram:1", "dm1", tc.text)
			in.Prepare = func(context.Context, *channel.Inbound) error { return tc.prepareErr }

			fx.ch.deliver(in)

			if tc.wantNotice != "" {
				fx.ch.waitFor(t, "dm1", func(m channel.Outbound) bool { return m.Text == tc.wantNotice })
			}
			if tc.wantTurn {
				fx.sender.nextStream(t)
				return
			}
			fx.sender.wantIdle(t)
		})
	}
}

func TestChatTurnsShowsStreamError(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	fx.sender.streamErr = errors.New("agent unreachable")

	fx.ch.write("telegram:1", "dm1", "hello")

	fx.ch.waitFor(t, "dm1", func(m channel.Outbound) bool { return m.Text == "⚠ agent unreachable" })
}

func TestChatTurnsAnswerThatFailsToRecordKeepsQuestionPending(t *testing.T) {
	t.Parallel()
	fx := newTurnsFixture(t)
	rec := fx.askPendingQuestion(t, "dm1", a2a.TaskStateInputRequired)
	rec.ID = "missing1"
	if err := fx.store.AddPendingQuestion(fx.ctx, "fake", "dm1", rec.ID); err != nil {
		t.Fatalf("AddPendingQuestion() error = %v", err)
	}
	q := approvals.FromRecord(rec)
	in := channel.Inbound{Channel: "fake", Conv: "dm1", Text: "approve"}

	fx.turns.answerIn(fx.ctx, fx.ch, in, auth.Owner(), rec, q, q.Options[0])

	fx.ch.waitFor(t, "dm1", hasText("Couldn't record the answer. Try again."))
	if ids, err := fx.store.PendingQuestions(fx.ctx, "fake", "dm1"); err != nil || !slices.Contains(ids, rec.ID) {
		t.Fatalf("PendingQuestions() = %q, %v, want %q among them", ids, err, rec.ID)
	}
}
