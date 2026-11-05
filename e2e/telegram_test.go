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

//go:build e2e

package e2e

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-serve/internal/channel/telegram"
	"github.com/a2aproject/a2a-serve/internal/channel/telegram/telegramtest"
)

// Live Telegram runs need a bot token and the ID of a private chat with the
// bot: the user's Telegram ID, after they sent the bot /start once.
const (
	envLiveTelegramToken = "E2E_TELEGRAM_BOT_TOKEN"
	envLiveTelegramChat  = "E2E_TELEGRAM_CHAT_ID"
)

func TestTelegram(t *testing.T) {
	t.Parallel()
	api := telegramtest.NewServer(t, telegramtest.Options{})
	testTelegramJourney(t, api, "123:abc", telegramtest.User{ID: 42, FirstName: "Anna", Username: "anna"}, 30*time.Second)
}

// TestTelegramLive sends the bot's messages through the real Bot API, which
// checks their formatting and shows them in the chat. The fake still plays
// the user, since a bot can't receive messages from another bot.
func TestTelegramLive(t *testing.T) {
	token, chat := os.Getenv(envLiveTelegramToken), os.Getenv(envLiveTelegramChat)
	if token == "" || chat == "" {
		t.Skipf("set %s and %s to run against Telegram", envLiveTelegramToken, envLiveTelegramChat)
	}
	id, err := strconv.ParseInt(chat, 10, 64)
	if err != nil {
		t.Fatalf("%s = %q: %v", envLiveTelegramChat, chat, err)
	}
	api := telegramtest.NewServer(t, telegramtest.Options{Upstream: "https://api.telegram.org"})
	testTelegramJourney(t, api, token, telegramtest.User{ID: id, FirstName: "a2a-serve e2e"}, 2*time.Minute)
	// The channel ignores unchanged edits, and the server retries rate
	// limited calls.
	var errs []string
	for _, e := range api.UpstreamErrors() {
		if !strings.Contains(e, "message is not modified") && !strings.Contains(e, "Too Many Requests") {
			errs = append(errs, e)
		}
	}
	if len(errs) > 0 {
		t.Fatalf("Telegram rejected calls:\n%s", strings.Join(errs, "\n"))
	}
}

func testTelegramJourney(t *testing.T, api *telegramtest.Server, token string, me telegramtest.User, timeout time.Duration) {
	t.Helper()
	owner := newUser(t)
	owner.env = append(owner.env, telegram.EnvServerURL+"="+api.URL, "E2E_BOT_TOKEN="+token)
	coder := owner.startAgent("coder")
	var added struct {
		OwnerLink *struct {
			Code string `json:"code"`
		} `json:"ownerLink"`
	}
	coder.serveJSON(&added, "channel", "add", "telegram", "--token", "env:E2E_BOT_TOKEN")
	if added.OwnerLink == nil {
		t.Fatalf("a2a-serve channel add telegram printed no owner link")
	}
	c := &telegramChat{t: t, api: api, me: me, timeout: timeout}

	c.say("/start "+added.OwnerLink.Code, "Linked as owner. Write to talk to coder.")
	c.say("hello", "Hello from fakeacp.")

	asked := c.sayAndWait("ask", func(m telegramtest.Message) bool { return len(m.Buttons) > 0 })
	c.press(asked, "Allow", "Pushed (allow).")

	md := c.sayAndWait("markdown", func(m telegramtest.Message) bool { return strings.Contains(m.Plain(), "Done.") })
	if md.ParseMode != "MarkdownV2" {
		t.Fatalf("markdown reply parse mode = %q, want MarkdownV2; text %q", md.ParseMode, md.Text)
	}

	c.say("remember banana", "OK.")
	c.say("recall", "You said: banana")
	c.say("/new", "Started a new conversation.")
	c.say("recall", "You said:")

	c.sayAndWait("slow", func(m telegramtest.Message) bool { return strings.Contains(m.Plain(), "...") })
	c.say("/stop", "Stopped.")

	last := c.lastID()
	coder.serve("schedule", "add", "ping", "--cron", "@daily", "--prompt", "scheduled ping", "--chat", "telegram")
	coder.serve("schedule", "run", "ping")
	c.wait(last, func(m telegramtest.Message) bool { return strings.Contains(m.Plain(), "scheduled ping") })
}

// telegramChat is the private chat of a user with the bot.
type telegramChat struct {
	t       *testing.T
	api     *telegramtest.Server
	me      telegramtest.User
	timeout time.Duration
}

// say sends text and waits for a new message of the bot that reads want.
func (c *telegramChat) say(text, want string) {
	c.t.Helper()
	c.sayAndWait(text, func(m telegramtest.Message) bool { return strings.TrimSpace(m.Plain()) == want })
}

// sayAndWait sends text and waits for a new message of the bot that
// satisfies match.
func (c *telegramChat) sayAndWait(text string, match func(telegramtest.Message) bool) telegramtest.Message {
	c.t.Helper()
	last := c.lastID()
	c.api.Say(c.me, text)
	return c.wait(last, match)
}

// press presses a button of m and waits for a new message that reads want.
func (c *telegramChat) press(m telegramtest.Message, label, want string) {
	c.t.Helper()
	last := c.lastID()
	if err := c.api.Press(c.me, m, label); err != nil {
		c.t.Fatal(err)
	}
	c.wait(last, func(m telegramtest.Message) bool { return strings.TrimSpace(m.Plain()) == want })
}

// wait waits for a message of the bot after message last that satisfies match.
func (c *telegramChat) wait(last int, match func(telegramtest.Message) bool) telegramtest.Message {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	m, err := c.api.WaitMessage(ctx, func(m telegramtest.Message) bool { return m.ID > last && match(m) })
	if err != nil {
		c.t.Fatal(err)
	}
	return m
}

func (c *telegramChat) lastID() int {
	last := 0
	for _, m := range c.api.Messages() {
		last = max(last, m.ID)
	}
	return last
}
