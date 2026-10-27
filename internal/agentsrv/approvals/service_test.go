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

package approvals

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// sendingHandler answers SendMessage with err, recording the caller.
type sendingHandler struct {
	a2asrv.RequestHandler
	err    error
	caller auth.Principal
}

func (h *sendingHandler) SendMessage(ctx context.Context, _ *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	h.caller, _ = auth.Caller(ctx)
	return nil, h.err
}

func newService(t *testing.T, h a2asrv.RequestHandler) *Service {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return &Service{Store: s, Handler: h, Approvers: func() []string { return []string{"pair:bob"} }}
}

func TestAnswerUndeliveredStaysOpen(t *testing.T) {
	t.Parallel()
	h := &sendingHandler{err: errors.New("agent unreachable")}
	svc := newService(t, h)
	ctx := context.Background()
	q := FromMessage(permissionMessage(t))
	rec, err := svc.NewRecord(ctx, "t1", "c1", "", q)
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}

	if err := svc.Answer(ctx, rec, auth.Owner(), q.Options[0]); err == nil {
		t.Fatalf("Answer() with a failing delivery error = nil, want error")
	}

	got, err := svc.Store.QuestionByID(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AnsweredBy != "" {
		t.Fatalf("question answered by %q after a failed delivery, want it open", got.AnsweredBy)
	}
	h.err = nil
	if err := svc.Answer(ctx, rec, auth.Owner(), q.Options[0]); err != nil {
		t.Fatalf("Answer() again error = %v, want nil", err)
	}
}

func TestAnswerOfApproverIsSentAsOwner(t *testing.T) {
	t.Parallel()
	h := &sendingHandler{}
	svc := newService(t, h)
	ctx := context.Background()
	q := FromMessage(permissionMessage(t))
	rec, err := svc.NewRecord(ctx, "t1", "c1", "", q)
	if err != nil {
		t.Fatalf("NewRecord() error = %v", err)
	}

	if err := svc.Answer(ctx, rec, auth.Principal{ID: "pair:bob", Roles: []string{auth.RoleMember}}, q.Options[0]); err != nil {
		t.Fatalf("Answer() error = %v", err)
	}

	if h.caller.ID != auth.OwnerID || h.caller.OnBehalfOf != "pair:bob" {
		t.Fatalf("answer sent as %+v, want the owner on behalf of pair:bob", h.caller)
	}
}
