package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/full"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	adksidecar "github.com/a2aproject/a2a-serve/examples/09-adk-sidecar"
)

func main() {
	ctx := context.Background()

	var prompt string
	var seedDir string
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "-") {
		fs := flag.NewFlagSet("demo", flag.ExitOnError)
		fs.StringVar(&prompt, "prompt", "", "Run a single prompt and print the response and audit summary")
		fs.StringVar(&seedDir, "workspace", "./workspace", "Directory used to seed per-session workspaces")
		if err := fs.Parse(os.Args[1:]); err != nil {
			log.Fatalf("parsing flags: %v", err)
		}
	}
	if seedDir == "" {
		seedDir = "./workspace"
	}

	guardrail, err := adksidecar.NewGuardrailPlugin(adksidecar.GuardrailConfig{})
	if err != nil {
		log.Fatalf("creating guardrail plugin: %v", err)
	}

	var egressAllow []string
	for part := range strings.SplitSeq(os.Getenv("SIDECAR_EGRESS_ALLOW"), ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			egressAllow = append(egressAllow, trimmed)
		}
	}

	absSeed, err := filepath.Abs(seedDir)
	if err != nil {
		log.Fatalf("resolving workspace seed: %v", err)
	}

	backend, err := adksidecar.NewBackend(adksidecar.BackendConfig{
		Sandbox:     envOr("SIDECAR_SANDBOX", "docker"),
		EgressAllow: egressAllow,
		Workspace:   adksidecar.NewSessionWorkspace(absSeed, 0),
		Plugins:     []*plugin.Plugin{guardrail.Plugin()},
	})
	if err != nil {
		log.Fatalf("creating backend: %v", err)
	}
	defer func() {
		if err := backend.Close(); err != nil {
			log.Printf("closing backend: %v", err)
		}
	}()

	tools, err := adksidecar.DefaultADKTools(nil)
	if err != nil {
		log.Fatalf("creating ADK tools: %v", err)
	}

	agentProfile := adksidecar.DefaultAgent()
	modelName := adksidecar.DefaultModel()
	var llm model.LLM
	if agentProfile != "claude" && os.Getenv("GEMINI_API_KEY") != "" && isGeminiModel(modelName) {
		m, err := gemini.NewModel(ctx, strings.TrimPrefix(strings.TrimPrefix(modelName, "google/"), "models/"), &genai.ClientConfig{
			APIKey: os.Getenv("GEMINI_API_KEY"),
		})
		if err != nil {
			log.Fatalf("creating Gemini model: %v", err)
		}
		llm = m
	}

	coder, err := adksidecar.NewSandboxAgent(adksidecar.SandboxAgentConfig{
		Name: "coder",
		Description: "Sandboxed coding agent managed by A2AServeBackend / a2a-serve-sidecar. " +
			"Handles workspace inspection, edits, shell commands, and ADK host tools.",
		Backend:   backend,
		Agent:     agentProfile,
		Model:     llm,
		ModelName: modelName,
		Tools:     tools,
	})
	if err != nil {
		log.Fatalf("creating SandboxAgent: %v", err)
	}

	if prompt != "" {
		if err := runOneShot(ctx, backend, coder, guardrail, prompt); err != nil {
			log.Fatalf("run failed: %v", err)
		}
		return
	}

	cfg := &launcher.Config{
		AgentLoader:     agent.NewSingleLoader(coder),
		SessionService:  session.InMemoryService(),
		ArtifactService: artifact.InMemoryService(),
		PluginConfig: runner.PluginConfig{
			Plugins: []*plugin.Plugin{guardrail.Plugin()},
		},
	}

	l := full.NewLauncher()
	if err := l.Execute(ctx, cfg, os.Args[1:]); err != nil {
		log.Fatalf("launcher failed: %v\n\n%s", err, l.CommandLineSyntax())
	}
}

func runOneShot(ctx context.Context, backend *adksidecar.A2AServeBackend, coder *adksidecar.SandboxAgent, guardrail *adksidecar.GuardrailPlugin, prompt string) error {
	sessSvc := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:         "dev",
		Agent:           coder,
		SessionService:  sessSvc,
		ArtifactService: artifact.InMemoryService(),
		PluginConfig: runner.PluginConfig{
			Plugins: []*plugin.Plugin{guardrail.Plugin()},
		},
	})
	if err != nil {
		return err
	}

	created, err := sessSvc.Create(ctx, &session.CreateRequest{
		AppName: "dev",
		UserID:  "demo-user",
	})
	if err != nil {
		return err
	}

	fmt.Println("\n--- ADK Response ---")
	msg := genai.NewContentFromText(prompt, genai.RoleUser)
	for ev, err := range r.Run(ctx, "demo-user", created.Session.ID(), msg, agent.RunConfig{}) {
		if err != nil {
			return err
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			if p.Text != "" {
				fmt.Printf("[%s] %s\n", ev.Author, p.Text)
			} else if p.FunctionCall != nil {
				if ask, ok := p.FunctionCall.Args["input_required"]; ok {
					fmt.Printf("[%s APPROVAL REQUIRED] %v\n", ev.Author, ask)
				}
			}
		}
	}

	fmt.Println("\n--- Backend & GuardrailPlugin Audit Summary ---")
	mwEvents := backend.MiddlewareEvents()
	fmt.Printf("Egress middleware calls: %d\n", len(mwEvents))
	for _, ev := range mwEvents {
		fmt.Printf("  %s %s context=%s task=%s status=%d\n", ev.Method, ev.Path, ev.ContextID, ev.TaskID, ev.Status)
	}
	hookEvents := backend.HookEvents()
	fmt.Printf("Tool hook events: %d\n", len(hookEvents))
	for _, ev := range hookEvents {
		fmt.Printf("  %s context=%s task=%s decision=%s call=%v\n", ev.Hook, ev.ContextID, ev.TaskID, ev.Decision, ev.Call["title"])
	}
	fmt.Printf("GuardrailPlugin callbacks: before_model=%d after_model=%d before_tool=%d after_tool=%d\n",
		len(guardrail.ModelRequests()),
		len(guardrail.ModelResponses()),
		len(guardrail.ToolChecks()),
		len(guardrail.ToolResults()),
	)
	return nil
}

func isGeminiModel(name string) bool {
	trimmed := strings.TrimPrefix(strings.TrimSpace(name), "models/")
	return strings.HasPrefix(trimmed, "google/") || strings.HasPrefix(trimmed, "gemini")
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
