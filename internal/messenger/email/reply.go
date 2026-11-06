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
	"io"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	// attribution matches the line, or up to three wrapped lines, which
	// introduces the quoted message in common mail clients and languages,
	// such as "On Mon, 1 Jan 2026, Alice <a@b> wrote:" or
	// "Am 01.01.2026 um 10:00 schrieb Alice <a@b>:".
	attribution = regexp.MustCompile(`(?i)^(on|am|le|el|il|op|em|den|w dniu)\s.*\s(wrote|schrieb|a écrit|escribió|ha scritto|schreef|escreveu|skrev|napisał|pisze)(\s.*)?:$`)
	// originalMessage matches the separator Outlook puts above the quoted
	// message, in several languages.
	originalMessage = regexp.MustCompile(`(?i)^-{2,}\s*(original message|ursprüngliche nachricht|message d'origine|mensaje original|messaggio originale|oorspronkelijk bericht|originalmeddelande|mensagem original|wiadomość oryginalna)\s*-{2,}$`)
	underscores     = regexp.MustCompile(`^_{10,}$`)
	// headerFrom and headerSent match the header block of a quoted message
	// in Outlook, in several languages.
	headerFrom = regexp.MustCompile(`(?i)^\*?(from|von|de|da|van|från|od|fra)\s?:`)
	headerSent = regexp.MustCompile(`(?i)^\*?(sent|date|gesendet|datum|envoyé|inviato|enviado|verzonden|skickat|wysłano|sendt|data)\s?:`)
	// trailer matches the lines mobile clients append to a message.
	trailer = regexp.MustCompile(`(?i)^(sent from my |sent from (mail|outlook|yahoo)|sent via |get outlook for |von meinem .* gesendet|envoyé de mon |enviado desde mi |enviado do meu |inviato da |verzonden met |skickat från min |wysłane z )`)
)

// StripQuoted drops the quoted text, the signature and mobile trailers of a
// message. A reply also loses the quoted message which Outlook and Gmail put
// below an attribution or a header block. Other mail keeps such blocks,
// because forwarded mail starts with them.
func StripQuoted(text string, reply bool) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []string
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		if trimmed == "-- " || trimmed == "--" || isAttribution(lines[i:], reply) {
			break
		}
		if reply && (originalMessage.MatchString(trimmed) || underscores.MatchString(trimmed) || isHeaderBlock(lines[i:])) {
			break
		}
		out = append(out, line)
	}
	for len(out) > 0 {
		last := strings.TrimSpace(out[len(out)-1])
		if last != "" && !trailer.MatchString(last) {
			break
		}
		out = out[:len(out)-1]
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// isAttribution reports whether lines start with an attribution. In a
// reply, a line which ends with a colon and is followed only by quoted text
// counts too, for languages the pattern lacks. Inline answers between
// quoted lines keep such a line.
func isAttribution(lines []string, reply bool) bool {
	first := strings.TrimSpace(lines[0])
	if strings.HasPrefix(first, "On ") && strings.HasSuffix(first, "wrote:") {
		return true
	}
	if !reply || first == "" {
		return false
	}
	joined := first
	for i := 1; ; i++ {
		if attribution.MatchString(joined) {
			return true
		}
		if i >= 3 || i >= len(lines) {
			break
		}
		joined += " " + strings.TrimSpace(lines[i])
	}
	if !strings.HasSuffix(first, ":") {
		return false
	}
	quoted := false
	for _, l := range lines[1:] {
		switch l = strings.TrimSpace(l); {
		case strings.HasPrefix(l, ">"):
			quoted = true
		case l != "":
			return false
		}
	}
	return quoted
}

// isHeaderBlock reports whether lines start with a From: line followed by a
// Sent: or Date: line, as Outlook quotes a message.
func isHeaderBlock(lines []string) bool {
	if !headerFrom.MatchString(strings.TrimSpace(lines[0])) {
		return false
	}
	for _, l := range lines[1:min(len(lines), 5)] {
		if headerSent.MatchString(strings.TrimSpace(l)) {
			return true
		}
	}
	return false
}

var space = regexp.MustCompile(`\s+`)

// htmlText returns the text of an HTML mail body. Block elements become
// lines, quoted blocks are marked with "> " and horizontal rules become the
// Outlook separator, so [StripQuoted] can cut the quoted message.
func htmlText(r io.Reader) (string, error) {
	z := html.NewTokenizer(r)
	var (
		lines []string
		line  strings.Builder
		quote int
		skip  int
	)
	flush := func() {
		text := strings.TrimSpace(line.String())
		line.Reset()
		if text == "" {
			if len(lines) > 0 && lines[len(lines)-1] != "" {
				lines = append(lines, "")
			}
			return
		}
		lines = append(lines, strings.Repeat("> ", quote)+text)
	}
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if !errors.Is(z.Err(), io.EOF) {
				return "", z.Err()
			}
			flush()
			return strings.TrimSpace(strings.Join(lines, "\n")), nil
		case html.TextToken:
			if skip > 0 {
				continue
			}
			text := space.ReplaceAllString(string(z.Text()), " ")
			if strings.HasSuffix(line.String(), " ") {
				text = strings.TrimPrefix(text, " ")
			}
			line.WriteString(text)
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			name, _ := z.TagName()
			a := atom.Lookup(name)
			switch a {
			case atom.Script, atom.Style, atom.Head, atom.Title:
				if tt == html.StartTagToken {
					skip++
				} else if tt == html.EndTagToken && skip > 0 {
					skip--
				}
			case atom.Blockquote:
				flush()
				if tt == html.StartTagToken {
					quote++
				} else if tt == html.EndTagToken && quote > 0 {
					quote--
				}
			case atom.Hr:
				flush()
				lines = append(lines, strings.Repeat("> ", quote)+"__________")
			case atom.Br, atom.P, atom.Div, atom.Li, atom.Tr, atom.Table, atom.Ul, atom.Ol, atom.Pre,
				atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
				flush()
			}
		}
	}
}
