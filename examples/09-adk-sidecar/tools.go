package adksidecar

import (
	"slices"
	"strings"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// ConfirmableTool is implemented by tools that advertise whether they require
// explicit user approval before execution.
type ConfirmableTool interface {
	tool.Tool
	RequiresConfirmation() bool
}

type runnableTool interface {
	tool.Tool
	Run(ctx agent.Context, args any) (map[string]any, error)
}

type declarableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
}

type confirmableWrapper struct {
	tool.Tool
	requireConfirmation bool
}

func (c confirmableWrapper) RequiresConfirmation() bool {
	return c.requireConfirmation
}

func (c confirmableWrapper) Declaration() *genai.FunctionDeclaration {
	if d, ok := c.Tool.(declarableTool); ok {
		return d.Declaration()
	}
	return nil
}

func (c confirmableWrapper) Run(ctx agent.Context, args any) (map[string]any, error) {
	if r, ok := c.Tool.(runnableTool); ok {
		return r.Run(ctx, args)
	}
	return nil, nil
}

// NewFunctionTool wraps functiontool.New so that cfg.RequireConfirmation is
// also visible to the sidecar hooks/beforeTool handler via ConfirmableTool.
func NewFunctionTool[TArgs, TResults any](cfg functiontool.Config, handler functiontool.Func[TArgs, TResults]) (tool.Tool, error) {
	t, err := functiontool.New(cfg, handler)
	if err != nil {
		return nil, err
	}
	if !cfg.RequireConfirmation {
		return t, nil
	}
	return confirmableWrapper{Tool: t, requireConfirmation: true}, nil
}

// DeploymentPolicy describes a service's deployment requirements.
type DeploymentPolicy struct {
	RequireCanary bool   `json:"require_canary"`
	MinReviewers  int    `json:"min_reviewers"`
	FreezeWindow  string `json:"freeze_window"`
}

// DeploymentRecord records an executed deployment.
type DeploymentRecord struct {
	Service string `json:"service"`
	Version string `json:"version"`
}

// DeploymentStore holds the demo services' deployment policies and history.
type DeploymentStore struct {
	policies map[string]DeploymentPolicy

	mu      sync.Mutex
	history []DeploymentRecord
}

// NewDeploymentStore returns a store pre-populated with the demo policies.
func NewDeploymentStore() *DeploymentStore {
	return &DeploymentStore{
		policies: map[string]DeploymentPolicy{
			"payments": {
				RequireCanary: true,
				MinReviewers:  2,
				FreezeWindow:  "Friday 17:00 UTC - Monday 09:00 UTC",
			},
			"search": {
				RequireCanary: true,
				MinReviewers:  1,
				FreezeWindow:  "none",
			},
		},
	}
}

// History returns a snapshot of recorded deployments.
func (s *DeploymentStore) History() []DeploymentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.history)
}

// ClearHistory removes all recorded deployments.
func (s *DeploymentStore) ClearHistory() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = nil
}

type echoArgs struct {
	Text string `json:"text" jsonschema:"Text to echo back from the ADK host process."`
}

type policyArgs struct {
	Service string `json:"service" jsonschema:"Internal service name to look up."`
}

type policyResult struct {
	Service       string   `json:"service"`
	Found         bool     `json:"found"`
	RequireCanary bool     `json:"require_canary,omitempty"`
	MinReviewers  int      `json:"min_reviewers,omitempty"`
	FreezeWindow  string   `json:"freeze_window,omitempty"`
	KnownServices []string `json:"known_services,omitempty"`
}

type deployArgs struct {
	Service string `json:"service" jsonschema:"Service name to deploy."`
	Version string `json:"version" jsonschema:"Version string to deploy."`
}

type deployResult struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

// DefaultADKTools returns the demo ADK tools bridged into a2a-serve-sidecar:
// echo, get_deployment_policy, and deploy_service (with RequireConfirmation).
func DefaultADKTools(store *DeploymentStore) ([]tool.Tool, error) {
	if store == nil {
		store = NewDeploymentStore()
	}

	echoTool, err := NewFunctionTool(functiontool.Config{
		Name:        "echo",
		Description: "Echo text back from the ADK host process.",
	}, func(_ agent.Context, args echoArgs) (string, error) {
		return "host echo: " + args.Text, nil
	})
	if err != nil {
		return nil, err
	}

	policyTool, err := NewFunctionTool(functiontool.Config{
		Name:        "get_deployment_policy",
		Description: "Look up deployment policy for an internal service.",
	}, func(_ agent.Context, args policyArgs) (policyResult, error) {
		key := strings.ToLower(strings.TrimSpace(args.Service))
		p, ok := store.policies[key]
		if !ok {
			known := make([]string, 0, len(store.policies))
			for k := range store.policies {
				known = append(known, k)
			}
			slices.Sort(known)
			return policyResult{
				Service:       key,
				Found:         false,
				KnownServices: known,
			}, nil
		}
		return policyResult{
			Service:       key,
			Found:         true,
			RequireCanary: p.RequireCanary,
			MinReviewers:  p.MinReviewers,
			FreezeWindow:  p.FreezeWindow,
		}, nil
	})
	if err != nil {
		return nil, err
	}

	deployTool, err := NewFunctionTool(functiontool.Config{
		Name:                "deploy_service",
		Description:         "Deploy a service version to production (requires user approval).",
		RequireConfirmation: true,
	}, func(_ agent.Context, args deployArgs) (deployResult, error) {
		rec := DeploymentRecord{
			Service: strings.TrimSpace(args.Service),
			Version: strings.TrimSpace(args.Version),
		}
		store.mu.Lock()
		store.history = append(store.history, rec)
		store.mu.Unlock()
		return deployResult{
			Status:  "deployed",
			Service: rec.Service,
			Version: rec.Version,
		}, nil
	})
	if err != nil {
		return nil, err
	}

	return []tool.Tool{echoTool, policyTool, deployTool}, nil
}
