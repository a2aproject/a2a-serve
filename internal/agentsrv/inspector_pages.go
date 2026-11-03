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
	"html/template"
	"log/slog"
	"net/http"
	"time"
)

var inspectorPages = template.Must(template.New("").Funcs(template.FuncMap{
	"since": func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return time.Since(t).Round(time.Second).String() + " ago"
	},
	"when": func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Local().Format("2006-01-02 15:04")
	},
	"path": func() string { return InspectorPath },
}).Parse(`
{{define "head"}}<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.}} · a2a-serve</title>
<style>
body{font-family:system-ui,sans-serif;max-width:64rem;margin:1.5rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}h2{font-size:1.1rem;margin-top:2rem;border-bottom:1px solid #ddd;padding-bottom:.25rem}
table{border-collapse:collapse;width:100%}td,th{text-align:left;padding:.3rem .5rem;border-bottom:1px solid #eee;vertical-align:top}
th{font-weight:600;color:#555}pre{white-space:pre-wrap;background:#f6f6f6;padding:.6rem;border-radius:.4rem;margin:.25rem 0}
.muted{color:#777}.err{color:#b00}.msg{margin:.5rem 0}.role{font-weight:600;font-size:.85rem;color:#555}
.approval{border:1px solid #e0c060;background:#fffbea;border-radius:.4rem;padding:.6rem;margin:.6rem 0}
button{font-size:.95rem;margin:.2rem .2rem 0 0;padding:.3rem .8rem}a{color:#0b57d0}
</style></head><body>{{end}}
{{define "foot"}}</body></html>{{end}}

{{define "overview"}}{{template "head" .Agent}}
<h1>{{.Agent}} <span class="muted">({{.Status.Harness}})</span></h1>
<table>
<tr><th>Running</th><td>for {{since .Status.Started}}, pid {{.Status.PID}}</td></tr>
<tr><th>Directory</th><td>{{.Status.Dir}}</td></tr>
<tr><th>Local</th><td>{{.Status.Local}}</td></tr>
{{if .Status.Private}}<tr><th>Private</th><td>{{.Status.Private}}{{if .Status.PrivateNote}} <span class="muted">({{.Status.PrivateNote}})</span>{{end}}</td></tr>{{end}}
{{if .Status.Public}}<tr><th>Public</th><td>{{.Status.Public}}</td></tr>{{end}}
<tr><th>Tasks</th><td>{{range $state, $n := .Status.Tasks}}{{$n}} {{$state}} &nbsp;{{else}}none{{end}}</td></tr>
{{if .Status.Owners}}<tr><th>Owner</th><td>{{range .Status.Owners}}{{.}} {{end}}</td></tr>{{end}}
{{with .Status.Backup}}<tr><th>Backup</th><td>{{.To}}, {{if .Last.IsZero}}none yet{{else}}last {{when .Last}}{{end}}, next {{when .Next}}{{if .LastError}} <span class="err">the last snapshot failed: {{.LastError}}</span>{{end}}</td></tr>{{end}}
</table>

<h2>Pending approvals</h2>
{{range .Approvals}}<div class="approval">
<div class="muted">{{if .Contact}}from {{.Contact}}, {{end}}asked {{since .Created}} · <a href="{{path}}/conversations/{{.ContextID}}">conversation</a></div>
<pre>{{.Text}}</pre>
<form method="post" action="{{.Action}}"><input type="hidden" name="csrf" value="{{$.CSRF}}">
{{range .Options}}<button name="option" value="{{.ID}}">{{.Label}}</button>{{end}}</form>
</div>{{else}}<p class="muted">Nothing waits for an answer.</p>{{end}}

<h2>Conversations</h2>
{{if .Conversations}}<table><tr><th>Updated</th><th>User</th><th>Chat</th><th>State</th><th>Tasks</th><th>First message</th></tr>
{{range .Conversations}}<tr><td><a href="{{path}}/conversations/{{.ContextID}}">{{when .Updated}}</a></td><td>{{.User}}</td><td>{{.Chat}}</td><td>{{.State}}</td><td>{{.Tasks}}</td><td>{{.Snippet}}</td></tr>
{{end}}</table>{{else}}<p class="muted">No conversations yet.</p>{{end}}

<h2>Messengers</h2>
{{if .Status.Channels}}<table><tr><th>Channel</th><th>Account</th><th></th></tr>
{{range .Status.Channels}}<tr><td>{{.Name}}</td><td>{{.Account}}</td><td>{{if .Error}}<span class="err">failing: {{.Error}}</span>{{end}}</td></tr>{{end}}
</table>{{else}}<p class="muted">None; add one with <code>a2a-serve channel add</code>.</p>{{end}}

<h2>MCP servers and skills</h2>
{{if .Status.ProjectError}}<p class="err">{{.Status.ProjectError}}</p>{{end}}
{{if .Status.MCPServers}}<table><tr><th>MCP server</th><th>Runs</th><th></th></tr>
{{range .Status.MCPServers}}<tr><td>{{.Name}}</td><td>{{.Summary}}</td><td>{{if .Missing}}<span class="err">not passed to the harness, unset: {{range .Missing}}{{.}} {{end}}(pass them with <code>--secret</code>)</span>{{end}}</td></tr>{{end}}
</table>{{else}}<p class="muted">No MCP servers in <code>.mcp.json</code>.</p>{{end}}
{{if .Status.Skills}}<p>Skills: {{range .Status.Skills}}{{.Name}} {{end}}</p>{{else}}<p class="muted">No skills.</p>{{end}}

<h2>Pairings</h2>
{{if .Tokens}}<table><tr><th>Name</th><th>Principal</th><th>Roles</th><th>Created</th><th>Last used</th></tr>
{{range .Tokens}}<tr><td>{{.Name}}</td><td>{{.Principal}}</td><td>{{range .Roles}}{{.}} {{end}}</td><td>{{when .Created}}</td><td>{{when .LastUsed}}</td></tr>{{end}}
</table>{{else}}<p class="muted">None; invite someone with <code>a2a-serve pair invite</code>.</p>{{end}}

<h2>Webhook nodes</h2>
{{if .Nodes}}<table><tr><th>Node</th><th>URL</th><th>Webhook</th><th>Paired</th></tr>
{{range .Nodes}}<tr><td>{{.Name}}</td><td>{{.URL}}</td><td>{{.Webhook}}</td><td>{{.Paired}}</td></tr>{{end}}
</table>{{else}}<p class="muted">None.</p>{{end}}

<h2>Contacts</h2>
{{if .Contacts}}<table><tr><th>Name</th><th>URL</th><th>Via</th></tr>
{{range .Contacts}}<tr><td>{{.Name}}</td><td>{{.URL}}</td><td>{{.Via}}</td></tr>{{end}}
</table>{{else}}<p class="muted">None; add one with <code>a2a-serve contact add</code>.</p>{{end}}
{{template "foot"}}{{end}}

{{define "conversation"}}{{template "head" .Agent}}
<p><a href="{{path}}/">← {{.Agent}}</a></p>
<h1>Conversation <span class="muted">{{.ContextID}}</span></h1>
{{if .Chat}}<p class="muted">Chat: {{.Chat}}</p>{{end}}
{{range .Tasks}}<h2>Task {{.ID}} <span class="muted">· {{.State}} · {{.User}} · {{when .Updated}}</span></h2>
{{range .Messages}}<div class="msg"><div class="role">{{.Role}}</div><pre>{{.Text}}</pre></div>{{end}}
{{range .Artifacts}}<div class="msg"><div class="role">artifact {{.Role}}</div><pre>{{.Text}}</pre></div>{{end}}
{{end}}
{{template "foot"}}{{end}}
`))

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := inspectorPages.ExecuteTemplate(w, name, data); err != nil {
		slog.Warn("rendering the inspector", "page", name, "error", err)
	}
}
