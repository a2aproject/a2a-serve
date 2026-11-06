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

package messenger

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agentsrv/approvals"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/channel/channeltest"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

const waitTimeout = 10 * time.Second

var testTimings = Timings{
	EditInterval:             50 * time.Millisecond,
	AnonymousQuestionTimeout: 5 * time.Second,
	SendRetry:                time.Millisecond,
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return st
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stubChannel is a channel whose Run is scripted.
type stubChannel struct {
	name string
	run  func(ctx context.Context, in chan<- channel.Inbound, report func(error)) error
}

func (s *stubChannel) Name() string { return s.name }

func (s *stubChannel) Caps() channel.Caps { return channel.Caps{} }

func (s *stubChannel) Run(ctx context.Context, in chan<- channel.Inbound, report func(error)) error {
	return s.run(ctx, in, report)
}

func (s *stubChannel) Send(context.Context, channel.Outbound) (channel.Ref, error) {
	return channel.Ref{}, nil
}

func (s *stubChannel) Edit(context.Context, channel.Ref, channel.Outbound) error { return nil }

// describedStub is a stubChannel that tells how to link to it.
type describedStub struct{ stubChannel }

func (d *describedStub) Describe() string { return "@" + d.name }

func (d *describedStub) LinkHint(code string) string { return "/start " + code }

func blockUntilDone(ctx context.Context, _ chan<- channel.Inbound, _ func(error)) error {
	<-ctx.Done()
	return nil
}

// tweakedChannel is a channeltest.Channel that can't edit or fails to send.
type tweakedChannel struct {
	*channeltest.Channel
	noEdit  bool
	sendErr error
}

func (c *tweakedChannel) Caps() channel.Caps {
	caps := c.Channel.Caps()
	if c.noEdit {
		caps.Edit = false
	}
	return caps
}

func (c *tweakedChannel) Send(ctx context.Context, m channel.Outbound) (channel.Ref, error) {
	if c.sendErr != nil {
		return channel.Ref{}, c.sendErr
	}
	return c.Channel.Send(ctx, m)
}

type readyWaiter interface{ WaitReady(t testing.TB) }

// chatFixture is the parts the chat code works with, over a temporary store.
type chatFixture struct {
	ctx        context.Context
	store      *store.Store
	tasks      *store.TaskStore
	chain      *auth.Chain
	records    *approvals.Records
	supervisor *ChannelSupervisor
	chat       *ChatApprovals
	bus        *Bus
	queue      *ConversationQueue
	timings    Timings
}

// newChatFixture runs chs in a supervisor and returns a ChatApprovals for
// them that has the --owner-chat setting ownerChat.
func newChatFixture(t *testing.T, ownerChat string, chs ...channel.Channel) *chatFixture {
	t.Helper()
	st := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	fx := &chatFixture{ctx: ctx, store: st, bus: NewBus(), queue: NewConversationQueue(), timings: testTimings}
	fx.tasks = st.Tasks("", func(context.Context) (string, bool, error) { return "", true, nil })
	fx.chain = &auth.Chain{Store: st, Policy: func() auth.Policy { return auth.Policy{} }}
	fx.records = approvals.NewRecords(approvals.RecordsDeps{
		Store:   st,
		Tasks:   fx.tasks,
		Chain:   fx.chain,
		BaseURL: func() string { return "https://agent.example" },
	})
	fx.supervisor = NewChannelSupervisor(ctx, SupervisorDeps{Store: st, Logf: func(string, ...any) {}, RetryMin: time.Millisecond})
	t.Cleanup(func() {
		cancel()
		stopCtx, stop := context.WithTimeout(context.Background(), waitTimeout)
		defer stop()
		if err := fx.supervisor.StopAll(stopCtx); err != nil {
			t.Errorf("stopAll() error = %v", err)
		}
	})
	fx.chat = NewChatApprovals(ctx, ChatApprovalsDeps{
		Store:      st,
		Tasks:      fx.tasks,
		Records:    fx.records,
		Supervisor: fx.supervisor,
		Bus:        fx.bus,
		Config: func() *agentdir.Config {
			return &agentdir.Config{Name: "coder", Approvals: agentdir.ApprovalsConfig{OwnerChat: ownerChat}}
		},
		Anonymous: func(context.Context, string) bool { return false },
		Timings:   fx.timings,
	})
	for _, ch := range chs {
		fx.supervisor.Add(ch)
		if w, ok := ch.(readyWaiter); ok {
			w.WaitReady(t)
		}
	}
	return fx
}

type linkSpec struct {
	pairing  string
	role     string
	channel  string
	identity string
	conv     string
}

// putLink links a chat account to a pairing, which is created unless it is
// the local one.
func putLink(t *testing.T, st *store.Store, l linkSpec) {
	t.Helper()
	ctx := context.Background()
	if l.pairing != auth.LocalPairing {
		if _, err := st.EnsurePairing(ctx, store.Pairing{Name: l.pairing, Kind: pairapi.PairingPerson, Role: l.role}); err != nil {
			t.Fatalf("EnsurePairing() error = %v", err)
		}
	}
	link := store.Link{Channel: l.channel, Identity: l.identity, Pairing: l.pairing, Conv: l.conv}
	if err := st.PutLink(ctx, link, pairapi.PairingPerson); err != nil {
		t.Fatalf("PutLink() error = %v", err)
	}
}

func (fx *chatFixture) link(t *testing.T, l linkSpec) {
	t.Helper()
	putLink(t, fx.store, l)
}

// permissionMessage is the agent's question of tool call tool1.
func permissionMessage(t *testing.T) *a2a.Message {
	t.Helper()
	optionSchema := &jsonschema.Schema{Type: "string"}
	for _, o := range [][2]string{{"once", "Allow"}, {"always", "Always allow"}, {"reject", "Deny"}} {
		optionSchema.OneOf = append(optionSchema.OneOf, &jsonschema.Schema{Const: jsonschema.Ptr[any](o[0]), Title: o[1]})
	}
	schema := &jsonschema.Schema{Type: "object", Title: "git push", Properties: map[string]*jsonschema.Schema{"option": optionSchema}}
	part, err := structuredinput.NewSchemaPart("tool1", schema)
	if err != nil {
		t.Fatalf("structuredinput.NewSchemaPart() error = %v", err)
	}
	return a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("Run git push?"), part)
}

// putTask stores a task. A task that waits for input asks permissionMessage.
func (fx *chatFixture) putTask(t *testing.T, id a2a.TaskID, contextID string, state a2a.TaskState) {
	t.Helper()
	task := &a2a.Task{ID: id, ContextID: contextID, Status: a2a.TaskStatus{State: state}}
	if state == a2a.TaskStateInputRequired {
		task.Status.Message = permissionMessage(t)
	}
	if _, err := fx.tasks.Create(fx.ctx, task); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
}

func (fx *chatFixture) setTaskState(t *testing.T, id a2a.TaskID, contextID string, state a2a.TaskState) {
	t.Helper()
	task := &a2a.Task{ID: id, ContextID: contextID, Status: a2a.TaskStatus{State: state}}
	if _, err := fx.tasks.Update(fx.ctx, &taskstore.UpdateRequest{Task: task, PrevVersion: taskstore.TaskVersionMissing}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
}

// endExecution stores the state an execution of a task ended in, and tells
// the followers of its context, as the executor does.
func (fx *chatFixture) endExecution(t *testing.T, id a2a.TaskID, contextID string, state a2a.TaskState) {
	t.Helper()
	fx.setTaskState(t, id, contextID, state)
	fx.bus.ExecutionEnded(contextID)
}

// newQuestion stores the question of permissionMessage that user may answer.
func (fx *chatFixture) newQuestion(t *testing.T, taskID a2a.TaskID, contextID, user string) store.Question {
	t.Helper()
	rec, err := fx.records.NewRecord(fx.ctx, taskID, contextID, "", user, approvals.FromMessage(permissionMessage(t)))
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}
	return rec
}
