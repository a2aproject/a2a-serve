package adksidecar

import (
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/remoteagent/v2"
	"google.golang.org/adk/v2/auth"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
)

const (
	DefaultAgentProfile = "opencode"
	DefaultModelName    = "gemini-3.5-flash-lite"
)

// DefaultAgent returns SIDECAR_AGENT from the environment, or DefaultAgentProfile.
func DefaultAgent() string {
	if v := strings.TrimSpace(os.Getenv("SIDECAR_AGENT")); v != "" {
		return v
	}
	return DefaultAgentProfile
}

// DefaultModel returns ADK_MODEL from the environment, or DefaultModelName.
func DefaultModel() string {
	if v := strings.TrimSpace(os.Getenv("ADK_MODEL")); v != "" {
		return v
	}
	return DefaultModelName
}

// SandboxAgentConfig configures a SandboxAgent.
type SandboxAgentConfig struct {
	Name        string
	Description string
	Backend     *A2AServeBackend
	// Agent is the a2a-serve-sidecar --agent profile ("opencode" or "claude").
	// Defaults to "opencode".
	Agent string
	// Command replaces the agent's command line (for example, with a test agent).
	Command string
	// Model configures the ADK model.LLM used for routing and optional direct
	// invocation when no upstream API key is configured on the backend.
	Model model.LLM
	// ModelName configures a model ID string directly when Model is nil.
	ModelName            string
	Tools                []tool.Tool
	Plugins              []*plugin.Plugin
	BeforeModelCallbacks []llmagent.BeforeModelCallback
	AfterModelCallbacks  []llmagent.AfterModelCallback
	BeforeToolCallbacks  []llmagent.BeforeToolCallback
	AfterToolCallbacks   []llmagent.AfterToolCallback
}

// SandboxAgent is an ADK agent delegating to a coding agent inside
// a2a-serve-sidecar via remoteagent/v2 and A2AServeBackend.
type SandboxAgent struct {
	agent.Agent

	name                 string
	description          string
	backend              *A2AServeBackend
	agentProfile         string
	command              string
	llm                  model.LLM
	modelName            string
	tools                []tool.Tool
	plugins              []*plugin.Plugin
	beforeModelCallbacks []llmagent.BeforeModelCallback
	afterModelCallbacks  []llmagent.AfterModelCallback
	beforeToolCallbacks  []llmagent.BeforeToolCallback
	afterToolCallbacks   []llmagent.AfterToolCallback

	mu           sync.Mutex
	remote       agent.Agent
	pendingTasks map[string]a2a.TaskID
}

// NewSandboxAgent builds a SandboxAgent backed by cfg.Backend.
func NewSandboxAgent(cfg SandboxAgentConfig) (*SandboxAgent, error) {
	if cfg.Backend == nil {
		return nil, errors.New("SandboxAgent requires a non-nil Backend")
	}
	if cfg.Name == "" {
		cfg.Name = "coder"
	}
	profile := strings.TrimSpace(cfg.Agent)
	if profile == "" {
		profile = DefaultAgentProfile
	}
	modelName := strings.TrimSpace(cfg.ModelName)
	if cfg.Model != nil {
		modelName = strings.TrimSpace(cfg.Model.Name())
	}
	sa := &SandboxAgent{
		name:                 cfg.Name,
		description:          cfg.Description,
		backend:              cfg.Backend,
		agentProfile:         profile,
		command:              cfg.Command,
		llm:                  cfg.Model,
		modelName:            modelName,
		tools:                slices.Clone(cfg.Tools),
		plugins:              slices.Clone(cfg.Plugins),
		beforeModelCallbacks: slices.Clone(cfg.BeforeModelCallbacks),
		afterModelCallbacks:  slices.Clone(cfg.AfterModelCallbacks),
		beforeToolCallbacks:  slices.Clone(cfg.BeforeToolCallbacks),
		afterToolCallbacks:   slices.Clone(cfg.AfterToolCallbacks),
		pendingTasks:         make(map[string]a2a.TaskID),
	}
	a, err := agent.New(agent.Config{
		Name:        sa.name,
		Description: sa.description,
		Run:         sa.run,
	})
	if err != nil {
		return nil, err
	}
	sa.Agent = a
	return sa, nil
}

func (sa *SandboxAgent) AgentProfile() string                                { return sa.agentProfile }
func (sa *SandboxAgent) Command() string                                     { return sa.command }
func (sa *SandboxAgent) Model() model.LLM                                    { return sa.llm }
func (sa *SandboxAgent) ModelName() string                                   { return sa.modelName }
func (sa *SandboxAgent) Tools() []tool.Tool                                  { return slices.Clone(sa.tools) }
func (sa *SandboxAgent) Plugins() []*plugin.Plugin                           { return slices.Clone(sa.plugins) }
func (sa *SandboxAgent) BeforeModelCallbacks() []llmagent.BeforeModelCallback { return slices.Clone(sa.beforeModelCallbacks) }
func (sa *SandboxAgent) AfterModelCallbacks() []llmagent.AfterModelCallback   { return slices.Clone(sa.afterModelCallbacks) }
func (sa *SandboxAgent) BeforeToolCallbacks() []llmagent.BeforeToolCallback   { return slices.Clone(sa.beforeToolCallbacks) }
func (sa *SandboxAgent) AfterToolCallbacks() []llmagent.AfterToolCallback     { return slices.Clone(sa.afterToolCallbacks) }

// GeminiModelName returns the bare Gemini model ID (e.g. "gemini-3.5-flash-lite")
// if configured, or empty string otherwise.
func (sa *SandboxAgent) GeminiModelName() string {
	raw := strings.TrimPrefix(sa.modelName, "models/")
	if rest, ok := strings.CutPrefix(raw, "google/"); ok {
		return rest
	}
	if strings.HasPrefix(raw, "gemini") {
		return raw
	}
	return ""
}

// AnthropicModelName returns the bare Claude model ID (e.g. "claude-sonnet-4-5"
// or "sonnet") if configured, or empty string otherwise.
func (sa *SandboxAgent) AnthropicModelName() string {
	raw := strings.TrimPrefix(sa.modelName, "models/")
	if rest, ok := strings.CutPrefix(raw, "anthropic/"); ok {
		return rest
	}
	if strings.HasPrefix(raw, "claude") {
		return raw
	}
	if sa.agentProfile == "claude" && raw != "" && !strings.Contains(raw, "/") {
		return raw
	}
	return ""
}

// SidecarModelID returns the --model flag value for a2a-serve-sidecar, formatted
// for the configured agent profile (provider/model for opencode, bare model name
// for claude).
func (sa *SandboxAgent) SidecarModelID() string {
	modelID := strings.TrimPrefix(sa.modelName, "models/")
	if modelID == "" {
		return ""
	}
	if sa.agentProfile == "claude" {
		return strings.TrimPrefix(modelID, "anthropic/")
	}
	if strings.Contains(modelID, "/") {
		return modelID
	}
	switch {
	case strings.HasPrefix(modelID, "claude"):
		return "anthropic/" + modelID
	case strings.HasPrefix(modelID, "gpt"), strings.HasPrefix(modelID, "o1"), strings.HasPrefix(modelID, "o3"), strings.HasPrefix(modelID, "o4"):
		return "openai/" + modelID
	default:
		return "google/" + modelID
	}
}

func (sa *SandboxAgent) ensureRemote() (agent.Agent, error) {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	if sa.remote != nil {
		return sa.remote, nil
	}

	managed, err := sa.backend.ensureSidecar(sa)
	if err != nil {
		return nil, err
	}

	cfg := remoteagent.A2AConfig{
		Name:              sa.name,
		Description:       sa.description,
		AgentCardProvider: remoteagent.NewAgentCardProvider(managed.a2aURL() + "/.well-known/agent-card.json"),
		BeforeRequestCallbacks: []remoteagent.BeforeA2ARequestCallback{
			func(ctx agent.Context, req *a2a.SendMessageRequest) (*session.Event, error) {
				if req.Message == nil {
					return nil, nil
				}
				contextID := callbackContextID(ctx)
				req.Message.ContextID = contextID
				if req.Message.TaskID == "" {
					if tid := sa.pendingTask(contextID); tid != "" {
						req.Message.TaskID = tid
					}
				}
				return nil, nil
			},
		},
		Converter: func(ctx agent.InvocationContext, _ *a2a.SendMessageRequest, a2aEvent a2a.Event, a2aErr error) (*session.Event, error) {
			if a2aErr != nil {
				return nil, a2aErr
			}
			sa.trackTaskState(invocationContextID(ctx), a2aEvent)
			return adka2a.ToSessionEvent(ctx, a2aEvent)
		},
	}
	if tok := managed.token(); tok != "" {
		cfg.Auth = auth.StaticToken(tok)
	}

	remote, err := remoteagent.NewA2A(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating remote A2A agent: %w", err)
	}
	sa.remote = remote
	return sa.remote, nil
}

func (sa *SandboxAgent) trackTaskState(contextID string, ev a2a.Event) {
	if ev == nil || contextID == "" {
		return
	}
	var state a2a.TaskState
	var taskID a2a.TaskID
	switch v := ev.(type) {
	case *a2a.TaskStatusUpdateEvent:
		state = v.Status.State
		taskID = v.TaskID
	case *a2a.Task:
		state = v.Status.State
		taskID = v.ID
	default:
		return
	}
	sa.mu.Lock()
	defer sa.mu.Unlock()
	if state == a2a.TaskStateInputRequired && taskID != "" {
		sa.pendingTasks[contextID] = taskID
	} else if state.Terminal() {
		delete(sa.pendingTasks, contextID)
	}
}

func (sa *SandboxAgent) pendingTask(contextID string) a2a.TaskID {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	return sa.pendingTasks[contextID]
}

func (sa *SandboxAgent) run(ic agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		remote, err := sa.ensureRemote()
		if err != nil {
			yield(nil, err)
			return
		}
		contextID := invocationContextID(ic)
		sa.backend.RegisterInvocation(contextID, ic, sa)
		defer func() {
			if ws := sa.backend.Workspace(); ws != nil {
				if _, err := ws.SyncToArtifacts(ic); err != nil {
					slog.Warn("syncing workspace files to artifacts", "context", contextID, "error", err)
				}
			}
			sa.backend.UnregisterInvocation(contextID)
		}()

		for ev, err := range remote.Run(ic) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

func callbackContextID(ctx agent.Context) string {
	if id := ctx.SessionID(); id != "" {
		return id
	}
	return ctx.InvocationID()
}

func invocationContextID(ic agent.InvocationContext) string {
	if ic.Session() != nil && ic.Session().ID() != "" {
		return ic.Session().ID()
	}
	return ic.InvocationID()
}
