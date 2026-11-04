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

// Package auth turns requests to an agent server into principals: it
// identifies the caller from channel links, network identity and tokens,
// and gives the identity one role with the policy.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/channel"
	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// OwnerID is the principal of the user running the agent, whichever way
// they arrive.
const OwnerID = "user:owner"

// LocalPairing names the owner token the server saves for the user running
// it. It works only on the local listener.
const LocalPairing = "local"

// HeaderHops is set by agent servers calling each other to the number of
// agent servers a request passed through.
const HeaderHops = "X-A2A-Serve-Hops"

// MaxHops limits chains of agents calling agents.
const MaxHops = 3

// Principal is a caller with its role.
type Principal struct {
	// ID is what the caller acts as, from [Identity.PrincipalID], or
	// agent:<name> for this agent's calls to its webhook nodes.
	ID   string `json:"id"`
	Role string `json:"role,omitempty"`
	// OnBehalfOf names the user this server acts for in its own calls to
	// its webhook nodes, which nodes see. It never decides access.
	OnBehalfOf string `json:"onBehalfOf,omitempty"`
	Identity
}

// IsOwner reports whether the principal has the owner role.
func (p Principal) IsOwner() bool { return p.Role == RoleOwner }

// Owner is the principal of the user running the agent.
func Owner() Principal {
	return Principal{ID: OwnerID, Role: RoleOwner, Identity: Identity{Self: true}}
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal carried by ctx.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Request is what an authenticator sees of a request.
type Request struct {
	// Listener is one of the network.Listener* kinds.
	Listener   string
	RemoteAddr string
	Header     http.Header
	// Channel is set for channel messages.
	Channel *channel.Inbound
}

// ErrNoCredential is returned for a caller who proved nothing about
// themselves, such as a request without a token on the public URL.
var ErrNoCredential = errors.New("this agent needs a credential")

// RefusedError is returned for a known caller the policy refuses.
type RefusedError struct {
	Identity Identity
}

func (e *RefusedError) Error() string {
	return "this agent doesn't accept " + e.Identity.Describe()
}

// Chain turns requests into identities, from channel links, the private
// network's identity and tokens, and gives each one role with the policy.
type Chain struct {
	Store *store.Store
	// Net identifies callers on the private listener. Optional.
	Net network.Provider
	// Policy returns the policy in force.
	Policy func() Policy
}

// GooglePrefix starts the names of the tokens of people who signed in with
// Google: google:<email>.
const GooglePrefix = "google:"

// Authenticate returns the principal of r: [ErrNoCredential] when r proves
// nothing, or a [RefusedError] when the policy refuses who it proves.
func (a *Chain) Authenticate(ctx context.Context, r Request) (Principal, error) {
	var (
		id  Identity
		err error
	)
	if r.Channel != nil {
		id, err = a.channelIdentity(ctx, r.Channel)
	} else {
		id, err = a.requestIdentity(ctx, r)
	}
	if err != nil {
		return Principal{}, err
	}
	return a.Resolve(id)
}

// Resolve gives an identity its role with the policy in force.
func (a *Chain) Resolve(id Identity) (Principal, error) {
	role := a.Policy().Resolve(id)
	if role == "" && id.identified() {
		return Principal{}, &RefusedError{Identity: id}
	}
	// Until anonymous callers get an ID of their own, a caller who proved
	// nothing can't call, whatever the policy says.
	pid := id.PrincipalID()
	if role == "" || pid == "" {
		return Principal{}, ErrNoCredential
	}
	return Principal{ID: pid, Role: role, Identity: id}, nil
}

// Refresh gives a principal stored to act later, such as the user a
// schedule runs as, its role now: the pairing's current role, and the
// policy in force. A zero identity is rebuilt from the ID.
func (a *Chain) Refresh(ctx context.Context, id string, identity Identity) (Principal, error) {
	if identity == (Identity{}) {
		identity = IdentityOf(id)
	}
	if identity.Pairing != "" {
		identity.PairingRole = ""
		u, err := a.Store.User(ctx, PairPrincipal(identity.Pairing))
		switch {
		case errors.Is(err, store.ErrNotFound):
			identity.Pairing = ""
		case err != nil:
			return Principal{}, err
		default:
			identity.PairingRole = pairingRole(u.Role)
		}
	}
	p, err := a.Resolve(identity)
	if err != nil {
		return Principal{}, err
	}
	p.ID = id
	return p, nil
}

func (a *Chain) requestIdentity(ctx context.Context, r Request) (Identity, error) {
	id := Identity{Network: r.Listener, Hops: hops(r.Header)}
	if r.Listener == network.ListenerPrivate && a.Net != nil {
		id.Provider = a.Net.Name()
		peer, err := a.Net.WhoIs(ctx, r.RemoteAddr)
		switch {
		case err == nil:
			id.Email, id.Node, id.Self = peer.Email, peer.Node, peer.Self
			if id.Email != "" {
				id.EmailSource = EmailFromNetwork
			}
		case !errors.Is(err, network.ErrNoIdentity):
			return Identity{}, err
		}
	}
	tok, ok := BearerToken(r.Header.Get("Authorization"))
	if !ok {
		return id, nil
	}
	if !strings.HasPrefix(tok, TokenPrefix) {
		node, ok, err := NodeOfToken(ctx, a.Store, tok)
		if ok {
			id.WebhookNode = node.ID
		}
		return id, err
	}
	t, err := a.Store.LookupToken(ctx, tok)
	if errors.Is(err, store.ErrNotFound) {
		return id, nil
	}
	if err != nil {
		return Identity{}, err
	}
	switch email, google := strings.CutPrefix(t.Name, GooglePrefix); {
	case t.Name == LocalPairing:
		id.Self = id.Self || r.Listener == network.ListenerLocal
	case google && id.Email == "":
		id.Email, id.EmailSource, id.Domain = email, EmailFromGoogle, t.Domain
	case !google:
		id.Pairing, id.PairingRole = t.Name, pairingRole(t.Role)
	}
	return id, nil
}

// pairingRole is the role a pairing stored with role has.
func pairingRole(role string) string {
	if role == RoleOwner {
		return RoleOwner
	}
	return RoleAuthenticated
}

// NodePrincipal is the principal of the webhook node id.
func NodePrincipal(id string) string { return "node:" + id }

// NodeOfToken returns the webhook node which token belongs to, or false if
// it isn't the current token of a node.
func NodeOfToken(ctx context.Context, s *store.Store, token string) (store.Node, bool, error) {
	_, id, ok := mailboxapi.ParseNodeToken(token)
	if !ok {
		return store.Node{}, false, nil
	}
	node, err := s.Node(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Node{}, false, nil
	}
	if err != nil {
		return store.Node{}, false, err
	}
	if node.TokenHash == "" || subtle.ConstantTimeCompare([]byte(node.TokenHash), []byte(cryptoutil.Hash(token))) != 1 {
		return store.Node{}, false, nil
	}
	return node, true, nil
}

// channelIdentity identifies the sender of a channel message: the pairing
// or the user their account is linked to, and the email of mail which
// passed DMARC.
func (a *Chain) channelIdentity(ctx context.Context, in *channel.Inbound) (Identity, error) {
	if !in.Authentic {
		return Identity{}, ErrNoCredential
	}
	id := Identity{Network: NetworkChat, Provider: in.Channel, ChatAccount: in.Identity, Direct: in.Direct}
	if addr, ok := strings.CutPrefix(in.Identity, channel.MailPrefix); ok && network.IsEmail(addr) {
		id.Email, id.EmailSource, id.Domain = addr, EmailFromMail, DomainOf(addr)
	}
	l, err := a.Store.LinkOf(ctx, in.Channel, in.Identity)
	if errors.Is(err, store.ErrNotFound) {
		return id, nil
	}
	if err != nil {
		return Identity{}, err
	}
	if l.UserID == OwnerID {
		id.Self = true
		return id, nil
	}
	name, ok := strings.CutPrefix(l.UserID, PairPrefix)
	if !ok {
		return id, nil
	}
	u, err := a.Store.User(ctx, l.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return id, nil
	}
	if err != nil {
		return Identity{}, err
	}
	id.Pairing, id.PairingRole = name, pairingRole(u.Role)
	return id, nil
}

// hops returns the hop count the caller sent. A caller can leave it out,
// so it only lowers a role, and guards against loops of agents calling
// each other.
func hops(h http.Header) int {
	n, err := strconv.Atoi(h.Get(HeaderHops))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// BearerToken extracts the token of an Authorization header.
func BearerToken(h string) (string, bool) {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

// Pairing.

// TokenPrefix marks pairing tokens.
const TokenPrefix = "a2p_"

// CodeTTL is how long pairing codes are valid.
const CodeTTL = 15 * time.Minute

// Code purposes.
const (
	PurposePair = "pair"
	PurposeNode = "node"
)

// codeAlphabet avoids characters that are easy to confuse when read out.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewCode returns an 8-character code like K7Q2-9XF4.
func NewCode() string {
	s := cryptoutil.RandomString(8, codeAlphabet)
	return s[:4] + "-" + s[4:]
}

// NormalizeCode makes codes case- and dash-insensitive.
func NormalizeCode(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
}

// NewToken returns a new pairing token.
func NewToken() string { return TokenPrefix + cryptoutil.RandomString(40, cryptoutil.Alphanumeric) }

// PrincipalForCode is the principal a pairing code creates: the user
// running the agent for the local token and the owner chat link, and the
// pairing otherwise.
func PrincipalForCode(name string) string {
	if name == OwnerLink || name == LocalPairing {
		return OwnerID
	}
	return PairPrincipal(name)
}

// OwnerLink names the code which links the chat accounts of the user
// running the agent.
const OwnerLink = "owner"

// MintCode stores a new single-use code and returns it.
func MintCode(ctx context.Context, s *store.Store, purpose, name, role string) (string, time.Time, error) {
	code := NewCode()
	expires := time.Now().Add(CodeTTL)
	if err := s.PutCode(ctx, NormalizeCode(code), store.Code{Purpose: purpose, Name: name, Role: role, Expires: expires}); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

// MintSoleCode is MintCode which also revokes the other pending codes of
// purpose and name.
func MintSoleCode(ctx context.Context, s *store.Store, purpose, name, role string) (string, time.Time, error) {
	code := NewCode()
	expires := time.Now().Add(CodeTTL)
	if err := s.ReplaceCode(ctx, NormalizeCode(code), store.Code{Purpose: purpose, Name: name, Role: role, Expires: expires}); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

// ErrInvalidCode is returned for unknown, used or expired codes.
var ErrInvalidCode = errors.New("invalid or expired code")

// IssueToken issues a bearer token at agentURL for a taken pair code.
func IssueToken(ctx context.Context, s *store.Store, c store.Code, agentURL string) (string, Principal, error) {
	p := Principal{ID: PrincipalForCode(c.Name), Role: pairingRole(c.Role)}
	if _, err := s.EnsureUser(ctx, p.ID, p.Role); err != nil {
		return "", Principal{}, err
	}
	tok := NewToken()
	if err := s.PutToken(ctx, tok, store.Token{Name: c.Name, Principal: p.ID, Role: p.Role, URL: agentURL}); err != nil {
		return "", Principal{}, err
	}
	return tok, p, nil
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

// Link consumes a pairing code sent from a channel and links the sender's
// identity to the code's principal.
func Link(ctx context.Context, s *store.Store, in *channel.Inbound, code string) (Principal, error) {
	if !in.Authentic {
		return Principal{}, fmt.Errorf("the sender of this message could not be verified")
	}
	c, err := s.TakeCode(ctx, NormalizeCode(code), PurposePair)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrCodeExpired) {
		return Principal{}, ErrInvalidCode
	}
	if err != nil {
		return Principal{}, err
	}
	p := Principal{ID: PrincipalForCode(c.Name), Role: pairingRole(c.Role)}
	if _, err := s.EnsureUser(ctx, p.ID, p.Role); err != nil {
		return Principal{}, err
	}
	l := store.Link{Channel: in.Channel, Identity: in.Identity, UserID: p.ID, Name: c.Name}
	if in.Direct {
		l.Conv = string(in.Conv)
	}
	return p, s.PutLink(ctx, l)
}
