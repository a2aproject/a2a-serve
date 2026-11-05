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
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/smtp"
	"time"

	"github.com/emersion/go-sasl"
	"golang.org/x/oauth2"
)

// OAuthConfig signs in to IMAP and SMTP with XOAUTH2, using access tokens
// obtained with a refresh token, as Microsoft 365 and Gmail require.
type OAuthConfig struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
	RefreshToken string
}

const tokenTimeout = 30 * time.Second

// newTokenSource returns access tokens for cfg, refreshing them when they
// expire.
func newTokenSource(cfg OAuthConfig) oauth2.TokenSource {
	conf := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     oauth2.Endpoint{TokenURL: cfg.TokenURL},
	}
	// The token source keeps this context for every refresh.
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: tokenTimeout})
	return conf.TokenSource(ctx, &oauth2.Token{RefreshToken: cfg.RefreshToken})
}

// accessToken returns a valid access token, or ctx's error if it ends
// first.
func (c *Channel) accessToken(ctx context.Context) (string, error) {
	type result struct {
		tok *oauth2.Token
		err error
	}
	done := make(chan result, 1)
	go func() {
		tok, err := c.tokens.Token()
		done <- result{tok, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		if r.err != nil {
			return "", fmt.Errorf("email: refreshing the OAuth access token: %w", r.err)
		}
		return r.tok.AccessToken, nil
	}
}

func xoauth2Response(user, token string) []byte {
	return []byte("user=" + user + "\x01auth=Bearer " + token + "\x01\x01")
}

// xoauth2Client is the SASL XOAUTH2 mechanism for IMAP.
type xoauth2Client struct{ user, token string }

var _ sasl.Client = xoauth2Client{}

func (a xoauth2Client) Start() (string, []byte, error) {
	return "XOAUTH2", xoauth2Response(a.user, a.token), nil
}

// Next answers the error details the server sends after a refused token
// with an empty response, which makes the server end the exchange with
// its error.
func (a xoauth2Client) Next([]byte) ([]byte, error) { return []byte{}, nil }

// xoauth2Auth is the XOAUTH2 mechanism for SMTP.
type xoauth2Auth struct{ user, token string }

var _ smtp.Auth = xoauth2Auth{}

func (a xoauth2Auth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS && !isLoopback(server.Name) {
		return "", nil, errors.New("refusing to send the OAuth token unencrypted")
	}
	return "XOAUTH2", xoauth2Response(a.user, a.token), nil
}

func (a xoauth2Auth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return []byte{}, nil
	}
	return nil, nil
}
