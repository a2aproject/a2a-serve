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

package pairings

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/pairapi"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// ErrInvalidCode is returned for unknown, used or expired codes.
var ErrInvalidCode = errors.New("invalid or expired code")

// ErrLocalCode is returned for a code of the local pairing redeemed for a
// token. Such codes only link chat accounts.
var ErrLocalCode = errors.New("this code links your chat account; send \"join <code>\" to the agent's bot")

// ErrUnverifiedSender is returned for a chat message whose sender the
// channel couldn't verify.
var ErrUnverifiedSender = errors.New("the sender of this message could not be verified")

// RateLimitError is returned to a client that failed to redeem codes too
// often.
type RateLimitError struct {
	// Wait is how long until the client may try again.
	Wait time.Duration
}

// Seconds is Wait in whole seconds, rounded up.
func (e *RateLimitError) Seconds() int { return int(math.Ceil(e.Wait.Seconds())) }

func (e *RateLimitError) Error() string {
	if n := e.Seconds(); n != 1 {
		return fmt.Sprintf("too many invalid pairing codes; try again in %d seconds", n)
	}
	return "too many invalid pairing codes; try again in 1 second"
}

// Via is how a code is redeemed: for a token at an agent URL, or for a
// chat link from a chat message.
type Via struct {
	// Client identifies the redeemer for the limit on failed attempts: its
	// address, or the source of a tunneled request. Empty is a client that
	// can't be told apart from others, and shares their backoff. Chat
	// messages are limited by their sender.
	Client string
	// URL is the agent URL a token is redeemed at.
	URL string
	// Kind is what the redeemer of a token is, one of the pairapi.Pairing*
	// kinds other than webhook. Empty is a person.
	Kind string
	// Chat is the message with the code, whose sender's chat account the
	// code links.
	Chat *channel.Inbound
}

// Redemption is what a code was exchanged for.
type Redemption struct {
	// Kind is pairapi.KindCaller for a token, pairapi.KindNode for a node
	// token, and empty for a chat link.
	Kind    string
	Pairing string
	Token   string
	// Principal is who the token or chat account acts as.
	Principal string
	// URL is where the token applies: the agent for callers, the node for
	// nodes.
	URL string
	// Node is the name of the webhook node of a node token.
	Node string
	// Role is the role of the pairing of a chat link.
	Role string
}

// Redeem exchanges a code for what via asks: a caller token, the node
// token of a webhook node, or a chat link. The code is consumed in the
// transaction that stores the result, so a failure leaves it pending.
// Failed attempts are limited per client.
func (s *Service) Redeem(ctx context.Context, code string, via Via) (*Redemption, error) {
	kind := pairapi.PairingPerson
	client := via.Client
	if via.Chat != nil {
		if !via.Chat.Authentic {
			return nil, ErrUnverifiedSender
		}
		client = "chat:" + via.Chat.Channel + ":" + via.Chat.Identity
	} else {
		kind = cmp.Or(via.Kind, pairapi.PairingPerson)
		if kind == pairapi.PairingWebhook || !slices.Contains(pairapi.PairingKinds, kind) {
			return nil, invalid("unknown kind %s", via.Kind)
		}
	}
	wait, done := s.deps.Limiter.attempt(client)
	if wait > 0 {
		return nil, &RateLimitError{Wait: wait}
	}
	var r Redemption
	c, err := s.deps.Store.RedeemCode(ctx, auth.NormalizeCode(code), func(c store.Code) (store.Grant, error) {
		return s.grant(ctx, c, via, kind, &r)
	})
	done(err == nil)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrCodeExpired) {
		return nil, ErrInvalidCode
	}
	if err != nil {
		return nil, err
	}
	r.Pairing = c.Pairing
	if via.Chat == nil {
		slog.Info("paired", "pairing", c.Pairing, "kind", r.Kind, "url", r.URL)
		return &r, nil
	}
	p, err := s.deps.Store.Pairing(ctx, c.Pairing)
	if err != nil {
		return nil, err
	}
	r.Principal, r.Role = auth.PairingPrincipal(c.Pairing), auth.PairingRole(p.Role)
	return &r, nil
}

// grant decides what the pending code c is exchanged for, filling r.
// Codes it refuses stay pending.
func (s *Service) grant(ctx context.Context, c store.Code, via Via, kind string, r *Redemption) (store.Grant, error) {
	if s.isRemoving(c.Pairing) {
		return store.Grant{}, store.ErrNotFound
	}
	if via.Chat != nil {
		if c.Kind == pairapi.PairingWebhook {
			return store.Grant{}, store.ErrNotFound
		}
		l := store.Link{Channel: via.Chat.Channel, Identity: via.Chat.Identity}
		if via.Chat.Direct {
			l.Conv = string(via.Chat.Conv)
		}
		return store.Grant{Link: &l, Kind: pairapi.PairingPerson}, nil
	}
	if c.Pairing == LocalOwner {
		return store.Grant{}, ErrLocalCode
	}
	if c.Kind != pairapi.PairingWebhook {
		r.Kind, r.Token, r.Principal, r.URL = pairapi.KindCaller, auth.NewToken(), auth.PairingPrincipal(c.Pairing), via.URL
		return store.Grant{Token: r.Token, TokenURL: via.URL, Kind: kind}, nil
	}
	node, err := s.deps.Store.Node(ctx, c.Pairing)
	if err != nil {
		return store.Grant{}, err
	}
	token := mailboxapi.NewNodeToken(mailboxapi.APIBase(via.URL), node.ID, cryptoutil.RandomString(40, cryptoutil.Alphanumeric))
	r.Kind, r.Token, r.Principal, r.URL, r.Node = pairapi.KindNode, token, auth.NodePrincipal(node.ID), mailboxapi.NodeURL(via.URL, node.ID), node.Name
	return store.Grant{NodeToken: token, NodeSecret: mailboxapi.WakeKey(token)}, nil
}

var linkCommand = regexp.MustCompile(`(?i)^\s*/?(?:join|link|start)(?:@\S+)?\s+([A-Za-z0-9-]{8,9})\s*$`)

// LinkCode returns the code of a "join <code>", "link <code>" or "/start
// <code>" message.
func LinkCode(text string) (string, bool) {
	m := linkCommand.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return m[1], true
}
