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

package agentsrv

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/a2aproject/a2a-serve/internal/agentdir"
	"github.com/a2aproject/a2a-serve/internal/auth"
	"github.com/a2aproject/a2a-serve/internal/controlapi"
)

// inspectorClient is a browser of the inspector of ts.
type inspectorClient struct {
	t    *testing.T
	ts   *testServer
	http *http.Client
	// site is the Sec-Fetch-Site header a browser sends, if set.
	site string
}

func newInspectorClient(t *testing.T, ts *testServer) *inspectorClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &inspectorClient{t: t, ts: ts, http: &http.Client{Jar: jar}}
}

func (c *inspectorClient) do(req *http.Request) (int, string) {
	c.t.Helper()
	if c.site != "" {
		req.Header.Set("Sec-Fetch-Site", c.site)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s error = %v", req.Method, req.URL, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		c.t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func (c *inspectorClient) get(path string) (int, string) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.ts.localURL()+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.do(req)
}

func (c *inspectorClient) post(path string, form url.Values) (int, string) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.ts.localURL()+path, strings.NewReader(form.Encode()))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req)
}

// login opens a session with a fresh login link and returns the overview.
func (c *inspectorClient) login() string {
	c.t.Helper()
	link, err := controlapi.NewClient(c.ts.dir).InspectorLogin(context.Background())
	if err != nil {
		c.t.Fatalf("InspectorLogin() error = %v", err)
	}
	code, body := c.get(link.Path)
	if code != http.StatusOK {
		c.t.Fatalf("GET %s = %d %q, want the overview", link.Path, code, body)
	}
	return body
}

func TestInspectorLogin(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	c := newInspectorClient(t, ts)
	if code, _ := c.get(InspectorPath + "/"); code != http.StatusUnauthorized {
		t.Fatalf("GET %s/ without a session = %d, want %d", InspectorPath, code, http.StatusUnauthorized)
	}
	if code, _ := c.get(InspectorPath + "/login?code=guess"); code != http.StatusUnauthorized {
		t.Fatalf("GET login with an unknown code = %d, want %d", code, http.StatusUnauthorized)
	}

	link, err := controlapi.NewClient(ts.dir).InspectorLogin(context.Background())
	if err != nil {
		t.Fatalf("InspectorLogin() error = %v", err)
	}
	if code, body := c.get(link.Path); code != http.StatusOK || !strings.Contains(body, "<h1>coder") {
		t.Fatalf("GET %s = %d %q, want the overview", link.Path, code, body)
	}
	other := newInspectorClient(t, ts)
	if code, _ := other.get(link.Path); code != http.StatusUnauthorized {
		t.Fatalf("GET %s a second time = %d, want %d", link.Path, code, http.StatusUnauthorized)
	}
	if code, _ := c.get(InspectorPath + "/"); code != http.StatusOK {
		t.Fatalf("GET %s/ with the session = %d, want %d", InspectorPath, code, http.StatusOK)
	}
}

func TestInspectorShowsProject(t *testing.T) {
	t.Parallel()
	overview := newInspectorClient(t, startProjectServer(t)).login()
	for _, want := range []string{"npx -y server-github", "unset: CODER_BOT_TOKEN", "Skills: deploy"} {
		if !strings.Contains(overview, want) {
			t.Fatalf("overview = %q, want it to contain %q", overview, want)
		}
	}
}

func TestInspectorShowsBackup(t *testing.T) {
	t.Parallel()
	to := t.TempDir()
	ts := startServer(t, serverOpts{
		secrets: agentdir.Secrets{"BACKUP_KEY": "k"},
		configure: func(c *agentdir.Config) {
			c.Backup = agentdir.BackupConfig{To: to, Key: "secret:BACKUP_KEY", Interval: "1h"}
		},
	})
	overview := newInspectorClient(t, ts).login()
	if want := "<th>Backup</th><td>" + to + ", "; !strings.Contains(overview, want) {
		t.Fatalf("overview = %q, want it to contain %q", overview, want)
	}
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func TestInspectorShowsConversationsAndAnswersApprovals(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	a2ac := ts.client(ts.pair("me", auth.RoleOwner))
	hello := send(t, a2ac, userText("hello", "", ""))
	task := send(t, a2ac, userText("ask", "", ""))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task state = %s, want input-required", task.Status.State)
	}

	c := newInspectorClient(t, ts)
	overview := c.login()
	for _, want := range []string{"Pending approvals", "git push", "/conversations/" + hello.ContextID, "hello"} {
		if !strings.Contains(overview, want) {
			t.Fatalf("overview = %q, want it to contain %q", overview, want)
		}
	}
	code, page := c.get(InspectorPath + "/conversations/" + hello.ContextID)
	if code != http.StatusOK || !strings.Contains(page, "Hello from fakeacp.") {
		t.Fatalf("GET conversation = %d %q, want the agent's answer", code, page)
	}
	if code, _ := c.get(InspectorPath + "/conversations/unknown"); code != http.StatusNotFound {
		t.Fatalf("GET unknown conversation = %d, want %d", code, http.StatusNotFound)
	}

	m := csrfField.FindStringSubmatch(overview)
	if m == nil {
		t.Fatalf("overview = %q, want a form with a CSRF token", overview)
	}
	action := regexp.MustCompile(`action="(` + InspectorPath + `/[^"]+)"`).FindStringSubmatch(overview)
	if action == nil {
		t.Fatalf("overview = %q, want an approval form", overview)
	}
	if code, _ := c.post(action[1], url.Values{"option": {"allow"}}); code != http.StatusForbidden {
		t.Fatalf("POST answer without the CSRF token = %d, want %d", code, http.StatusForbidden)
	}
	c.site = "cross-site"
	if code, _ := c.post(action[1], url.Values{"option": {"allow"}, "csrf": {m[1]}}); code != http.StatusForbidden {
		t.Fatalf("cross-site POST answer = %d, want %d", code, http.StatusForbidden)
	}
	c.site = "same-origin"
	if code, body := c.post(action[1], url.Values{"option": {"allow"}, "csrf": {m[1]}}); code != http.StatusOK || strings.Contains(body, "git push origin main") {
		t.Fatalf("POST answer = %d %q, want the overview without the approval", code, body)
	}
	wantResult(t, waitState(t, a2ac, task.ID, a2a.TaskStateCompleted), a2a.TaskStateCompleted, "Pushed (allow).")
}

func TestInspectorAnswersMembersApproval(t *testing.T) {
	t.Parallel()
	ts := startServer(t, serverOpts{})
	member := ts.client(ts.pair("anna", auth.RoleMember))
	task := send(t, member, userText("ask", "", ""))
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task state = %s, want input-required", task.Status.State)
	}

	c := newInspectorClient(t, ts)
	var overview string
	action := regexp.MustCompile(`action="(` + InspectorPath + `/tasks/[^"]+)"`)
	for range 100 {
		if overview = c.login(); action.MatchString(overview) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m, a := csrfField.FindStringSubmatch(overview), action.FindStringSubmatch(overview)
	if m == nil || a == nil {
		t.Fatalf("overview = %q, want the member's approval with a form", overview)
	}
	if code, body := c.post(a[1], url.Values{"option": {"allow"}, "csrf": {m[1]}}); code != http.StatusOK {
		t.Fatalf("POST answer = %d %q, want the overview", code, body)
	}
	wantResult(t, waitState(t, member, task.ID, a2a.TaskStateCompleted), a2a.TaskStateCompleted, "Pushed (allow).")
}
