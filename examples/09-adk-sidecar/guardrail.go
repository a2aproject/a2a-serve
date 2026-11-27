package adksidecar

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// ToolCheckRecord records a before_tool callback observation.
type ToolCheckRecord struct {
	Tool string
	Args map[string]any
}

// ToolResultRecord records an after_tool callback observation.
type ToolResultRecord struct {
	Tool   string
	Result map[string]any
}

// GuardrailConfig configures a GuardrailPlugin.
type GuardrailConfig struct {
	Name                    string
	DenyCommands            []string
	BlockedPromptSubstrings []string
}

// GuardrailPlugin enforces model and tool guardrails on a sandboxed agent and
// records the callbacks it saw.
type GuardrailPlugin struct {
	plugin                  *plugin.Plugin
	denyCommands            []string
	blockedPromptSubstrings []string

	mu             sync.Mutex
	modelRequests  []string
	modelResponses []string
	toolChecks     []ToolCheckRecord
	toolResults    []ToolResultRecord
}

// NewGuardrailPlugin builds a GuardrailPlugin with sensible defaults matching
// the demo (denying "rm -rf*" / "rm -fr*" and blocking "BLOCK_MODEL_CALL").
func NewGuardrailPlugin(cfg GuardrailConfig) (*GuardrailPlugin, error) {
	if cfg.Name == "" {
		cfg.Name = "guardrail_plugin"
	}
	if len(cfg.DenyCommands) == 0 {
		cfg.DenyCommands = []string{"rm -rf*", "rm -fr*"}
	}
	if len(cfg.BlockedPromptSubstrings) == 0 {
		cfg.BlockedPromptSubstrings = []string{"BLOCK_MODEL_CALL"}
	}
	g := &GuardrailPlugin{
		denyCommands:            slices.Clone(cfg.DenyCommands),
		blockedPromptSubstrings: slices.Clone(cfg.BlockedPromptSubstrings),
	}
	p, err := plugin.New(plugin.Config{
		Name:                cfg.Name,
		BeforeModelCallback: g.beforeModel,
		AfterModelCallback:  g.afterModel,
		BeforeToolCallback:  g.beforeTool,
		AfterToolCallback:   g.afterTool,
	})
	if err != nil {
		return nil, err
	}
	g.plugin = p
	return g, nil
}

// Plugin returns the underlying ADK *plugin.Plugin.
func (g *GuardrailPlugin) Plugin() *plugin.Plugin { return g.plugin }

func (g *GuardrailPlugin) ModelRequests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.modelRequests)
}

func (g *GuardrailPlugin) ModelResponses() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.modelResponses)
}

func (g *GuardrailPlugin) ToolChecks() []ToolCheckRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.toolChecks)
}

func (g *GuardrailPlugin) ToolResults() []ToolResultRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.toolResults)
}

func (g *GuardrailPlugin) beforeModel(_ agent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	g.mu.Lock()
	g.modelRequests = append(g.modelRequests, req.Model)
	g.mu.Unlock()
	slog.Info("GuardrailPlugin.before_model_callback", "model", req.Model)

	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.Text == "" {
				continue
			}
			for _, marker := range g.blockedPromptSubstrings {
				if marker != "" && strings.Contains(p.Text, marker) {
					slog.Warn("GuardrailPlugin blocked model request", "marker", marker)
					return &model.LLMResponse{
						Content: &genai.Content{
							Role: genai.RoleModel,
							Parts: []*genai.Part{
								{Text: fmt.Sprintf("Blocked by GuardrailPlugin: prompt contained %q.", marker)},
							},
						},
					}, nil
				}
			}
		}
	}
	return nil, nil
}

func (g *GuardrailPlugin) afterModel(_ agent.Context, resp *model.LLMResponse, _ error) (*model.LLMResponse, error) {
	var b strings.Builder
	if resp != nil && resp.Content != nil {
		for _, p := range resp.Content.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text)
			}
		}
	}
	text := b.String()
	snippet := text
	if len(snippet) > 120 {
		snippet = snippet[:120]
	}
	g.mu.Lock()
	g.modelResponses = append(g.modelResponses, snippet)
	g.mu.Unlock()
	slog.Info("GuardrailPlugin.after_model_callback", "chars", len(text))
	return nil, nil
}

func (g *GuardrailPlugin) beforeTool(_ agent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	g.mu.Lock()
	g.toolChecks = append(g.toolChecks, ToolCheckRecord{
		Tool: t.Name(),
		Args: maps.Clone(args),
	})
	g.mu.Unlock()

	command, _ := args["command"].(string)
	if command == "" {
		return nil, nil
	}
	for _, pattern := range g.denyCommands {
		if matchGlob(pattern, command) {
			msg := fmt.Sprintf("GuardrailPlugin denied command matching %q", pattern)
			slog.Warn(msg, "command", command)
			return map[string]any{"error": msg}, nil
		}
	}
	return nil, nil
}

func (g *GuardrailPlugin) afterTool(_ agent.Context, t tool.Tool, _ map[string]any, result map[string]any, _ error) (map[string]any, error) {
	g.mu.Lock()
	g.toolResults = append(g.toolResults, ToolResultRecord{
		Tool:   t.Name(),
		Result: maps.Clone(result),
	})
	g.mu.Unlock()
	slog.Info("GuardrailPlugin.after_tool_callback", "tool", t.Name())
	return nil, nil
}

// matchGlob matches fnmatch-style wildcards ('*' and '?'), allowing '*' to cross '/'.
func matchGlob(pattern, s string) bool {
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == s
	}
	p, t := 0, 0
	starIdx, matchIdx := -1, 0
	for t < len(s) {
		if p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[t]) {
			p++
			t++
		} else if p < len(pattern) && pattern[p] == '*' {
			starIdx = p
			matchIdx = t
			p++
		} else if starIdx != -1 {
			p = starIdx + 1
			matchIdx++
			t = matchIdx
		} else {
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
