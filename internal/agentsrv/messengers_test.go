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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/agenterr"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// fakeBotAPI accepts the bot token "good" and rejects every other one.
func fakeBotAPI(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{"ok":true,"result":true}`
		switch {
		case !strings.HasPrefix(r.URL.Path, "/botgood/"):
			w.WriteHeader(http.StatusUnauthorized)
			body = `{"ok":false,"error_code":401,"description":"Unauthorized"}`
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			body = `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"Coder","username":"my_coder_bot"}}`
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			time.Sleep(100 * time.Millisecond)
			body = `{"ok":true,"result":[]}`
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("writing %s: %v", r.URL.Path, err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestAddMessengerSavesOnlyValidTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		token         string
		wantErr       bool
		wantMessenger *agentdir.TelegramConfig
		wantSecrets   agentdir.Secrets
	}{
		{name: "invalid token", token: "bad", wantErr: true, wantSecrets: agentdir.Secrets{}},
		{
			name:          "valid token",
			token:         "good",
			wantMessenger: &agentdir.TelegramConfig{Token: "secret:" + agentdir.SecretTelegram},
			wantSecrets:   agentdir.Secrets{agentdir.SecretTelegram: "good"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := startServer(t, serverOpts{name: "assistant", telegramAPI: fakeBotAPI(t)})
			_, err := controlapi.NewClient(ts.dir).AddMessenger(context.Background(), controlapi.MessengerRequest{
				Kind:    agentdir.MessengerTelegram,
				Token:   "secret:" + agentdir.SecretTelegram,
				Secrets: map[string]string{agentdir.SecretTelegram: tc.token},
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("AddMessenger() error = %v, want error %v", err, tc.wantErr)
			}
			conf, err := ts.dir.LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantMessenger, conf.Messengers.Telegram); diff != "" {
				t.Fatalf("LoadConfig() wrong telegram messenger (-want +got) diff = %s", diff)
			}
			secrets, err := ts.dir.LoadSecrets()
			if err != nil {
				t.Fatalf("LoadSecrets() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantSecrets, secrets); diff != "" {
				t.Fatalf("LoadSecrets() wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}

func TestAddMessengerReturnsOwnerLink(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant", telegramAPI: fakeBotAPI(t)})

	st, err := controlapi.NewClient(ts.dir).AddMessenger(context.Background(), controlapi.MessengerRequest{
		Kind:    agentdir.MessengerTelegram,
		Token:   "secret:" + agentdir.SecretTelegram,
		Secrets: map[string]string{agentdir.SecretTelegram: "good"},
	})

	if err != nil {
		t.Fatalf("AddMessenger() error = %v", err)
	}
	if l := st.OwnerLink; l == nil || l.Code == "" || !strings.Contains(l.Hint, l.Code) || time.Until(l.Expires) <= 0 || strings.Contains(st.Account, l.Code) {
		t.Fatalf("AddMessenger() = %+v, want an owner link with a code, apart from the account", st)
	}
}

func TestDeleteMessengerNotConfigured(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})

	_, err := controlapi.NewClient(ts.dir).DeleteMessenger(context.Background(), "telegram")

	var nf *agenterr.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("DeleteMessenger() error = %v, want an agenterr.NotFoundError", err)
	}
	if diff := cmp.Diff(&agenterr.NotFoundError{Kind: "messenger", Name: "telegram"}, nf); diff != "" {
		t.Fatalf("DeleteMessenger() wrong error (-want +got) diff = %s", diff)
	}
}

func TestAddMessengerWhileReadingStatus(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{name: "assistant", telegramAPI: fakeBotAPI(t)})
	ctx := context.Background()
	client := controlapi.NewClient(ts.dir)
	done := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		defer close(errc)
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := client.Status(ctx); err != nil {
				errc <- err
				return
			}
		}
	}()
	for range 3 {
		if _, err := client.AddMessenger(ctx, controlapi.MessengerRequest{
			Kind:    agentdir.MessengerTelegram,
			Token:   "secret:" + agentdir.SecretTelegram,
			Secrets: agentdir.Secrets{agentdir.SecretTelegram: "good"},
		}); err != nil {
			t.Fatalf("AddMessenger() error = %v", err)
		}
	}
	close(done)
	if err := <-errc; err != nil {
		t.Fatalf("Status() error = %v", err)
	}
}

func TestMessengerChangesReportAccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		policy     agentdir.PolicyConfig
		wantChange bool
	}{
		{name: "public bot open to anyone", policy: agentdir.PolicyConfig{PublicAllowIncoming: agentdir.AccessAnyone}, wantChange: true},
		{name: "paired callers only"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := startServer(t, serverOpts{name: "assistant", telegramAPI: fakeBotAPI(t), public: publicNet(), configure: func(c *agentdir.Config) { c.Policy = tc.policy }})
			client := controlapi.NewClient(ts.dir)
			ctx := context.Background()
			before, err := client.Status(ctx)
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}

			added, err := client.AddMessenger(ctx, controlapi.MessengerRequest{
				Kind:    agentdir.MessengerTelegram,
				Token:   "secret:" + agentdir.SecretTelegram,
				Secrets: map[string]string{agentdir.SecretTelegram: "good"},
			})
			if err != nil {
				t.Fatalf("AddMessenger() error = %v", err)
			}
			removed, err := client.DeleteMessenger(ctx, agentdir.MessengerTelegram)
			if err != nil {
				t.Fatalf("DeleteMessenger() error = %v", err)
			}

			if !tc.wantChange {
				if added.Access != nil || removed.Access != nil {
					t.Fatalf("AddMessenger().Access = %+v, DeleteMessenger().Access = %+v, want both nil", added.Access, removed.Access)
				}
				return
			}
			if added.Access == nil || cmp.Equal(before.Access.Summary, added.Access.Summary) {
				t.Fatalf("AddMessenger().Access = %+v, want who may call with the messenger", added.Access)
			}
			if removed.Access == nil {
				t.Fatalf("DeleteMessenger().Access = nil, want who may call without the messenger")
			}
			if diff := cmp.Diff(before.Access, removed.Access); diff != "" {
				t.Fatalf("DeleteMessenger().Access wrong result (-want +got) diff = %s", diff)
			}
		})
	}
}
