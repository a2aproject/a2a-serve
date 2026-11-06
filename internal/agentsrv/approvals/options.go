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

package approvals

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext/structuredinput"
	"github.com/a2aproject/a2a-go/v2/integrations/executor/a2aacp"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/a2aproject/a2a-serve/internal/toolrules"
)

// Metadata keys of a permission question whose options the server wrote.
const (
	// agentOptionsKey maps the server's options to the agent's.
	agentOptionsKey = "a2a-serve/agent-options"
	// ruleKey is the rule that the always option allows.
	ruleKey = "a2a-serve/rule"
)

// Labels of the server's options.
const (
	LabelOnce   = "Allow once"
	LabelReject = "Deny"
)

// AlwaysLabel is the label of the option that allows rule for the rest of
// the conversation.
func AlwaysLabel(rule string) string {
	return "Always allow " + RuleText(rule)
}

// RuleText shows a rule to people: an MCP tool or a command without its
// prefix, or a tool kind.
func RuleText(rule string) string {
	if s, ok := strings.CutPrefix(rule, toolrules.PrefixTool); ok {
		return s
	}
	return strings.TrimPrefix(rule, toolrules.PrefixCommand)
}

// WithServerOptions returns msg, a permission question of the agent, with
// the server's options: once and reject, which choose the agent's options
// that allow the call once and reject it, and always, which allows rule
// for the rest of the conversation when rule isn't empty. The agent's
// other options are dropped. A form, a question that offers no way to
// allow the call, and a question rewritten before are returned unchanged.
func WithServerOptions(msg *a2a.Message, rule string) (*a2a.Message, error) {
	if msg == nil || msg.Metadata[agentOptionsKey] != nil {
		return msg, nil
	}
	req, err := structuredinput.ParseRequest(msg)
	if err != nil || req == nil || req.Schema == nil {
		return msg, err
	}
	optionSchema := req.Schema.Properties["option"]
	if optionSchema == nil || req.Schema.Properties["content"] != nil {
		return msg, nil
	}
	var offered []string
	for _, o := range optionSchema.OneOf {
		if o.Const == nil {
			continue
		}
		if id, ok := (*o.Const).(string); ok {
			offered = append(offered, id)
		}
	}
	once := firstOf(offered, a2aacp.OptionOnce, a2aacp.OptionAlways)
	if once == "" {
		return msg, nil
	}
	agentOptions := map[string]any{a2aacp.OptionOnce: once}
	options := []Option{{ID: a2aacp.OptionOnce, Label: LabelOnce}}
	if rule != "" {
		agentOptions[a2aacp.OptionAlways] = once
		options = append(options, Option{ID: a2aacp.OptionAlways, Label: AlwaysLabel(rule)})
	}
	if reject := firstOf(offered, a2aacp.OptionReject, a2aacp.OptionRejectAlways); reject != "" {
		agentOptions[a2aacp.OptionReject] = reject
		options = append(options, Option{ID: a2aacp.OptionReject, Label: LabelReject})
	}

	schema := *req.Schema
	schema.Properties = maps.Clone(req.Schema.Properties)
	opt := &jsonschema.Schema{Type: "string"}
	ids := make([]string, len(options))
	for i, o := range options {
		ids[i] = o.ID
		if o.ID == a2aacp.OptionAlways {
			ids[i] = alwaysReply(o.ID, rule)
		}
		opt.OneOf = append(opt.OneOf, &jsonschema.Schema{Const: jsonschema.Ptr[any](o.ID), Title: o.Label})
	}
	schema.Properties["option"] = opt
	schemaPart, err := structuredinput.NewSchemaPart(req.ID, &schema)
	if err != nil {
		return nil, err
	}

	out := *msg
	out.Parts = nil
	for _, p := range msg.Parts {
		if meta, ok := structuredinput.GetPartMetadata(p); ok && meta.Type == structuredinput.PartTypeInputSchema {
			out.Parts = append(out.Parts, schemaPart)
			continue
		}
		if t := p.Text(); t != "" {
			c := *p
			c.Content = a2a.Text(withReplies(t, ids))
			out.Parts = append(out.Parts, &c)
			continue
		}
		out.Parts = append(out.Parts, p)
	}
	out.Metadata = maps.Clone(msg.Metadata)
	if out.Metadata == nil {
		out.Metadata = map[string]any{}
	}
	out.Metadata[agentOptionsKey] = agentOptions
	if rule != "" {
		out.Metadata[ruleKey] = rule
	}
	return &out, nil
}

// alwaysReply is the reply that chooses the always option, with the rule
// it allows, for people who answer without buttons.
func alwaysReply(reply, rule string) string {
	return fmt.Sprintf("%s (allows %s in this conversation)", reply, RuleText(rule))
}

// withReplies replaces the line of text that lists the replies.
func withReplies(text string, ids []string) string {
	const prefix = "Reply with one of: "
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			lines[i] = fmt.Sprintf("%s%s.", prefix, strings.Join(ids, ", "))
		}
	}
	return strings.Join(lines, "\n")
}

// AgentOption returns the option of the agent that the server's option id
// chooses, or false if the server didn't write the question's options.
func (q *Question) AgentOption(id string) (string, bool) {
	if q.AgentOptions == nil {
		return "", false
	}
	v, ok := q.AgentOptions[id]
	return v, ok
}

func firstOf(offered []string, ids ...string) string {
	for _, id := range ids {
		if slices.Contains(offered, id) {
			return id
		}
	}
	return ""
}
