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

package agentmesh

import (
	"errors"
	"net"
	"net/url"
)

// ErrCleartextRelayURL is the reason [CheckRelayURL] refuses an http URL
// that isn't on loopback.
var ErrCleartextRelayURL = errors.New("must use https, since tokens travel over it")

// CheckRelayURL checks the URL of a relay, which agents join and send
// their tokens to: a bare origin with https, or with http on loopback. Its
// error is the reason, for the caller to name the URL and the flag.
func CheckRelayURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("want a bare origin such as https://relay.example.com")
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && Loopback(raw):
		return nil
	case u.Scheme == "http":
		return ErrCleartextRelayURL
	}
	return errors.New("want an https URL such as https://relay.example.com")
}

// Loopback reports whether the relay URL raw is on this machine.
func Loopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// InsecureLoopback reports whether the relay URL raw is plain http on this
// machine, the only place a relay is joined without TLS.
func InsecureLoopback(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "http" && Loopback(raw)
}
