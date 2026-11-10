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
// identifies the caller from messenger links, network identity and tokens,
// and gives the identity one role with the policy.
package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-serve/internal/cryptoutil"
	"github.com/a2aproject/a2a-serve/internal/mailboxapi"
	"github.com/a2aproject/a2a-serve/internal/messenger"
	"github.com/a2aproject/a2a-serve/internal/network"
	"github.com/a2aproject/a2a-serve/internal/store"
)

// OwnerID is the principal of the user running the agent, whichever way
// they arrive.
const OwnerID = "user:owner"

// GuestID is the principal of guests who proved nothing about themselves,
// such as callers on the public URL without a token. They share it, so the
// IDs of their tasks and conversations, which the server generates, prove
// what is theirs. Tasks and conversations store it, so it keeps the name
// of the role before guests were called guests.
const GuestID = store.GuestID

// OwnerName names the user running the agent where a pairing name is
// expected, such as in the owner chat setting and schedules. No pairing can
// take it.
const OwnerName = "owner"

// LocalPairing is the built-in pairing of the user running the agent. Its
// tokens work only on the local listener; its chat links are that user.
const LocalPairing = store.LocalPairing

// HeaderHops is set by agent servers calling each other to the number of
// agent servers a request passed through.
const HeaderHops = "X-A2A-Serve-Hops"

// HeaderCaller is set by agent servers calling each other to an opaque,
// stable ID of the person a call is for. It keeps the agent processes of
// the callers of one agent apart, and never decides access.
const HeaderCaller = "X-A2A-Serve-Caller"

// maxCallerLen is the length of the longest caller ID taken.
const maxCallerLen = 64

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

// MayCallAgents reports whether the conversations of the principal may
// call other agents. Guests can't use the agent's contacts and nodes.
func (p Principal) MayCallAgents() bool { return p.Role == RoleOwner || p.Role == RoleMember }

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
	// Messenger is set for messenger messages.
	Messenger *messenger.Inbound
}

// ErrNoCredential is returned for a caller who proved nothing about
// themselves, such as a request without a token on the public URL.
var ErrNoCredential = errors.New("this agent needs a credential")

// ErrUnknownToken is returned for a caller whose token the agent doesn't
// know, such as one that is mistyped, revoked or replaced. It matches
// [ErrNoCredential].
var ErrUnknownToken error = unknownTokenError{}

type unknownTokenError struct{}

func (unknownTokenError) Error() string { return "this agent doesn't recognize the token" }

func (unknownTokenError) Is(target error) bool { return target == ErrNoCredential }

// RefusedError is returned for a known caller the policy refuses.
type RefusedError struct {
	Identity Identity
}

func (e *RefusedError) Error() string {
	return "this agent doesn't accept " + e.Identity.Describe()
}

// Refused reports whether err refuses a caller: a [RefusedError] or
// [ErrNoCredential].
func Refused(err error) bool {
	var re *RefusedError
	return errors.As(err, &re) || errors.Is(err, ErrNoCredential)
}

// Chain turns requests into identities, from messenger links, the private
// network's identity and tokens, and gives each one role with the policy.
type Chain struct {
	Store *store.Store
	// Net identifies callers on the private listener. Optional.
	Net network.Provider
	// Policy returns the policy in force.
	Policy func() Policy
}

// Authenticate returns the principal of r: [ErrNoCredential] when r proves
// nothing, [ErrUnknownToken] when it carries a token that proves nothing,
// or a [RefusedError] when the policy refuses who it proves.
func (a *Chain) Authenticate(ctx context.Context, r Request) (Principal, error) {
	var (
		id  Identity
		err error
	)
	if r.Messenger != nil {
		id, err = a.messengerIdentity(ctx, r.Messenger)
	} else {
		id, err = a.requestIdentity(ctx, r)
	}
	if err != nil {
		return Principal{}, err
	}
	p, err := a.Resolve(id)
	if _, hasToken := BearerToken(r.Header.Get("Authorization")); hasToken && r.Messenger == nil && errors.Is(err, ErrNoCredential) {
		return Principal{}, ErrUnknownToken
	}
	return p, err
}

// Resolve gives an identity its role with the policy in force.
func (a *Chain) Resolve(id Identity) (Principal, error) {
	role := a.Policy().Resolve(id)
	if role == "" && id.identified() {
		return Principal{}, &RefusedError{Identity: id}
	}
	pid := id.PrincipalID()
	if pid == "" && role == RoleGuest {
		pid = GuestID
	}
	if role == "" || pid == "" {
		return Principal{}, ErrNoCredential
	}
	return Principal{ID: pid, Role: role, Identity: id}, nil
}

// Stored is [Chain.Refresh] of a principal stored with its identity as
// JSON, or empty when the ID says it all. A principal the policy refuses
// now has no role.
func (a *Chain) Stored(ctx context.Context, id, identity string) (Principal, error) {
	var ident Identity
	if identity != "" {
		if err := json.Unmarshal([]byte(identity), &ident); err != nil {
			return Principal{}, fmt.Errorf("reading the identity of %s: %w", id, err)
		}
	}
	p, err := a.Refresh(ctx, id, ident)
	if Refused(err) {
		return Principal{ID: id, Identity: ident}, nil
	}
	return p, err
}

// Refresh gives a principal stored to act later, such as the user a
// schedule runs as, its role now: the pairing's current role, whether its
// webhook node still exists, what the private network reports of its
// machine, and the policy in force. A zero identity is rebuilt from the ID.
func (a *Chain) Refresh(ctx context.Context, id string, identity Identity) (Principal, error) {
	if identity == (Identity{}) {
		identity = IdentityOf(id)
	}
	if identity.Network == network.ListenerPrivate {
		var err error
		if identity, err = a.refreshNetwork(ctx, identity); err != nil {
			return Principal{}, err
		}
	}
	if id == OwnerID {
		identity.Self = true
	}
	if identity.Pairing != "" {
		identity.PairingRole = ""
		p, err := a.Store.Pairing(ctx, identity.Pairing)
		switch {
		case errors.Is(err, store.ErrNotFound):
			identity.Pairing = ""
		case err != nil:
			return Principal{}, err
		default:
			identity.PairingRole = PairingRole(p.Role)
		}
	}
	if identity.WebhookNode != "" {
		_, err := a.Store.Node(ctx, identity.WebhookNode)
		switch {
		case errors.Is(err, store.ErrNotFound):
			identity.WebhookNode = ""
		case err != nil:
			return Principal{}, err
		}
	}
	p, err := a.Resolve(identity)
	if err != nil {
		return Principal{}, err
	}
	p.ID = id
	return p, nil
}

// refreshNetwork replaces the claims the private network made about the
// caller's machine with what it reports now, and drops them once it no
// longer has the machine. A network that can't look machines up, such as
// a relay, keeps them.
func (a *Chain) refreshNetwork(ctx context.Context, id Identity) (Identity, error) {
	var peer *network.Peer
	if a.Net != nil && a.Net.Name() == id.Provider && id.Node != "" {
		p, err := a.Net.Member(ctx, id.Node)
		switch {
		case errors.Is(err, network.ErrUnsupported):
			return id, nil
		case err == nil:
			peer = p
		case !errors.Is(err, network.ErrNoIdentity):
			return Identity{}, err
		}
	}
	id.Node, id.Self = "", false
	if id.EmailSource == EmailFromNetwork {
		id.Email, id.EmailSource = "", ""
	}
	if peer == nil {
		return id, nil
	}
	id.Node, id.Self = peer.Node, peer.Self
	if peer.Email != "" {
		id.Email, id.EmailSource, id.Domain = peer.Email, EmailFromNetwork, ""
	}
	return id, nil
}

func (a *Chain) requestIdentity(ctx context.Context, r Request) (Identity, error) {
	id := Identity{Network: r.Listener, Hops: Hops(r.Header)}
	if id.Hops > 0 {
		id.Caller = caller(r.Header)
	}
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
	switch {
	case t.Pairing == LocalPairing:
		id.Self = id.Self || r.Listener == network.ListenerLocal
	case t.Pairing == "" && id.Email == "":
		id.Email, id.EmailSource, id.Domain = t.Email, EmailFromGoogle, t.Domain
	case t.Pairing != "":
		return a.withPairing(ctx, id, t.Pairing)
	}
	return id, nil
}

// withPairing adds the pairing name with its current role to id.
func (a *Chain) withPairing(ctx context.Context, id Identity, name string) (Identity, error) {
	p, err := a.Store.Pairing(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return id, nil
	}
	if err != nil {
		return Identity{}, err
	}
	id.Pairing, id.PairingRole = p.Name, PairingRole(p.Role)
	return id, nil
}

// PairingRole is the role a pairing stored with role has.
func PairingRole(role string) string {
	if role == RoleOwner {
		return RoleOwner
	}
	return RoleMember
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

// messengerIdentity identifies the sender of a messenger message: the pairing
// their account is linked to, which is the user running the agent for the
// local pairing, and the email of mail which passed DMARC.
func (a *Chain) messengerIdentity(ctx context.Context, in *messenger.Inbound) (Identity, error) {
	if !in.Authentic {
		return Identity{}, ErrNoCredential
	}
	id := Identity{Network: NetworkChat, Provider: in.Messenger, ChatAccount: in.Identity, Direct: in.Direct}
	if network.IsEmail(in.Identity) {
		id.Email, id.EmailSource, id.Domain = in.Identity, EmailFromMail, DomainOf(in.Identity)
	}
	l, err := a.Store.LinkOf(ctx, in.Messenger, in.Identity)
	if errors.Is(err, store.ErrNotFound) {
		return id, nil
	}
	if err != nil {
		return Identity{}, err
	}
	if l.Pairing == LocalPairing {
		id.Self = true
		return id, nil
	}
	return a.withPairing(ctx, id, l.Pairing)
}

// Hops returns the hop count the caller sent. A caller can leave it out,
// so it only lowers a role, and guards against loops of agents calling
// each other.
func Hops(h http.Header) int {
	n, err := strconv.Atoi(h.Get(HeaderHops))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// caller returns the caller ID another agent sent, or "" for a missing
// or malformed one: up to maxCallerLen letters, digits, '-' and '_'.
func caller(h http.Header) string {
	c := h.Get(HeaderCaller)
	if len(c) > maxCallerLen {
		return ""
	}
	for _, r := range c {
		if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '-' || r == '_') {
			return ""
		}
	}
	return c
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

// PairingPrincipal is the principal of the pairing name: the user running
// the agent for the local pairing.
func PairingPrincipal(name string) string {
	if name == LocalPairing {
		return OwnerID
	}
	return PairPrincipal(name)
}

// PrincipalPairing is the pairing of a principal, which is the local
// pairing for the user running the agent, or false if it has none.
func PrincipalPairing(id string) (string, bool) {
	if id == OwnerID {
		return LocalPairing, true
	}
	name, ok := strings.CutPrefix(id, PairPrefix)
	return name, ok && name != ""
}

// MintCode stores a new single-use code of a pairing and returns it.
func MintCode(ctx context.Context, s *store.Store, pairing string) (string, time.Time, error) {
	code := NewCode()
	expires := time.Now().Add(CodeTTL)
	if err := s.PutCode(ctx, NormalizeCode(code), store.Code{Pairing: pairing, Expires: expires}); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

// MintInviteCode is MintCode which first creates the pairing p if it
// doesn't exist.
func MintInviteCode(ctx context.Context, s *store.Store, p store.Pairing) (string, time.Time, error) {
	code := NewCode()
	expires := time.Now().Add(CodeTTL)
	if err := s.InviteCode(ctx, p, NormalizeCode(code), expires); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

// MintSoleCode is MintCode which also revokes the other pending codes of
// the pairing.
func MintSoleCode(ctx context.Context, s *store.Store, pairing string) (string, time.Time, error) {
	code := NewCode()
	expires := time.Now().Add(CodeTTL)
	if err := s.ReplaceCode(ctx, NormalizeCode(code), store.Code{Pairing: pairing, Expires: expires}); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

// IssueToken issues a bearer token of a pairing at agentURL, and gives the
// pairing kind if it has none yet. It returns the token and the principal
// it acts as.
func IssueToken(ctx context.Context, s *store.Store, pairing, agentURL, kind string) (string, string, error) {
	tok := NewToken()
	if err := s.PutToken(ctx, tok, store.Token{Pairing: pairing, URL: agentURL}, kind); err != nil {
		return "", "", err
	}
	return tok, PairingPrincipal(pairing), nil
}
