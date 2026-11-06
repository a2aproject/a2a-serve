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

//go:build e2e

package e2e

import (
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"
)

// inspector is a browser session of the owner in the web inspector.
type inspector struct {
	t        *testing.T
	client   *http.Client
	overview string
}

// inspect logs in to the inspector with the link of `a2a-serve inspect`.
func (a *agent) inspect() *inspector {
	t := a.u.t
	t.Helper()
	var link struct {
		URL string `json:"url"`
	}
	a.serveJSON(&link, "inspect", "--no-browser")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	in := &inspector{t: t, client: &http.Client{Jar: jar, Timeout: cmdTimeout}}
	resp, body := in.do(http.MethodGet, link.URL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the login link = %s %q, want the overview", resp.Status, body)
	}
	in.overview = resp.Request.URL.String()
	return in
}

func (in *inspector) do(method, u string, form url.Values) (*http.Response, string) {
	in.t.Helper()
	req, err := http.NewRequest(method, u, strings.NewReader(form.Encode()))
	if err != nil {
		in.t.Fatalf("http.NewRequest() error = %v", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := in.client.Do(req)
	if err != nil {
		in.t.Fatalf("%s %s error = %v", method, u, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			in.t.Errorf("closing the response body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		in.t.Fatalf("reading %s %s: %v", method, u, err)
	}
	return resp, string(body)
}

// page returns the overview page.
func (in *inspector) page() string {
	in.t.Helper()
	resp, body := in.do(http.MethodGet, in.overview, nil)
	if resp.StatusCode != http.StatusOK {
		in.t.Fatalf("GET %s = %s %q, want the overview", in.overview, resp.Status, body)
	}
	return body
}

// approval is a pending approval form of the overview.
type approval struct {
	// Contact is the agent a relayed approval comes from.
	Contact string
	Text    string
	Action  string
	CSRF    string
	// Question is the ID of the task's question that the form answers.
	Question string
	Options  []string
}

var (
	approvalRE = regexp.MustCompile(`(?s)<div class="approval">\s*<div class="muted">(?:from ([^,<]+), )?.*?</div>\s*<pre>(.*?)</pre>\s*<form method="post" action="([^"]+)"><input type="hidden" name="csrf" value="([^"]+)">(.*?)</form>`)
	optionRE   = regexp.MustCompile(`<button name="option" value="([^"]+)">`)
	questionRE = regexp.MustCompile(`<input type="hidden" name="question" value="([^"]+)">`)
)

// approvals returns the pending approvals of the overview.
func (in *inspector) approvals() []approval {
	in.t.Helper()
	var out []approval
	for _, m := range approvalRE.FindAllStringSubmatch(in.page(), -1) {
		a := approval{Contact: html.UnescapeString(m[1]), Text: html.UnescapeString(m[2]), Action: html.UnescapeString(m[3]), CSRF: m[4]}
		if q := questionRE.FindStringSubmatch(m[5]); q != nil {
			a.Question = html.UnescapeString(q[1])
		}
		for _, o := range optionRE.FindAllStringSubmatch(m[5], -1) {
			a.Options = append(a.Options, html.UnescapeString(o[1]))
		}
		out = append(out, a)
	}
	return out
}

// waitApproval waits until the overview shows an approval of contact, empty
// for this agent's own, whose text has want.
func (in *inspector) waitApproval(contact, want string) approval {
	in.t.Helper()
	var found approval
	eventually(in.t, "waiting for an approval in the inspector", func() (bool, string) {
		all := in.approvals()
		for _, a := range all {
			if a.Contact == contact && strings.Contains(a.Text, want) {
				found = a
				return true, ""
			}
		}
		return false, "approvals " + strings.Join(texts(all), "; ")
	})
	return found
}

func texts(all []approval) []string {
	var out []string
	for _, a := range all {
		out = append(out, a.Text)
	}
	return out
}

// answer posts option to the approval's form with csrf as its token, and
// returns the status of the response.
func (in *inspector) answer(a approval, option, csrf string) int {
	in.t.Helper()
	target, err := url.Parse(in.overview)
	if err != nil {
		in.t.Fatalf("url.Parse(%q) error = %v", in.overview, err)
	}
	target.Path = a.Action
	form := url.Values{"csrf": {csrf}, "option": {option}}
	if a.Question != "" {
		form.Set("question", a.Question)
	}
	resp, _ := in.do(http.MethodPost, target.String(), form)
	return resp.StatusCode
}

func TestInspectorAnswersApproval(t *testing.T) {
	t.Parallel()
	owner := newUser(t)
	coder := owner.startAgent("coder")
	task := owner.mustSend(coder.url, "ask")
	in := coder.inspect()

	if page := in.page(); !strings.Contains(page, "<h1>coder") || !strings.Contains(page, "<h2>Conversations</h2>") {
		t.Fatalf("inspector overview = %q, want the agent's overview", page)
	}
	a := in.waitApproval("", "git push")
	if diff := cmp.Diff([]string{"once", "always", "reject"}, a.Options); diff != "" {
		t.Fatalf("inspector approval options wrong result (-want +got) diff = %s", diff)
	}
	if got := in.answer(a, "once", "wrong"); got != http.StatusForbidden {
		t.Fatalf("POST %s with a wrong CSRF token = %d, want %d", a.Action, got, http.StatusForbidden)
	}
	if got := in.answer(a, "once", a.CSRF); got != http.StatusOK {
		t.Fatalf("POST %s = %d, want %d after the redirect to the overview", a.Action, got, http.StatusOK)
	}

	eventually(t, "waiting for the answered task", func() (bool, string) {
		got := resultOf(owner.mustGetTask(coder.url, string(task.ID)))
		return got == taskResult{State: a2a.TaskStateCompleted, Text: "Pushed (allow)."}, got.Text
	})
	if all := in.approvals(); len(all) != 0 {
		t.Fatalf("inspector approvals after the answer = %v, want none", texts(all))
	}
}
