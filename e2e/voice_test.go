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
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/go-cmp/cmp"

	"github.com/a2aproject/a2a-serve/internal/messenger/telegram"
	"github.com/a2aproject/a2a-serve/internal/messenger/telegram/telegramtest"
	"github.com/a2aproject/a2a-serve/internal/speech/speechtest"
)

type doctorCheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

func TestVoice(t *testing.T) {
	t.Parallel()
	svc := speechtest.NewServer(t, speechtest.Options{Key: "sk-voice", Transcript: "scheduled check"})
	api := telegramtest.NewServer(t, telegramtest.Options{})
	me := telegramtest.User{ID: 42, FirstName: "Anna", Username: "anna"}
	owner := newUser(t)
	owner.env = append(owner.env, telegram.EnvServerURL+"="+api.URL, "E2E_BOT_TOKEN=123:abc", "VOICE_KEY=sk-voice")
	coder := owner.startAgent("coder")
	coder.serve("config", "set", "--voice", svc.URL(), "--voice-key", "env:VOICE_KEY", "--voice-name", "af_heart", "--restart")

	var checks []doctorCheck
	coder.serveJSON(&checks, "doctor", "--online")
	i := slices.IndexFunc(checks, func(c doctorCheck) bool { return c.ID == "voice" })
	if i < 0 || checks[i].Status != "ok" || checks[i].Message != "voice service "+svc.URL()+" transcribes and speaks" {
		t.Fatalf("a2a-serve doctor --online voice check = %+v, want it to transcribe and speak", checks)
	}

	var card a2a.AgentCard
	if err := json.Unmarshal([]byte(coder.serve("card", "--url=local")), &card); err != nil {
		t.Fatalf("a2a-serve card printed invalid JSON: %v", err)
	}
	if !slices.Contains(card.DefaultInputModes, "audio/ogg") || !slices.Contains(card.DefaultOutputModes, "audio/ogg") {
		t.Fatalf("card modes = %q in, %q out, want audio/ogg in both", card.DefaultInputModes, card.DefaultOutputModes)
	}

	var added struct {
		OwnerLink *struct {
			Code string `json:"code"`
		} `json:"ownerLink"`
	}
	coder.serveJSON(&added, "messenger", "add", "telegram", "--token", "env:E2E_BOT_TOKEN")
	if added.OwnerLink == nil {
		t.Fatalf("a2a-serve messenger add telegram printed no owner link")
	}
	c := &telegramChat{t: t, api: api, me: me, timeout: 30 * time.Second}
	c.say("/start "+added.OwnerLink.Code, "Linked as owner. Write to talk to coder.")

	api.SetFile("voice1", []byte("OggS voice"))
	last := c.lastID()
	api.SayVoice(me, "voice1")
	c.wait(last, func(m telegramtest.Message) bool { return m.Plain() == `🎤 "scheduled check"` })
	c.wait(last, func(m telegramtest.Message) bool { return strings.Contains(m.Plain(), "ran scheduled check") })

	c.say("/voice on", "Voice replies are on. I'll send a voice note with each answer. /voice off turns them off.")
	last = c.lastID()
	api.Say(me, "scheduled again")
	c.wait(last, func(m telegramtest.Message) bool { return strings.Contains(m.Plain(), "ran scheduled again") })
	c.wait(last, func(m telegramtest.Message) bool { return m.Voice == "reply.ogg" })

	last = c.lastID()
	api.Say(me, "speak")
	long := c.wait(last, func(m telegramtest.Message) bool { return strings.Contains(m.Plain(), "The long answer") })
	if strings.Contains(long.Plain(), "speak") || strings.Contains(long.Plain(), "short answer") {
		t.Fatalf("reply = %q, want it without the <speak> block", long.Plain())
	}
	c.wait(last, func(m telegramtest.Message) bool { return m.Voice == "reply.ogg" })
	var spoken []string
	for _, r := range svc.Requests() {
		if r.Path == "/v1/audio/speech" && r.Voice == "af_heart" {
			spoken = append(spoken, r.Input)
		}
	}
	if !slices.Contains(spoken, "The short answer.") {
		t.Fatalf("spoken texts in voice af_heart = %q, want the <speak> block among them", spoken)
	}

	req, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"messageId": "voice-call-1", "role": "ROLE_USER",
			"parts": []map[string]any{{"raw": []byte("OggS voice"), "mediaType": "audio/ogg", "filename": "voice.ogg"}},
		},
		"configuration": map[string]any{"acceptedOutputModes": []string{"text/plain", "audio/ogg"}},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	task := owner.mustSend(coder.url, "--request-payload", string(req))
	var got []string
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			if raw := p.Raw(); len(raw) > 0 {
				got = append(got, p.MediaType+" "+string(raw))
				continue
			}
			got = append(got, p.Text())
		}
	}
	want := []string{"ran scheduled check", "audio/ogg " + string(speechtest.Speech)}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("A2A voice call wrong artifacts (-want +got) diff = %s", diff)
	}
}

func TestVoiceSetupHintWorksAsPrinted(t *testing.T) {
	t.Parallel()
	svc := speechtest.NewServer(t, speechtest.Options{})
	api := telegramtest.NewServer(t, telegramtest.Options{})
	me := telegramtest.User{ID: 42, FirstName: "Anna", Username: "anna"}
	owner := newUser(t)
	owner.env = append(owner.env, telegram.EnvServerURL+"="+api.URL, "E2E_BOT_TOKEN=123:abc")
	coder := owner.startAgent("coder")
	var added struct {
		OwnerLink *struct {
			Code string `json:"code"`
		} `json:"ownerLink"`
	}
	coder.serveJSON(&added, "messenger", "add", "telegram", "--token", "env:E2E_BOT_TOKEN")
	if added.OwnerLink == nil {
		t.Fatalf("a2a-serve messenger add telegram printed no owner link")
	}
	c := &telegramChat{t: t, api: api, me: me, timeout: 30 * time.Second}
	c.say("/start "+added.OwnerLink.Code, "Linked as owner. Write to talk to coder.")

	hint := c.sayAndWait("/voice on", func(m telegramtest.Message) bool { return strings.Contains(m.Plain(), "aren't set up") })
	cmds := printedCommands(t, hint.Text, "a2a-serve ")
	for _, cmd := range cmds {
		cmd = replaceWord(cmd, "<service>", svc.URL())
		owner.run(coder.dir, serveCommand(cmd)...)
	}
	coder.serve("restart")
	c.say("/voice on", "Voice replies are on. I'll send a voice note with each answer. /voice off turns them off.")
}
