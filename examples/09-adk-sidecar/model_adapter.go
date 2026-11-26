package adksidecar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

var geminiModelPathRE = regexp.MustCompile(`^(v1(?:beta)?/models/)([^/:]+)(:.*)$`)

// geminiConfigFields are the fields of a Gemini request body that
// genai.GenerateContentConfig holds alongside generationConfig fields.
var geminiConfigFields = []string{
	"systemInstruction",
	"tools",
	"toolConfig",
	"safetySettings",
	"cachedContent",
	"labels",
}

// Rendered is an HTTP model response produced in place of the upstream provider.
type Rendered struct {
	Body        []byte
	ContentType string
}

type requestSnapshot struct {
	model    string
	contents []byte
	config   []byte
}

// ModelAdapter translates a provider's HTTP API to and from ADK's
// model.LLMRequest and model.LLMResponse.
type ModelAdapter interface {
	StreamMode(path string, query url.Values, body []byte) string
	ParseRequest(path string, body []byte) (*model.LLMRequest, error)
	Snapshot(req *model.LLMRequest) (requestSnapshot, error)
	ApplyRequest(path string, body []byte, before requestSnapshot, req *model.LLMRequest) (string, []byte, error)
	ParseResponse(body []byte, mode string) (*model.LLMResponse, error)
	RenderResponse(resp *model.LLMResponse, mode string) (Rendered, error)
}

func snapshotLLMRequest(req *model.LLMRequest) (requestSnapshot, error) {
	contents, err := json.Marshal(req.Contents)
	if err != nil {
		return requestSnapshot{}, err
	}
	cfgObj := req.Config
	if cfgObj == nil {
		cfgObj = &genai.GenerateContentConfig{}
	}
	cfg, err := json.Marshal(cfgObj)
	if err != nil {
		return requestSnapshot{}, err
	}
	return requestSnapshot{
		model:    req.Model,
		contents: contents,
		config:   cfg,
	}, nil
}

// GeminiAdapter translates Gemini API HTTP requests and responses to and from
// ADK's model.LLMRequest and model.LLMResponse.
type GeminiAdapter struct{}

// StreamMode returns how the Gemini API answers this request: "sse", "array", or "json".
func (GeminiAdapter) StreamMode(path string, query url.Values, _ []byte) string {
	if query.Get("alt") == "sse" {
		return "sse"
	}
	if strings.Contains(path, "streamGenerateContent") {
		return "array"
	}
	return "json"
}

// ParseRequest parses a Gemini generateContent/streamGenerateContent HTTP body
// into an ADK model.LLMRequest.
func (GeminiAdapter) ParseRequest(path string, body []byte) (*model.LLMRequest, error) {
	modelName := ""
	if m := geminiModelPathRE.FindStringSubmatch(strings.TrimPrefix(path, "/")); m != nil {
		modelName = m[2]
	}
	raw, err := parseJSONObject(body)
	if err != nil {
		return nil, err
	}

	var contents []*genai.Content
	if rawContents, ok := raw["contents"]; ok && len(rawContents) > 0 && string(rawContents) != "null" {
		if err := json.Unmarshal(rawContents, &contents); err != nil {
			return nil, fmt.Errorf("parsing contents: %w", err)
		}
	}

	configMap := make(map[string]json.RawMessage)
	if rawGenCfg, ok := raw["generationConfig"]; ok && len(rawGenCfg) > 0 && string(rawGenCfg) != "null" {
		if err := json.Unmarshal(rawGenCfg, &configMap); err != nil {
			return nil, fmt.Errorf("parsing generationConfig: %w", err)
		}
	}
	for _, key := range geminiConfigFields {
		if v, ok := raw[key]; ok && len(v) > 0 && string(v) != "null" {
			configMap[key] = v
		}
	}

	cfg := &genai.GenerateContentConfig{}
	if len(configMap) > 0 {
		cfgBytes, err := json.Marshal(configMap)
		if err != nil {
			return nil, fmt.Errorf("encoding config: %w", err)
		}
		if err := json.Unmarshal(cfgBytes, cfg); err != nil {
			return nil, fmt.Errorf("parsing config: %w", err)
		}
	}

	return &model.LLMRequest{
		Model:    modelName,
		Contents: contents,
		Config:   cfg,
	}, nil
}

// Snapshot captures the mutable fields of req so ApplyRequest can tell whether
// callbacks changed anything.
func (GeminiAdapter) Snapshot(req *model.LLMRequest) (requestSnapshot, error) {
	return snapshotLLMRequest(req)
}

// ApplyRequest writes callback mutations from req back onto the Gemini path and
// raw HTTP body. If callbacks changed neither contents nor config, the original
// body is returned unchanged.
func (a GeminiAdapter) ApplyRequest(path string, body []byte, before requestSnapshot, req *model.LLMRequest) (string, []byte, error) {
	after, err := a.Snapshot(req)
	if err != nil {
		return "", nil, err
	}
	if after.model != before.model && after.model != "" {
		if m := geminiModelPathRE.FindStringSubmatch(strings.TrimPrefix(path, "/")); m != nil {
			path = m[1] + strings.TrimPrefix(after.model, "models/") + m[3]
		}
	}
	if bytes.Equal(after.contents, before.contents) && bytes.Equal(after.config, before.config) {
		return path, body, nil
	}

	raw, err := parseJSONObject(body)
	if err != nil {
		return "", nil, err
	}
	if !bytes.Equal(after.contents, before.contents) {
		raw["contents"] = after.contents
	}
	if !bytes.Equal(after.config, before.config) {
		cfgMap := make(map[string]json.RawMessage)
		if err := json.Unmarshal(after.config, &cfgMap); err != nil {
			return "", nil, err
		}
		delete(cfgMap, "httpOptions")
		for _, key := range geminiConfigFields {
			delete(raw, key)
			if v, ok := cfgMap[key]; ok {
				raw[key] = v
				delete(cfgMap, key)
			}
		}
		if len(cfgMap) > 0 {
			genCfgBytes, err := json.Marshal(cfgMap)
			if err != nil {
				return "", nil, err
			}
			raw["generationConfig"] = genCfgBytes
		} else {
			delete(raw, "generationConfig")
		}
	}

	out, err := json.Marshal(raw)
	if err != nil {
		return "", nil, err
	}
	return path, out, nil
}

// ParseResponse merges a Gemini JSON, JSON-array, or SSE response into a single
// ADK model.LLMResponse.
func (GeminiAdapter) ParseResponse(body []byte, mode string) (*model.LLMResponse, error) {
	var chunks []json.RawMessage
	switch mode {
	case "sse":
		for line := range strings.SplitSeq(string(body), "\n") {
			line = strings.TrimRight(line, "\r")
			data, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			if data == "" || data == "[DONE]" {
				continue
			}
			if !json.Valid([]byte(data)) {
				return nil, fmt.Errorf("invalid SSE JSON chunk")
			}
			chunks = append(chunks, json.RawMessage(data))
		}
	default:
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 {
			if trimmed[0] == '[' {
				if err := json.Unmarshal(trimmed, &chunks); err != nil {
					return nil, fmt.Errorf("parsing response array: %w", err)
				}
			} else {
				if !json.Valid(trimmed) {
					return nil, fmt.Errorf("invalid JSON response")
				}
				chunks = []json.RawMessage{trimmed}
			}
		}
	}

	var parts []*genai.Part
	var finishReason genai.FinishReason
	var usage *genai.GenerateContentResponseUsageMetadata
	var modelVersion string

	for _, chunk := range chunks {
		var resp genai.GenerateContentResponse
		if err := json.Unmarshal(chunk, &resp); err != nil {
			return nil, fmt.Errorf("parsing GenerateContentResponse chunk: %w", err)
		}
		for _, cand := range resp.Candidates {
			if cand == nil {
				continue
			}
			if cand.Content != nil {
				parts = append(parts, cand.Content.Parts...)
			}
			if cand.FinishReason != "" {
				finishReason = cand.FinishReason
			}
		}
		if resp.UsageMetadata != nil {
			usage = resp.UsageMetadata
		}
		if resp.ModelVersion != "" {
			modelVersion = resp.ModelVersion
		}
	}

	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: mergeTextParts(parts),
		},
		FinishReason:  finishReason,
		UsageMetadata: usage,
		ModelVersion:  modelVersion,
	}, nil
}

// RenderResponse encodes an ADK model.LLMResponse in the Gemini API format
// matching mode ("sse", "array", or "json").
func (GeminiAdapter) RenderResponse(resp *model.LLMResponse, mode string) (Rendered, error) {
	content := &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{{Text: ""}},
	}
	if resp != nil && resp.Content != nil {
		copied := *resp.Content
		copied.Role = genai.RoleModel
		if len(copied.Parts) == 0 {
			copied.Parts = []*genai.Part{{Text: ""}}
		}
		content = &copied
	}
	finish := genai.FinishReasonStop
	if resp != nil && resp.FinishReason != "" {
		finish = resp.FinishReason
	}
	payload := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{
				Content:      content,
				FinishReason: finish,
				Index:        0,
			},
		},
	}
	if resp != nil && resp.UsageMetadata != nil {
		payload.UsageMetadata = resp.UsageMetadata
	}

	switch mode {
	case "sse":
		data, err := json.Marshal(payload)
		if err != nil {
			return Rendered{}, err
		}
		return Rendered{
			Body:        append(append([]byte("data: "), data...), "\r\n\r\n"...),
			ContentType: "text/event-stream",
		}, nil
	case "array":
		data, err := json.Marshal([]*genai.GenerateContentResponse{payload})
		if err != nil {
			return Rendered{}, err
		}
		return Rendered{
			Body:        data,
			ContentType: "application/json",
		}, nil
	default:
		data, err := json.Marshal(payload)
		if err != nil {
			return Rendered{}, err
		}
		return Rendered{
			Body:        data,
			ContentType: "application/json",
		}, nil
	}
}

// AnthropicAdapter translates Anthropic Messages API (/v1/messages) HTTP
// requests and responses to and from ADK's model.LLMRequest and model.LLMResponse.
type AnthropicAdapter struct{}

// StreamMode returns "sse" when the request JSON sets "stream": true, else "json".
func (AnthropicAdapter) StreamMode(_ string, _ url.Values, body []byte) string {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Stream {
		return "sse"
	}
	return "json"
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	Thinking  string         `json:"thinking,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   any            `json:"content,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int32 `json:"input_tokens"`
	OutputTokens int32 `json:"output_tokens"`
}

// ParseRequest parses an Anthropic Messages API body into an ADK model.LLMRequest.
func (AnthropicAdapter) ParseRequest(_ string, body []byte) (*model.LLMRequest, error) {
	raw, err := parseJSONObject(body)
	if err != nil {
		return nil, err
	}

	var modelName string
	if v, ok := raw["model"]; ok {
		if err := json.Unmarshal(v, &modelName); err != nil {
			return nil, fmt.Errorf("parsing model: %w", err)
		}
	}

	cfg := &genai.GenerateContentConfig{}
	if v, ok := raw["system"]; ok && len(v) > 0 && string(v) != "null" {
		parts, err := parseAnthropicContent(v)
		if err != nil {
			return nil, fmt.Errorf("parsing system: %w", err)
		}
		cfg.SystemInstruction = &genai.Content{Parts: parts}
	}
	if v, ok := raw["max_tokens"]; ok {
		var maxTok int32
		if err := json.Unmarshal(v, &maxTok); err != nil {
			return nil, fmt.Errorf("parsing max_tokens: %w", err)
		}
		cfg.MaxOutputTokens = maxTok
	}
	if v, ok := raw["temperature"]; ok {
		var temp float32
		if err := json.Unmarshal(v, &temp); err != nil {
			return nil, fmt.Errorf("parsing temperature: %w", err)
		}
		cfg.Temperature = &temp
	}
	if v, ok := raw["top_p"]; ok {
		var topP float32
		if err := json.Unmarshal(v, &topP); err != nil {
			return nil, fmt.Errorf("parsing top_p: %w", err)
		}
		cfg.TopP = &topP
	}
	if v, ok := raw["top_k"]; ok {
		var topK float32
		if err := json.Unmarshal(v, &topK); err != nil {
			return nil, fmt.Errorf("parsing top_k: %w", err)
		}
		cfg.TopK = &topK
	}
	if v, ok := raw["stop_sequences"]; ok {
		var stops []string
		if err := json.Unmarshal(v, &stops); err != nil {
			return nil, fmt.Errorf("parsing stop_sequences: %w", err)
		}
		cfg.StopSequences = stops
	}

	var contents []*genai.Content
	if rawMsgs, ok := raw["messages"]; ok && len(rawMsgs) > 0 && string(rawMsgs) != "null" {
		var msgs []anthropicMessage
		if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
			return nil, fmt.Errorf("parsing messages: %w", err)
		}
		for _, m := range msgs {
			parts, err := parseAnthropicContent(m.Content)
			if err != nil {
				return nil, fmt.Errorf("parsing message content: %w", err)
			}
			role := genai.RoleUser
			if m.Role == "assistant" {
				role = genai.RoleModel
			}
			contents = append(contents, &genai.Content{Role: role, Parts: parts})
		}
	}

	return &model.LLMRequest{
		Model:    modelName,
		Contents: contents,
		Config:   cfg,
	}, nil
}

// Snapshot captures mutable request state so unchanged Anthropic requests stay verbatim.
func (AnthropicAdapter) Snapshot(req *model.LLMRequest) (requestSnapshot, error) {
	return snapshotLLMRequest(req)
}

// ApplyRequest writes callback mutations back onto the Anthropic Messages JSON body.
func (a AnthropicAdapter) ApplyRequest(path string, body []byte, before requestSnapshot, req *model.LLMRequest) (string, []byte, error) {
	after, err := a.Snapshot(req)
	if err != nil {
		return "", nil, err
	}
	modelChanged := after.model != before.model && after.model != ""
	contentsChanged := !bytes.Equal(after.contents, before.contents)
	configChanged := !bytes.Equal(after.config, before.config)
	if !modelChanged && !contentsChanged && !configChanged {
		return path, body, nil
	}

	raw, err := parseJSONObject(body)
	if err != nil {
		return "", nil, err
	}
	if modelChanged {
		modelBytes, err := json.Marshal(after.model)
		if err != nil {
			return "", nil, err
		}
		raw["model"] = modelBytes
	}
	if contentsChanged {
		msgs := make([]map[string]any, 0, len(req.Contents))
		for _, c := range req.Contents {
			if c == nil {
				continue
			}
			role := "user"
			if c.Role == genai.RoleModel {
				role = "assistant"
			}
			msgs = append(msgs, map[string]any{
				"role":    role,
				"content": encodeAnthropicParts(c.Parts),
			})
		}
		msgBytes, err := json.Marshal(msgs)
		if err != nil {
			return "", nil, err
		}
		raw["messages"] = msgBytes
	}
	if configChanged && req.Config != nil {
		if req.Config.SystemInstruction != nil {
			sysBytes, err := json.Marshal(encodeAnthropicParts(req.Config.SystemInstruction.Parts))
			if err != nil {
				return "", nil, err
			}
			raw["system"] = sysBytes
		} else {
			delete(raw, "system")
		}
		if req.Config.MaxOutputTokens > 0 {
			b, err := json.Marshal(req.Config.MaxOutputTokens)
			if err != nil {
				return "", nil, err
			}
			raw["max_tokens"] = b
		}
		if req.Config.Temperature != nil {
			b, err := json.Marshal(*req.Config.Temperature)
			if err != nil {
				return "", nil, err
			}
			raw["temperature"] = b
		}
		if req.Config.TopP != nil {
			b, err := json.Marshal(*req.Config.TopP)
			if err != nil {
				return "", nil, err
			}
			raw["top_p"] = b
		}
		if req.Config.TopK != nil {
			b, err := json.Marshal(int(*req.Config.TopK))
			if err != nil {
				return "", nil, err
			}
			raw["top_k"] = b
		}
		if len(req.Config.StopSequences) > 0 {
			b, err := json.Marshal(req.Config.StopSequences)
			if err != nil {
				return "", nil, err
			}
			raw["stop_sequences"] = b
		}
	}

	out, err := json.Marshal(raw)
	if err != nil {
		return "", nil, err
	}
	return path, out, nil
}

// ParseResponse merges an Anthropic JSON or SSE response into an ADK model.LLMResponse.
func (AnthropicAdapter) ParseResponse(body []byte, mode string) (*model.LLMResponse, error) {
	if mode == "sse" {
		return parseAnthropicSSE(body)
	}
	var msg struct {
		Model      string           `json:"model"`
		Content    []anthropicBlock `json:"content"`
		StopReason string           `json:"stop_reason"`
		Usage      *anthropicUsage  `json:"usage"`
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &msg); err != nil {
			return nil, fmt.Errorf("parsing Anthropic response: %w", err)
		}
	}
	parts := decodeAnthropicBlocks(msg.Content)
	return &model.LLMResponse{
		Content:       &genai.Content{Role: genai.RoleModel, Parts: mergeTextParts(parts)},
		FinishReason:  mapAnthropicStopReason(msg.StopReason),
		UsageMetadata: mapAnthropicUsage(msg.Usage),
		ModelVersion:  msg.Model,
	}, nil
}

// RenderResponse encodes an ADK model.LLMResponse as an Anthropic Messages JSON
// or SSE response.
func (AnthropicAdapter) RenderResponse(resp *model.LLMResponse, mode string) (Rendered, error) {
	var parts []*genai.Part
	if resp != nil && resp.Content != nil {
		parts = resp.Content.Parts
	}
	blocks := encodeAnthropicParts(parts)
	if len(blocks) == 0 {
		blocks = []map[string]any{{"type": "text", "text": ""}}
	}
	modelName := "claude"
	if resp != nil && resp.ModelVersion != "" {
		modelName = resp.ModelVersion
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if resp != nil && resp.UsageMetadata != nil {
		usage["input_tokens"] = resp.UsageMetadata.PromptTokenCount
		usage["output_tokens"] = resp.UsageMetadata.CandidatesTokenCount
	}
	stopReason := "end_turn"
	if resp != nil && resp.FinishReason == genai.FinishReasonMaxTokens {
		stopReason = "max_tokens"
	}

	if mode != "sse" {
		payload := map[string]any{
			"id":            "msg_adk",
			"type":          "message",
			"role":          "assistant",
			"model":         modelName,
			"content":       blocks,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage":         usage,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return Rendered{}, err
		}
		return Rendered{Body: data, ContentType: "application/json"}, nil
	}

	var buf bytes.Buffer
	writeEvent := func(event string, data any) error {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		fmt.Fprintf(&buf, "event: %s\ndata: %s\n\n", event, b)
		return nil
	}
	if err := writeEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            "msg_adk",
			"type":          "message",
			"role":          "assistant",
			"model":         modelName,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         usage,
		},
	}); err != nil {
		return Rendered{}, err
	}
	for i, blk := range blocks {
		if blk["type"] == "text" {
			text, _ := blk["text"].(string)
			if err := writeEvent("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         i,
				"content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return Rendered{}, err
			}
			if text != "" {
				if err := writeEvent("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": i,
					"delta": map[string]any{"type": "text_delta", "text": text},
				}); err != nil {
					return Rendered{}, err
				}
			}
			if err := writeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": i,
			}); err != nil {
				return Rendered{}, err
			}
			continue
		}
		if err := writeEvent("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         i,
			"content_block": blk,
		}); err != nil {
			return Rendered{}, err
		}
		if err := writeEvent("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": i,
		}); err != nil {
			return Rendered{}, err
		}
	}
	if err := writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usage,
	}); err != nil {
		return Rendered{}, err
	}
	if err := writeEvent("message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return Rendered{}, err
	}
	return Rendered{Body: buf.Bytes(), ContentType: "text/event-stream"}, nil
}

func parseAnthropicSSE(body []byte) (*model.LLMResponse, error) {
	var parts []*genai.Part
	var stopReason string
	var modelVersion string
	var usage anthropicUsage
	var hasUsage bool

	type activeTool struct {
		id    string
		name  string
		input strings.Builder
	}
	toolsByIndex := map[int]*activeTool{}

	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Model string          `json:"model"`
				Usage *anthropicUsage `json:"usage"`
			} `json:"message"`
			ContentBlock *anthropicBlock `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage *anthropicUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil, fmt.Errorf("parsing Anthropic SSE event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message.Model != "" {
				modelVersion = ev.Message.Model
			}
			if ev.Message.Usage != nil {
				usage.InputTokens = ev.Message.Usage.InputTokens
				usage.OutputTokens = ev.Message.Usage.OutputTokens
				hasUsage = true
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				continue
			}
			switch ev.ContentBlock.Type {
			case "text":
				if ev.ContentBlock.Text != "" {
					parts = append(parts, &genai.Part{Text: ev.ContentBlock.Text})
				}
			case "thinking":
				if ev.ContentBlock.Thinking != "" {
					parts = append(parts, &genai.Part{Text: ev.ContentBlock.Thinking, Thought: true})
				}
			case "tool_use":
				toolsByIndex[ev.Index] = &activeTool{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				parts = append(parts, &genai.Part{Text: ev.Delta.Text})
			case "thinking_delta":
				parts = append(parts, &genai.Part{Text: ev.Delta.Thinking, Thought: true})
			case "input_json_delta":
				if at, ok := toolsByIndex[ev.Index]; ok {
					at.input.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if at, ok := toolsByIndex[ev.Index]; ok {
				args := map[string]any{}
				if raw := strings.TrimSpace(at.input.String()); raw != "" {
					if err := json.Unmarshal([]byte(raw), &args); err != nil {
						return nil, fmt.Errorf("parsing Anthropic tool_use input JSON: %w", err)
					}
				}
				parts = append(parts, &genai.Part{
					FunctionCall: &genai.FunctionCall{ID: at.id, Name: at.name, Args: args},
				})
				delete(toolsByIndex, ev.Index)
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				if ev.Usage.InputTokens > 0 {
					usage.InputTokens = ev.Usage.InputTokens
				}
				if ev.Usage.OutputTokens > 0 {
					usage.OutputTokens = ev.Usage.OutputTokens
				}
				hasUsage = true
			}
		}
	}

	var usagePtr *anthropicUsage
	if hasUsage {
		usagePtr = &usage
	}
	return &model.LLMResponse{
		Content:       &genai.Content{Role: genai.RoleModel, Parts: mergeTextParts(parts)},
		FinishReason:  mapAnthropicStopReason(stopReason),
		UsageMetadata: mapAnthropicUsage(usagePtr),
		ModelVersion:  modelVersion,
	}, nil
}

func parseAnthropicContent(raw json.RawMessage) ([]*genai.Part, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, err
		}
		return []*genai.Part{{Text: text}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, err
	}
	return decodeAnthropicBlocks(blocks), nil
}

func decodeAnthropicBlocks(blocks []anthropicBlock) []*genai.Part {
	parts := make([]*genai.Part, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, &genai.Part{Text: b.Text})
		case "thinking":
			parts = append(parts, &genai.Part{Text: b.Thinking, Thought: true})
		case "tool_use":
			parts = append(parts, &genai.Part{
				FunctionCall: &genai.FunctionCall{ID: b.ID, Name: b.Name, Args: b.Input},
			})
		case "tool_result":
			resp := map[string]any{"content": b.Content}
			parts = append(parts, &genai.Part{
				FunctionResponse: &genai.FunctionResponse{ID: b.ToolUseID, Name: b.ToolUseID, Response: resp},
			})
		}
	}
	return parts
}

func encodeAnthropicParts(parts []*genai.Part) []map[string]any {
	blocks := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if p == nil {
			continue
		}
		switch {
		case p.FunctionCall != nil:
			input := p.FunctionCall.Args
			if input == nil {
				input = map[string]any{}
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    p.FunctionCall.ID,
				"name":  p.FunctionCall.Name,
				"input": input,
			})
		case p.FunctionResponse != nil:
			content := any(p.FunctionResponse.Response)
			if v, ok := p.FunctionResponse.Response["content"]; ok {
				content = v
			}
			blocks = append(blocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": p.FunctionResponse.ID,
				"content":     content,
			})
		case p.Thought:
			blocks = append(blocks, map[string]any{
				"type":     "thinking",
				"thinking": p.Text,
			})
		default:
			blocks = append(blocks, map[string]any{
				"type": "text",
				"text": p.Text,
			})
		}
	}
	return blocks
}

func mapAnthropicStopReason(reason string) genai.FinishReason {
	switch reason {
	case "end_turn", "stop_sequence", "tool_use":
		return genai.FinishReasonStop
	case "max_tokens":
		return genai.FinishReasonMaxTokens
	default:
		return ""
	}
}

func mapAnthropicUsage(u *anthropicUsage) *genai.GenerateContentResponseUsageMetadata {
	if u == nil {
		return nil
	}
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     u.InputTokens,
		CandidatesTokenCount: u.OutputTokens,
		TotalTokenCount:      u.InputTokens + u.OutputTokens,
	}
}

func mergeTextParts(parts []*genai.Part) []*genai.Part {
	out := make([]*genai.Part, 0, len(parts))
	for _, p := range parts {
		if p == nil {
			continue
		}
		if len(out) > 0 && isPlainTextPart(p) && isPlainTextPart(out[len(out)-1]) && p.Thought == out[len(out)-1].Thought {
			merged := *out[len(out)-1]
			merged.Text += p.Text
			out[len(out)-1] = &merged
			continue
		}
		out = append(out, p)
	}
	return out
}

func isPlainTextPart(p *genai.Part) bool {
	return p.FunctionCall == nil && p.FunctionResponse == nil && p.InlineData == nil && p.FileData == nil && p.ExecutableCode == nil && p.CodeExecutionResult == nil
}

func parseJSONObject(body []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	if trimmed[0] != '{' {
		return nil, errors.New("expected JSON object body")
	}
	out := make(map[string]json.RawMessage)
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, err
	}
	return out, nil
}
