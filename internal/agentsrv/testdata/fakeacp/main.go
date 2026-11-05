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

// Command fakeacp is a scripted ACP agent for the agent server's conformance
// tests. The prompt text selects the behavior:
//
//	hello            two message chunks
//	tool             a tool call with progress
//	ask              a permission request; the answer decides the reply
//	ask twice        two permission requests in one turn
//	slow             chunks until canceled
//	remember <x>     stores x in the session
//	recall           replies with the stored value
//	crash            exits mid-turn
//	mcp              replies with the MCP servers of session/new
//	pid              replies with the host name and process ID
//	agents           replies with the agents URL and the socket that
//	                 reaches it
//	ls <dir>         replies with the entries of dir
//	scheduled ...    replies with "ran" and the prompt, for schedule runs
//	markdown         replies with Markdown that uses most of the syntax
//	call <agent> <t> sends t to an agent through the agents URL of the prompt
//	acall <agent> <t> the same, without waiting for the result
//	parallel <dir> <agent> <t>
//	                 a permission request; while it waits, once <dir>/go
//	                 exists, a call like call, whose output goes to
//	                 <dir>/result
//
// Sessions are kept in .fakeacp in the working directory, so session/load
// works after a restart.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// markdownSample is a reply with the Markdown that agents write, and the
// characters MarkdownV2 needs escaped.
const markdownSample = "# Release v1.2.3\n\n" +
	"**Bold**, *italic*, ~~struck~~ and `inline_code()`.\n\n" +
	"- one. two! (three)\n" +
	"- [a link](https://example.com/a_b?c=d) and 1 + 1 = 2 > 1 #tag {x} |pipe|\n\n" +
	"```go\nfmt.Println(\"a_b*c\")\n```\n\n" +
	"> quoted text\n\nDone."

// agentsLinePrefix starts the line the agent server adds to the first
// prompt of a session.
const agentsLinePrefix = "[a2a agents]"

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type session struct {
	Memory     string            `json:"memory"`
	MCPServers []json.RawMessage `json:"mcpServers"`
	// AgentsURL is from the last agents line of the session, which a real
	// agent keeps in its history.
	AgentsURL string `json:"agentsURL"`
}

type agent struct {
	out     *json.Encoder
	outMu   sync.Mutex
	mu      sync.Mutex
	nextID  int
	pending map[string]chan message
	cancels map[string]chan struct{}
	dir     string
}

func main() {
	a := &agent{out: json.NewEncoder(os.Stdout), pending: map[string]chan message{}, cancels: map[string]chan struct{}{}}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			fmt.Fprintln(os.Stderr, "fakeacp: bad message:", err)
			continue
		}
		if m.Method == "" {
			a.mu.Lock()
			ch := a.pending[string(m.ID)]
			delete(a.pending, string(m.ID))
			a.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		// A cancel that follows a prompt finds it, also before the prompt
		// runs, as with an agent that reads its input in order.
		if m.Method == "session/prompt" {
			go a.prompt(m, a.registerPrompt(m))
			continue
		}
		go a.handle(m)
	}
}

// registerPrompt returns the channel that session/cancel closes to stop the
// prompt m.
func (a *agent) registerPrompt(m message) chan struct{} {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	canceled := make(chan struct{})
	if json.Unmarshal(m.Params, &p) != nil {
		return canceled
	}
	a.mu.Lock()
	a.cancels[p.SessionID] = canceled
	a.mu.Unlock()
	return canceled
}

func (a *agent) write(v any) {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	if err := a.out.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "fakeacp: write:", err)
		os.Exit(1)
	}
}

func (a *agent) respond(id json.RawMessage, result any) {
	a.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (a *agent) fail(id json.RawMessage, msg string) {
	a.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": msg}})
}

func (a *agent) notify(sessionID string, update map[string]any) {
	a.write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": update}})
}

func (a *agent) call(method string, params any) message {
	a.mu.Lock()
	a.nextID++
	id := fmt.Sprintf("%d", a.nextID)
	ch := make(chan message, 1)
	a.pending[id] = ch
	a.mu.Unlock()
	a.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": method, "params": params})
	return <-ch
}

func (a *agent) chunk(sessionID, text string) {
	a.notify(sessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
}

// askPush asks permission to run git push in tool call toolCallID, and
// returns the chosen option, or false if the request was canceled.
func (a *agent) askPush(sessionID, toolCallID string) (string, bool) {
	a.notify(sessionID, map[string]any{"sessionUpdate": "tool_call", "toolCallId": toolCallID, "title": "git push", "kind": "execute", "status": "pending", "rawInput": map[string]any{"command": "git push origin main"}})
	resp := a.call("session/request_permission", map[string]any{
		"sessionId": sessionID,
		"toolCall":  map[string]any{"toolCallId": toolCallID},
		"options": []map[string]any{
			{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
			{"optionId": "always", "name": "Always", "kind": "allow_always"},
			{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
		},
	})
	var r struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil || r.Outcome.Outcome == "cancelled" {
		return "", false
	}
	return r.Outcome.OptionID, true
}

func (a *agent) sessionPath(id string) string { return filepath.Join(a.dir, ".fakeacp", id+".json") }

func (a *agent) load(id string) (*session, error) {
	data, err := os.ReadFile(a.sessionPath(id))
	if err != nil {
		return nil, err
	}
	var s session
	return &s, json.Unmarshal(data, &s)
}

func (a *agent) save(id string, s *session) error {
	if err := os.MkdirAll(filepath.Dir(a.sessionPath(id)), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeAtomic(a.sessionPath(id), data)
}

// writeAtomic writes path so that readers, also the tests, never see it
// partly written.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *agent) handle(m message) {
	switch m.Method {
	case "initialize":
		a.respond(m.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true}})
	case "session/new":
		var p struct {
			Cwd        string            `json:"cwd"`
			MCPServers []json.RawMessage `json:"mcpServers"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			a.fail(m.ID, err.Error())
			return
		}
		a.dir = p.Cwd
		id := fmt.Sprintf("s%d", time.Now().UnixNano())
		if err := a.save(id, &session{MCPServers: p.MCPServers}); err != nil {
			a.fail(m.ID, err.Error())
			return
		}
		a.respond(m.ID, map[string]any{"sessionId": id})
	case "session/load":
		var p struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			a.fail(m.ID, err.Error())
			return
		}
		a.dir = p.Cwd
		if _, err := a.load(p.SessionID); err != nil {
			a.fail(m.ID, "unknown session")
			return
		}
		a.respond(m.ID, map[string]any{})
	case "session/cancel":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			a.mu.Lock()
			if ch := a.cancels[p.SessionID]; ch != nil {
				close(ch)
				delete(a.cancels, p.SessionID)
			}
			a.mu.Unlock()
		}
	default:
		if len(m.ID) > 0 {
			a.fail(m.ID, "method not found: "+m.Method)
		}
	}
}

func (a *agent) prompt(m message, canceled chan struct{}) {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		a.fail(m.ID, err.Error())
		return
	}
	var texts []string
	agentsURL := ""
	for _, b := range p.Prompt {
		if b.Type != "text" {
			continue
		}
		if strings.HasPrefix(b.Text, agentsLinePrefix) {
			agentsURL = parseAgentsURL(b.Text)
			continue
		}
		texts = append(texts, b.Text)
	}
	text := strings.TrimSpace(strings.Join(texts, "\n"))
	sid := p.SessionID
	s, err := a.load(sid)
	if err != nil {
		a.fail(m.ID, "unknown session")
		return
	}
	lined := agentsURL != ""
	if lined && agentsURL != s.AgentsURL {
		s.AgentsURL = agentsURL
		if err := a.save(sid, s); err != nil {
			a.fail(m.ID, err.Error())
			return
		}
	}
	agentsURL = s.AgentsURL
	end := func(reason string) { a.respond(m.ID, map[string]any{"stopReason": reason}) }

	cmd, arg, _ := strings.Cut(text, " ")
	// A scheduled run asks like its prompt, after the server's preamble line.
	if _, prompt, ok := strings.Cut(text, "\n"); ok && strings.HasPrefix(text, "Scheduled run of ") && prompt == "ask" {
		cmd = prompt
	}
	switch strings.ToLower(cmd) {
	case "tool":
		a.notify(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "list files", "kind": "read", "status": "pending"})
		a.notify(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "in_progress"})
		a.notify(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "completed"})
		a.chunk(sid, "Listed the files.")
		end("end_turn")
	case "ask":
		if arg == "twice" {
			first, ok := a.askPush(sid, "t2")
			if !ok {
				end("cancelled")
				return
			}
			second, ok := a.askPush(sid, "t3")
			if !ok {
				end("cancelled")
				return
			}
			a.chunk(sid, "Pushed twice ("+first+", "+second+").")
			end("end_turn")
			return
		}
		option, ok := a.askPush(sid, "t2")
		if !ok {
			end("cancelled")
			return
		}
		if option == "deny" {
			a.notify(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t2", "status": "failed"})
			a.chunk(sid, "Push denied.")
		} else {
			a.notify(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t2", "status": "completed"})
			a.chunk(sid, "Pushed ("+option+").")
		}
		end("end_turn")
	case "slow":
		for i := 0; ; i++ {
			select {
			case <-canceled:
				end("cancelled")
				return
			case <-time.After(50 * time.Millisecond):
				a.chunk(sid, ".")
			}
		}
	case "remember":
		s.Memory = arg
		if err := a.save(sid, s); err != nil {
			a.fail(m.ID, err.Error())
			return
		}
		a.chunk(sid, "OK.")
		end("end_turn")
	case "recall":
		a.chunk(sid, "You said: "+s.Memory)
		end("end_turn")
	case "mcp":
		data, err := json.Marshal(s.MCPServers)
		if err != nil {
			a.fail(m.ID, err.Error())
			return
		}
		a.chunk(sid, string(data))
		end("end_turn")
	case "scheduled":
		a.chunk(sid, "ran "+text)
		end("end_turn")
	case "markdown":
		a.chunk(sid, markdownSample)
		end("end_turn")
	case "line":
		if lined {
			a.chunk(sid, "with agents line")
		} else {
			a.chunk(sid, "without agents line")
		}
		end("end_turn")
	case "pid":
		host, err := os.Hostname()
		if err != nil {
			a.fail(m.ID, err.Error())
			return
		}
		a.chunk(sid, fmt.Sprintf("pid %s/%d", host, os.Getpid()))
		end("end_turn")
	case "agents":
		host, err := os.Hostname()
		if err != nil {
			a.fail(m.ID, err.Error())
			return
		}
		a.chunk(sid, agentsURL+" "+os.Getenv("A2ACLI_UNIX_SOCKET")+" "+host)
		end("end_turn")
	case "ls":
		entries, err := os.ReadDir(arg)
		if err != nil {
			a.chunk(sid, "ls error: "+err.Error())
			end("end_turn")
			return
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		a.chunk(sid, "ls: "+strings.Join(names, " "))
		end("end_turn")
	case "crash":
		a.chunk(sid, "about to crash")
		os.Exit(3)
	case "call", "acall":
		agentName, msg, _ := strings.Cut(arg, " ")
		out, err := callAgent(agentsURL, agentName, msg, strings.ToLower(cmd) == "acall")
		if err != nil {
			a.chunk(sid, "tool error: "+err.Error())
		} else {
			a.chunk(sid, "tool result: "+out)
		}
		end("end_turn")
	case "parallel":
		dir, rest, _ := strings.Cut(arg, " ")
		agentName, msg, _ := strings.Cut(rest, " ")
		called := make(chan struct{})
		go func() {
			defer close(called)
			callWhenSignaled(agentsURL, dir, agentName, msg)
		}()
		option, ok := a.askPush(sid, "t2")
		<-called
		if !ok {
			end("cancelled")
			return
		}
		a.chunk(sid, "Pushed ("+option+").")
		end("end_turn")
	default:
		a.chunk(sid, "Hello ")
		a.chunk(sid, "from fakeacp.")
		end("end_turn")
	}
}

// parseAgentsURL returns the agents URL of the line the agent server adds
// to prompts.
func parseAgentsURL(line string) string {
	_, rest, ok := strings.Cut(line, "Agents URL for this conversation: ")
	if !ok {
		return ""
	}
	u, _, _ := strings.Cut(rest, " ")
	return u
}

// callWhenSignaled calls an agent once dir/go exists, and writes the result
// or error to dir/result.
func callWhenSignaled(agentsURL, dir, name, text string) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "fakeacp: no signal in", dir)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, err := callAgent(agentsURL, name, text, false)
	result := "tool result: " + out
	if err != nil {
		result = "tool error: " + err.Error()
	}
	if err := writeAtomic(filepath.Join(dir, "result"), []byte(result)); err != nil {
		fmt.Fprintln(os.Stderr, "fakeacp: writing the result:", err)
	}
}

// callAgent sends text to an agent the way the a2a CLI does: it resolves
// the agent's card under the agents URL and sends a message.
func callAgent(agentsURL, name, text string, async bool) (string, error) {
	if agentsURL == "" {
		return "", fmt.Errorf("no agents URL in the prompt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hc := httpClient()
	card, err := agentcard.NewResolver(hc).Resolve(ctx, agentsURL+"/"+name)
	if err != nil {
		return "", err
	}
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(hc), a2aclient.WithRESTTransport(hc))
	if err != nil {
		return "", err
	}
	defer func() {
		if err := client.Destroy(); err != nil {
			fmt.Fprintln(os.Stderr, "closing client:", err)
		}
	}()
	req := &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))}
	if async {
		req.Config = &a2a.SendMessageConfig{ReturnImmediately: true}
	}
	res, err := client.SendMessage(ctx, req)
	if err != nil {
		return "", err
	}
	switch r := res.(type) {
	case *a2a.Message:
		return partsText(r.Parts), nil
	case *a2a.Task:
		var parts []*a2a.Part
		for _, art := range r.Artifacts {
			parts = append(parts, art.Parts...)
		}
		if len(parts) == 0 && r.Status.Message != nil {
			parts = r.Status.Message.Parts
		}
		return fmt.Sprintf("task %s %s: %s", r.ID, r.Status.State, partsText(parts)), nil
	}
	return "", fmt.Errorf("unexpected result %T", res)
}

// httpClient honors A2ACLI_UNIX_SOCKET=<path> like the a2a CLI: every
// request goes through the socket.
func httpClient() *http.Client {
	path := os.Getenv("A2ACLI_UNIX_SOCKET")
	if path == "" {
		return http.DefaultClient
	}
	var d net.Dialer
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d.DialContext(ctx, "unix", path)
	}}}
}

func partsText(parts []*a2a.Part) string {
	var out []string
	for _, p := range parts {
		if t := p.Text(); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "")
}
