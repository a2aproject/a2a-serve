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

// Package messenger defines the messengers an agent server is reachable from.
// Implementations live in subpackages; the server's router turns their
// messages into A2A tasks.
package messenger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Messenger is one messenger account of the agent, such as its Telegram bot.
type Messenger interface {
	// Name is the messenger name, such as telegram.
	Name() string
	// Caps reports what the messenger supports.
	Caps() Caps
	// Run holds an outbound connection to the messenger and delivers
	// messages to in until ctx ends. A messenger that reconnects by itself
	// calls report with the error when it loses the connection, and with
	// nil once it is connected again.
	Run(ctx context.Context, in chan<- Inbound, report func(error)) error
	// Send sends a message and returns a reference to it.
	Send(ctx context.Context, m Outbound) (Ref, error)
	// Edit replaces a sent message. Only called if Caps().Edit.
	Edit(ctx context.Context, ref Ref, m Outbound) error
}

// Describer is implemented by messengers which can tell the user how to reach
// them and how to link an identity with a pairing code.
type Describer interface {
	// Describe returns the account, such as @my_coder_bot.
	Describe() string
	// LinkHint tells a person how to link themselves with code.
	LinkHint(code string) string
}

// Validator is implemented by messengers which can check their credentials
// before they are saved.
type Validator interface {
	// Validate reports whether the messenger's credentials work.
	Validate(ctx context.Context) error
}

// Linker is a messenger that keeps state only for senders who were let in.
type Linker interface {
	// Linked tells the messenger that the sender of in linked their chat
	// account with in, which skipped [Inbound.Prepare].
	Linked(ctx context.Context, in Inbound) error
}

// Validate checks the credentials of ch if it is a [Validator].
func Validate(ctx context.Context, ch Messenger) error {
	if v, ok := ch.(Validator); ok {
		return v.Validate(ctx)
	}
	return nil
}

// RateLimitError is returned by a messenger when the messenger asks the
// sender to wait before the next call.
type RateLimitError struct {
	RetryAfter time.Duration
	Err        error
}

func (e *RateLimitError) Error() string { return e.Err.Error() }

func (e *RateLimitError) Unwrap() error { return e.Err }

// AttachmentError is returned by [Inbound.Prepare] for an attachment that
// couldn't be downloaded. The message is still handled without it.
type AttachmentError struct {
	Name string
	Err  error
}

func (e *AttachmentError) Error() string { return fmt.Sprintf("downloading %s: %v", e.Name, e.Err) }

func (e *AttachmentError) Unwrap() error { return e.Err }

// TooLargeError is returned by [ReadLimited] for data over the limit.
type TooLargeError struct {
	Limit int64
}

func (e *TooLargeError) Error() string { return "file is " + e.Over() }

// Over describes the limit, such as "over 20 MiB".
func (e *TooLargeError) Over() string {
	const mib = 1 << 20
	if e.Limit%mib == 0 {
		return fmt.Sprintf("over %d MiB", e.Limit/mib)
	}
	return fmt.Sprintf("over %d bytes", e.Limit)
}

// ReadLimited reads r to its end, and returns a [TooLargeError] if it has more
// than limit bytes. Cutting the data at limit instead would hand a corrupt
// file to the agent.
func ReadLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, &TooLargeError{Limit: limit}
	}
	return data, nil
}

// DroppedError is returned by [Inbound.Prepare] when the message can't be
// handled. Reason is shown to its sender.
type DroppedError struct {
	Reason string
	Err    error
}

func (e *DroppedError) Error() string { return fmt.Sprintf("%s: %v", e.Reason, e.Err) }

func (e *DroppedError) Unwrap() error { return e.Err }

// FailedAttachments returns the attachment errors within err, which may be
// joined.
func FailedAttachments(err error) []*AttachmentError {
	var out []*AttachmentError
	var walk func(error)
	walk = func(err error) {
		switch e := err.(type) {
		case nil:
		case *AttachmentError:
			out = append(out, e)
		case interface{ Unwrap() []error }:
			for _, sub := range e.Unwrap() {
				walk(sub)
			}
		default:
			walk(errors.Unwrap(err))
		}
	}
	walk(err)
	return out
}

// Caps are what a messenger supports.
type Caps struct {
	Edit    bool
	Buttons bool
	Threads bool
	Typing  bool
	// MaxText is the longest text of one message; longer text is split.
	MaxText  int
	Markdown bool
	// Stream is true when output should be streamed by editing a message,
	// false when only questions and the final result are sent.
	Stream bool
}

// ConvKey identifies a conversation within a messenger: a chat, a thread or
// the root of an email thread.
type ConvKey string

// File is a file attached to a message.
type File struct {
	Name      string
	MediaType string
	Data      []byte
}

// Action is a click on a question's button.
type Action struct {
	QuestionID string
	Option     int
}

// Inbound is a message from a messenger.
type Inbound struct {
	Messenger string
	// MessageID is the dedup key; it becomes the A2A messageId.
	MessageID string
	// Identity is the sender's chat account: telegram:<user id>,
	// discord:<user id>, or an email sender's address.
	Identity string
	// Name is the sender's display name.
	Name string
	Conv ConvKey
	// Direct is true for a one-to-one chat with the sender.
	Direct bool
	Text   string
	Files  []File
	Action *Action
	// Authentic is true when the messenger verified the sender: always for
	// bot APIs, after DMARC or DKIM for email.
	Authentic bool
	// ReplyTo is the message this one answers, if the messenger tracks it.
	ReplyTo Ref
	// Prepare, if set, finishes the message once its sender is known to be
	// allowed in: it downloads files and may move Conv to a new thread. It
	// keeps strangers from making the server do work on their behalf. It
	// returns [AttachmentError] for files it couldn't download and
	// [DroppedError] if the message must not be handled.
	Prepare func(ctx context.Context, in *Inbound) error
}

// Outbound is a message to a messenger.
type Outbound struct {
	Conv  ConvKey
	Text  string
	Files []File
	// Question adds one button per option, if the messenger has buttons.
	Question *Question
	// ReplyTo threads the message under another one, if supported.
	ReplyTo Ref
	// Subject is used by messengers with subjects, such as email.
	Subject string
	// Literal shows Text exactly as written: the messenger applies no markup,
	// so that links and formatting can't hide any of it. It is set for
	// approvals, whose text the owner has to read in full.
	Literal bool
}

// Command is a chat command that messengers with a command menu list.
type Command struct {
	Name        string
	Description string
	// Choices are the values its argument takes, if it takes one.
	Choices []string
}

// Question is a decision offered with buttons.
type Question struct {
	ID      string
	Options []string
}

// Ref identifies a sent message, for edits and replies.
type Ref struct {
	Conv ConvKey
	ID   string
}

// ActionData encodes a button payload: h:<question id>:<option index>.
// It must stay within Telegram's 64-byte callback data limit.
func ActionData(questionID string, option int) string {
	return "h:" + questionID + ":" + strconv.Itoa(option)
}

// ParseActionData decodes a button payload made by [ActionData].
func ParseActionData(s string) (*Action, error) {
	rest, ok := strings.CutPrefix(s, "h:")
	if !ok {
		return nil, fmt.Errorf("not a question action: %q", s)
	}
	id, idx, ok := strings.Cut(rest, ":")
	n, err := strconv.Atoi(idx)
	if !ok || err != nil || id == "" || n < 0 {
		return nil, fmt.Errorf("malformed question action: %q", s)
	}
	return &Action{QuestionID: id, Option: n}, nil
}

// Split cuts text into pieces of at most limit bytes, preferring line breaks.
func Split(text string, limit int) []string {
	if limit <= 0 || len(text) <= limit {
		return []string{text}
	}
	var out []string
	for len(text) > limit {
		cut := strings.LastIndex(text[:limit], "\n")
		if cut < limit/2 {
			cut = limit
			for cut > 0 && !isRuneStart(text[cut]) {
				cut--
			}
		}
		out = append(out, text[:cut])
		text = strings.TrimPrefix(text[cut:], "\n")
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Truncate cuts text to at most limit bytes without splitting a character.
func Truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !isRuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// KV persists small messenger state, such as the last message seen, across
// restarts.
type KV interface {
	// Get returns the value of key, or false if it is not set.
	Get(ctx context.Context, key string) (string, bool, error)
	// Set stores the value of key.
	Set(ctx context.Context, key, value string) error
	// Delete removes key. Removing a key that is not set is a no-op.
	Delete(ctx context.Context, key string) error
}
