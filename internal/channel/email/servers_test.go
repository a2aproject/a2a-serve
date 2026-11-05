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

package email

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"
)

// imapServer is an in-memory IMAP server which records its traffic.
type imapServer struct {
	addr string
	user *imapmemserver.User

	mu      sync.Mutex
	traffic bytes.Buffer
}

func (s *imapServer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.traffic.Write(p)
}

// sentCount returns how often the server sent text to a client.
func (s *imapServer) sentCount(text string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Count(s.traffic.String(), text)
}

// startIMAP serves the INBOX of user with password, or with the OAuth
// access token if token is set.
func startIMAP(t *testing.T, user, password, token string) *imapServer {
	t.Helper()
	s := &imapServer{user: imapmemserver.NewUser(user, password)}
	if err := s.user.Create("INBOX", nil); err != nil {
		t.Fatalf("Create(INBOX) error = %v", err)
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			sess := imapmemserver.NewUserSession(s.user)
			if token != "" {
				return &oauthSession{UserSession: sess, user: user, token: token}, nil, nil
			}
			return sess, nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}, imap.CapSASLIR: {}},
		InsecureAuth: true,
		DebugWriter:  s,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	s.addr = ln.Addr().String()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Logf("IMAP server: %v", err)
		}
	}()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Logf("closing the IMAP server: %v", err)
		}
	})
	return s
}

func (s *imapServer) deliver(t *testing.T, raw string) {
	t.Helper()
	if _, err := s.user.Append("INBOX", bytes.NewReader([]byte(raw)), &imap.AppendOptions{}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
}

// dial connects without TLS, which the test server allows.
func (s *imapServer) dial(t *testing.T) *imapclient.Client {
	t.Helper()
	client, err := imapclient.DialInsecure(s.addr, nil)
	if err != nil {
		t.Fatalf("imapclient.DialInsecure() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Logf("closing the IMAP client: %v", err)
		}
	})
	return client
}

// oauthSession signs in with XOAUTH2 only.
type oauthSession struct {
	*imapmemserver.UserSession
	user, token string
}

func (s *oauthSession) Login(string, string) error { return imapserver.ErrAuthFailed }

func (s *oauthSession) AuthenticateMechanisms() []string { return []string{"XOAUTH2"} }

func (s *oauthSession) Authenticate(mech string) (sasl.Server, error) {
	if mech != "XOAUTH2" {
		return nil, imapserver.ErrAuthFailed
	}
	return saslFunc(func(resp []byte) ([]byte, bool, error) {
		if string(resp) != string(xoauth2Response(s.user, s.token)) {
			return nil, false, imapserver.ErrAuthFailed
		}
		return nil, true, nil
	}), nil
}

type saslFunc func([]byte) ([]byte, bool, error)

func (f saslFunc) Next(resp []byte) ([]byte, bool, error) { return f(resp) }

// tokenServer is an OAuth token endpoint which grants access for one
// refresh token.
func tokenServer(t *testing.T, refreshToken, accessToken string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != refreshToken {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"access_token": accessToken, "token_type": "Bearer", "expires_in": 3600}); err != nil {
			t.Errorf("encoding the token response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// smtpServer is a plain SMTP server on loopback. It refuses MAIL FROM with
// the next of refusals, then accepts.
type smtpServer struct {
	addr string

	mu       sync.Mutex
	refusals []string
	auths    []string
	msgs     []string
}

func startSMTP(t *testing.T, refusals ...string) *smtpServer {
	t.Helper()
	s := &smtpServer{refusals: refusals}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	s.addr = ln.Addr().String()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(t, conn)
		}
	}()
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Logf("closing the SMTP listener: %v", err)
		}
	})
	return s
}

func (s *smtpServer) serve(t *testing.T, conn net.Conn) {
	defer func() {
		if err := conn.Close(); err != nil {
			t.Logf("closing the SMTP connection: %v", err)
		}
	}()
	r := bufio.NewReader(conn)
	reply := func(line string) bool {
		_, err := fmt.Fprintf(conn, "%s\r\n", line)
		return err == nil
	}
	if !reply("220 localhost ready") {
		return
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			reply("250-localhost")
			reply("250 AUTH PLAIN XOAUTH2")
		case "AUTH":
			mech, ir, _ := strings.Cut(arg, " ")
			payload, err := base64.StdEncoding.DecodeString(ir)
			if err != nil {
				reply("501 bad base64")
				continue
			}
			s.mu.Lock()
			s.auths = append(s.auths, mech+" "+string(payload))
			s.mu.Unlock()
			reply("235 authenticated")
		case "MAIL":
			s.mu.Lock()
			var refusal string
			if len(s.refusals) > 0 {
				refusal, s.refusals = s.refusals[0], s.refusals[1:]
			}
			s.mu.Unlock()
			if refusal != "" {
				reply(refusal)
				continue
			}
			reply("250 ok")
		case "RCPT":
			reply("250 ok")
		case "DATA":
			reply("354 go ahead")
			var msg strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				msg.WriteString(l)
			}
			s.mu.Lock()
			s.msgs = append(s.msgs, msg.String())
			s.mu.Unlock()
			reply("250 queued")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 unknown command")
		}
	}
}

func (s *smtpServer) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

func (s *smtpServer) authentications() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auths...)
}
