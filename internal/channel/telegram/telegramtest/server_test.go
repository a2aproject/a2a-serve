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

package telegramtest_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/channel/telegram/telegramtest"
)

func newBot(t *testing.T, url string) *bot.Bot {
	t.Helper()
	b, err := bot.New("123:abc", bot.WithServerURL(url), bot.WithSkipGetMe())
	if err != nil {
		t.Fatalf("bot.New() error = %v", err)
	}
	return b
}

func TestServerForwardsToUpstream(t *testing.T) {
	t.Parallel()
	upstream := telegramtest.NewServer(t, telegramtest.Options{})
	upstream.Fail("sendMessage", telegramtest.Failure{Status: http.StatusBadRequest, OnlyWith: "MarkdownV2",
		Body: `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`})
	api := telegramtest.NewServer(t, telegramtest.Options{Upstream: upstream.URL})
	b := newBot(t, api.URL)
	ctx := t.Context()

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: 42, Text: "*bold", ParseMode: models.ParseModeMarkdown}); err == nil {
		t.Fatalf("SendMessage() error = nil, want the upstream's error")
	}
	sent, err := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: 42, Text: "hello"})
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	if _, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: 42, MessageID: sent.ID, Text: "hello again"}); err != nil {
		t.Fatalf("EditMessageText() error = %v", err)
	}
	if _, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: 42, MessageID: sent.ID, Text: "hello again"}); err == nil {
		t.Fatalf("EditMessageText() with the same text error = nil, want message is not modified")
	}

	want := []telegramtest.Message{{ID: sent.ID, ChatID: 42, Text: "hello again", Edits: 1}}
	if diff := cmp.Diff(want, api.Messages()); diff != "" {
		t.Fatalf("Server.Messages() wrong result (-want +got) diff = %s", diff)
	}
	if diff := cmp.Diff(want, upstream.Messages()); diff != "" {
		t.Fatalf("upstream Server.Messages() wrong result (-want +got) diff = %s", diff)
	}
	wantErrs := []string{"sendMessage: Bad Request: can't parse entities", "editMessageText: Bad Request: message is not modified"}
	if diff := cmp.Diff(wantErrs, api.UpstreamErrors()); diff != "" {
		t.Fatalf("Server.UpstreamErrors() wrong result (-want +got) diff = %s", diff)
	}
}

func TestServerPlaysUser(t *testing.T) {
	t.Parallel()
	api := telegramtest.NewServer(t, telegramtest.Options{})
	anna := telegramtest.User{ID: 42, FirstName: "Anna"}
	got := make(chan *models.Update, 2)
	b, err := bot.New("123:abc", bot.WithServerURL(api.URL), bot.WithSkipGetMe(), bot.WithNotAsyncHandlers(),
		bot.WithDefaultHandler(func(_ context.Context, _ *bot.Bot, u *models.Update) { got <- u }))
	if err != nil {
		t.Fatalf("bot.New() error = %v", err)
	}
	go b.Start(t.Context())

	api.Say(anna, "hi")
	if u := receive(t, got); u.Message == nil || u.Message.Text != "hi" || u.Message.From.ID != 42 {
		t.Fatalf("update = %+v, want message hi from 42", u)
	}
	sent, err := b.SendMessage(t.Context(), &bot.SendMessageParams{ChatID: 42, Text: "Push?", ReplyMarkup: &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{{Text: "Allow", CallbackData: "a"}, {Text: "Deny", CallbackData: "d"}}},
	}})
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	m, err := api.WaitMessage(ctx, func(m telegramtest.Message) bool { return m.ID == sent.ID })
	if err != nil {
		t.Fatalf("Server.WaitMessage() error = %v", err)
	}
	if err := api.Press(anna, m, "Maybe"); err == nil {
		t.Fatalf("Server.Press(Maybe) error = nil, want no such button")
	}
	if err := api.Press(anna, m, "Deny"); err != nil {
		t.Fatalf("Server.Press(Deny) error = %v", err)
	}
	if u := receive(t, got); u.CallbackQuery == nil || u.CallbackQuery.Data != "d" || u.CallbackQuery.Message.Message.ID != sent.ID {
		t.Fatalf("update = %+v, want callback d on message %d", u, sent.ID)
	}
}

func receive(t *testing.T, got <-chan *models.Update) *models.Update {
	t.Helper()
	select {
	case u := <-got:
		return u
	case <-time.After(10 * time.Second):
		t.Fatalf("no update within 10s")
		return nil
	}
}

func TestMessagePlain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  telegramtest.Message
		want string
	}{
		{name: "MarkdownV2", msg: telegramtest.Message{Text: `*Bold* and \_x\_ \(1\)\.`, ParseMode: "MarkdownV2"}, want: "Bold and _x_ (1)."},
		{name: "plain text", msg: telegramtest.Message{Text: "*not bold*"}, want: "*not bold*"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.msg.Plain(); got != tc.want {
				t.Fatalf("Message.Plain() = %q, want %q", got, tc.want)
			}
		})
	}
}
