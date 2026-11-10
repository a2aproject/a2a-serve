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

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/messenger"
)

// command is a chat command. /help, the messengers' command menus and the
// greeting of /start list the commands from [commands].
type command struct {
	name string
	// help is the line /help and the command menus show; empty keeps the
	// command out of them.
	help string
	// arg is set for a command that takes an argument; a message that adds
	// text to a command without one goes to the agent.
	arg bool
	// choices are the values of its argument, which Discord offers.
	choices []string
	// waits makes the command wait for the turn of its conversation, so
	// that it doesn't overtake earlier messages. Others run at once, such
	// as /stop, which must not wait for the task it stops.
	waits  bool
	handle func(c *ChatTurns, ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal, arg string)
}

// commands are the chat commands, in the order /help lists them.
var commands []command

func init() {
	commands = []command{
		{name: "new", help: "Start a new conversation", waits: true, handle: (*ChatTurns).newConversation},
		{name: "stop", help: "Stop the current task", handle: (*ChatTurns).stopCommand},
		{name: "allowed", help: "Show and remove what runs without asking for you", arg: true, handle: (*ChatTurns).allowedCommand},
		{name: "voice", help: "Turn voice replies on or off for you", arg: true, choices: []string{"on", "off"}, handle: (*ChatTurns).voiceCommand},
		{name: "help", help: "List the commands", handle: (*ChatTurns).helpCommand},
		{name: "start", waits: true, handle: (*ChatTurns).startCommand},
	}
}

// Commands are the commands the messengers show in their command menus.
func Commands() []messenger.Command {
	var out []messenger.Command
	for _, cmd := range commands {
		if cmd.help != "" {
			out = append(out, messenger.Command{Name: cmd.name, Description: cmd.help, Choices: cmd.choices})
		}
	}
	return out
}

// parseCommand returns the command text invokes, as /name or /name@bot,
// and its argument.
func parseCommand(text string) (*command, string, bool) {
	first, arg, _ := strings.Cut(strings.TrimSpace(text), " ")
	arg = strings.TrimSpace(arg)
	for i := range commands {
		if isCommand(first, commands[i].name) && (arg == "" || commands[i].arg) {
			return &commands[i], arg, true
		}
	}
	return nil, "", false
}

// helpText lists the commands with their help lines.
func helpText() string {
	var b strings.Builder
	b.WriteString("Commands:")
	for _, cmd := range Commands() {
		fmt.Fprintf(&b, "\n/%s - %s", cmd.Name, cmd.Description)
	}
	return b.String()
}

func (c *ChatTurns) helpCommand(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, _ auth.Principal, _ string) {
	c.reply(ctx, ch, in, helpText())
}

func (c *ChatTurns) startCommand(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, _ auth.Principal, _ string) {
	c.reply(ctx, ch, in, fmt.Sprintf("Hi! I'm %s. Write what you need.\n\n%s", c.config().Name, helpText()))
}

func (c *ChatTurns) newConversation(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, _ auth.Principal, _ string) {
	if err := c.store.SetContext(ctx, in.Messenger, string(in.Conv), a2a.NewContextID()); err != nil {
		slog.Error("starting a new context", "error", err)
		return
	}
	c.reply(ctx, ch, in, "Started a new conversation.")
}

func (c *ChatTurns) stopCommand(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal, _ string) {
	c.stop(ctx, ch, in, p)
}

func (c *ChatTurns) allowedCommand(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal, _ string) {
	if c.prefs == nil {
		c.reply(ctx, ch, in, "This agent doesn't keep what you allowed.")
		return
	}
	c.handleAllowed(ctx, ch, in, p)
}

// voiceCommand answers /voice on, /voice off and /voice, which tells
// whether voice replies are on.
func (c *ChatTurns) voiceCommand(ctx context.Context, ch messenger.Messenger, in messenger.Inbound, p auth.Principal, arg string) {
	if p.Role == auth.RoleGuest {
		c.reply(ctx, ch, in, "Voice replies are for people the owner linked. Ask the owner for a code.")
		return
	}
	arg = strings.ToLower(arg)
	if arg == "on" && !c.config().Voice.On() {
		if p.IsOwner() {
			c.reply(ctx, ch, in, "Voice replies aren't set up on this agent. Set up voice with `a2a-serve config set --voice <service>`; see `a2a-serve help config set`.")
			return
		}
		c.reply(ctx, ch, in, "Voice replies aren't set up on this agent. Ask its owner to set them up.")
		return
	}
	switch arg {
	case "on", "off":
		if err := c.store.SetVoiceReplies(ctx, p.ID, arg == "on"); err != nil {
			slog.Error("saving voice replies", "user", p.ID, "error", err)
			c.reply(ctx, ch, in, "Couldn't save it. Try again.")
			return
		}
		if arg == "on" {
			c.reply(ctx, ch, in, "Voice replies are on. I'll send a voice note with each answer. /voice off turns them off.")
			return
		}
		c.reply(ctx, ch, in, "Voice replies are off. /voice on turns them on.")
	case "":
		on, err := c.store.VoiceReplies(ctx, p.ID)
		if err != nil {
			slog.Error("loading voice replies", "user", p.ID, "error", err)
			c.reply(ctx, ch, in, "Couldn't load it. Try again.")
			return
		}
		if on {
			c.reply(ctx, ch, in, "Voice replies are on. /voice off turns them off.")
			return
		}
		c.reply(ctx, ch, in, "Voice replies are off. /voice on turns them on.")
	default:
		c.reply(ctx, ch, in, "Send /voice on or /voice off.")
	}
}

// voiceReplies reports whether p gets voice replies: voice is set up and p
// turned it on.
func (c *ChatTurns) voiceReplies(ctx context.Context, p auth.Principal) bool {
	if !c.config().Voice.On() || p.Role == auth.RoleGuest {
		return false
	}
	on, err := c.store.VoiceReplies(ctx, p.ID)
	if err != nil {
		slog.Warn("loading voice replies", "user", p.ID, "error", err)
	}
	return on
}
