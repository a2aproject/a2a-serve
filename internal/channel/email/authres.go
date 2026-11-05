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
	"errors"
	"fmt"
	"strings"
)

// authResult is one resinfo of an Authentication-Results header (RFC 8601),
// such as dmarc=pass header.from=alice.org.
type authResult struct {
	method string
	result string
	props  map[string]string
}

type tokenKind int

const (
	tokenValue tokenKind = iota
	tokenSemicolon
	tokenEquals
)

type token struct {
	kind  tokenKind
	value string
}

// tokenize splits an Authentication-Results header into values, ";" and
// "=", dropping comments. Quoted strings and comments may contain ";" and
// "=" copied from the message, such as a quoted envelope sender, so they
// must not separate results.
func tokenize(s string) ([]token, error) {
	var toks []token
	for i := 0; i < len(s); {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '(':
			end, err := skipComment(s, i)
			if err != nil {
				return nil, err
			}
			i = end
		case ';':
			toks = append(toks, token{kind: tokenSemicolon})
			i++
		case '=':
			toks = append(toks, token{kind: tokenEquals})
			i++
		default:
			v, end, err := readValue(s, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tokenValue, value: v})
			i = end
		}
	}
	return toks, nil
}

// skipComment returns the index after the comment starting at s[i].
func skipComment(s string, i int) (int, error) {
	depth := 0
	for ; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i + 1, nil
			}
		}
	}
	return 0, errors.New("unterminated comment")
}

// readValue reads a value starting at s[i] up to whitespace, a special or a
// comment. Quoted parts, as in a quoted local part, are unquoted in place.
func readValue(s string, i int) (string, int, error) {
	var b strings.Builder
	for i < len(s) {
		switch c := s[i]; c {
		case ' ', '\t', '\r', '\n', '(', ';', '=':
			return b.String(), i, nil
		case '"':
			for i++; ; i++ {
				if i >= len(s) {
					return "", 0, errors.New("unterminated quoted string")
				}
				if s[i] == '\\' && i+1 < len(s) {
					i++
				} else if s[i] == '"' {
					i++
					break
				}
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), i, nil
}

// parseAuthResults parses an Authentication-Results header into its
// authserv-id and results.
func parseAuthResults(header string) (string, []authResult, error) {
	toks, err := tokenize(header)
	if err != nil {
		return "", nil, err
	}
	if len(toks) == 0 || toks[0].kind != tokenValue {
		return "", nil, errors.New("missing authserv-id")
	}
	id := toks[0].value
	rest := toks[1:]
	if len(rest) > 0 && rest[0].kind == tokenValue {
		if rest[0].value != "1" {
			return "", nil, fmt.Errorf("unsupported version %q", rest[0].value)
		}
		rest = rest[1:]
	}
	var results []authResult
	for len(rest) > 0 {
		if rest[0].kind != tokenSemicolon {
			return "", nil, errors.New("missing ; before a result")
		}
		rest = rest[1:]
		end := 0
		for end < len(rest) && rest[end].kind != tokenSemicolon {
			end++
		}
		pairs, err := parsePairs(rest[:end])
		if err != nil {
			return "", nil, err
		}
		rest = rest[end:]
		if len(pairs) == 0 {
			continue
		}
		method, _, _ := strings.Cut(pairs[0][0], "/")
		r := authResult{method: method, result: strings.ToLower(pairs[0][1]), props: map[string]string{}}
		for _, p := range pairs[1:] {
			r.props[p[0]] = p[1]
		}
		results = append(results, r)
	}
	return id, results, nil
}

// parsePairs parses key=value pairs, lowercasing the keys. A lone "none"
// means there are no results.
func parsePairs(toks []token) ([][2]string, error) {
	if len(toks) == 1 && toks[0].kind == tokenValue && strings.EqualFold(toks[0].value, "none") {
		return nil, nil
	}
	var pairs [][2]string
	for len(toks) > 0 {
		if len(toks) < 3 || toks[0].kind != tokenValue || toks[1].kind != tokenEquals || toks[2].kind != tokenValue {
			return nil, errors.New("malformed result")
		}
		pairs = append(pairs, [2]string{strings.ToLower(toks[0].value), toks[2].value})
		toks = toks[3:]
	}
	return pairs, nil
}

// Authenticate checks the topmost Authentication-Results header added by
// the trusted server, because headers further down may have been forged by
// the sender. It accepts dmarc=pass for the sender's domain. Without a DMARC
// verdict, or with dmarc=none, it accepts a DKIM signature of exactly that
// domain. The error tells why the mail is rejected.
func Authenticate(results []string, trusted, fromDomain string) error {
	for _, r := range results {
		id, _, ok := strings.Cut(r, ";")
		if !ok || !strings.EqualFold(strings.Fields(id + " x")[0], trusted) {
			continue
		}
		_, parsed, err := parseAuthResults(r)
		if err != nil {
			return fmt.Errorf("malformed Authentication-Results of %s: %w", trusted, err)
		}
		return verdict(parsed, fromDomain)
	}
	return fmt.Errorf("no Authentication-Results of %s", trusted)
}

func verdict(results []authResult, fromDomain string) error {
	dmarcPass := false
	for _, r := range results {
		if r.method != "dmarc" {
			continue
		}
		switch r.result {
		case "none":
		case "pass":
			if from := r.props["header.from"]; !strings.EqualFold(from, fromDomain) {
				return fmt.Errorf("dmarc=pass for %q, want %s", from, fromDomain)
			}
			dmarcPass = true
		default:
			return fmt.Errorf("dmarc=%s", r.result)
		}
	}
	if dmarcPass {
		return nil
	}
	headerI := false
	for _, r := range results {
		if r.method != "dkim" || r.result != "pass" {
			continue
		}
		if strings.EqualFold(r.props["header.d"], fromDomain) {
			return nil
		}
		headerI = headerI || r.props["header.d"] == "" && r.props["header.i"] != ""
	}
	if headerI {
		return fmt.Errorf("no dmarc=pass, and dkim=pass names only header.i, want header.d=%s", fromDomain)
	}
	return fmt.Errorf("no dmarc=pass and no dkim=pass with header.d=%s", fromDomain)
}
