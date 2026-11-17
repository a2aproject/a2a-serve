// Command acpgolden records the events of acpagent (adk-go PR 2021, commit
// 83c1a4b) for a conversation with fakeacp, as JSON lines that AcpAgent's
// tests compare with. It builds in a checkout of adk-go at that commit,
// copied to examples/acpgolden:
//
//	MODE=none acpgolden FAKEACP TURN... > none.jsonl
//	MODE=sse acpgolden FAKEACP TURN... > sse.jsonl
//
// with the turns of test_matches_acpagent_events, each in an empty directory.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/exp/acpagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type record struct {
	Turn         string   `json:"turn"`
	Partial      bool     `json:"partial"`
	TurnComplete bool     `json:"turn_complete"`
	Texts        []string `json:"texts"`
	Thought      []bool   `json:"thought"`
	ErrorMessage string   `json:"error_message"`
}

func main() {
	ctx := context.Background()
	ag, closer, err := acpagent.New(ctx, acpagent.Config{Name: "coder", Command: []string{os.Args[1]}})
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := closer.Close(); err != nil {
			log.Print(err)
		}
	}()
	svc := session.InMemoryService()
	r, err := runner.New(runner.Config{AppName: "golden", Agent: ag, SessionService: svc})
	if err != nil {
		log.Fatal(err)
	}
	s, err := svc.Create(ctx, &session.CreateRequest{AppName: "golden", UserID: "u"})
	if err != nil {
		log.Fatal(err)
	}
	cfg := agent.RunConfig{StreamingMode: agent.StreamingModeSSE}
	if os.Getenv("MODE") == "none" {
		cfg.StreamingMode = agent.StreamingModeNone
	}
	enc := json.NewEncoder(os.Stdout)
	for _, turn := range os.Args[2:] {
		for ev, err := range r.Run(ctx, "u", s.Session.ID(), genai.NewContentFromText(turn, genai.RoleUser), cfg) {
			if err != nil {
				log.Fatalf("%s: %v", turn, err)
			}
			rec := record{Turn: turn, Partial: ev.Partial, TurnComplete: ev.TurnComplete, Texts: []string{}, Thought: []bool{}, ErrorMessage: ev.ErrorMessage}
			if ev.Content != nil {
				for _, p := range ev.Content.Parts {
					if p.Text != "" {
						rec.Texts = append(rec.Texts, p.Text)
						rec.Thought = append(rec.Thought, p.Thought)
					}
				}
			}
			if err := enc.Encode(rec); err != nil {
				log.Fatal(err)
			}
		}
	}
}
