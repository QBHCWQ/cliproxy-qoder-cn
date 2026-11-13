package main

import (
	"bytes"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	qoderMaxOutputTokens = 32768
	sseDataPrefix        = "data:"
	sseDoneFrame         = "data: [DONE]\n\n"
)

type qoderMessageMeta struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type qoderModelConfig struct {
	Key             string `json:"key"`
	IsReasoning     bool   `json:"is_reasoning"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Source          string `json:"source"`
}

type qoderRequest struct {
	RequestID      string            `json:"request_id"`
	RequestSetID   string            `json:"request_set_id"`
	ChatRecordID   string            `json:"chat_record_id"`
	SessionID      string            `json:"session_id"`
	Stream         bool              `json:"stream"`
	ChatTask       string            `json:"chat_task"`
	IsReply        bool              `json:"is_reply"`
	IsRetry        bool              `json:"is_retry"`
	Source         int               `json:"source"`
	Version        string            `json:"version"`
	SessionType    string            `json:"session_type"`
	AgentID        string            `json:"agent_id"`
	TaskID         string            `json:"task_id"`
	CodeLanguage   string            `json:"code_language"`
	ChatPrompt     string            `json:"chat_prompt"`
	ImageURLs      any               `json:"image_urls"`
	AliyunUserType string            `json:"aliyun_user_type"`
	System         string            `json:"system"`
	Messages       []json.RawMessage `json:"messages"`
	Tools          []json.RawMessage `json:"tools"`
	Parameters     struct {
		MaxTokens int `json:"max_tokens"`
	} `json:"parameters"`
	ChatContext struct {
		ChatPrompt string `json:"chatPrompt"`
		ImageURLs  any    `json:"imageUrls"`
		Extra      struct {
			Context     []any `json:"context"`
			ModelConfig struct {
				Key         string `json:"key"`
				IsReasoning bool   `json:"is_reasoning"`
			} `json:"modelConfig"`
			OriginalContent string `json:"originalContent"`
		} `json:"extra"`
		Features []any  `json:"features"`
		Text     string `json:"text"`
	} `json:"chat_context"`
	ModelConfig qoderModelConfig `json:"model_config"`
	Business    struct {
		Product string `json:"product"`
		Version string `json:"version"`
		Type    string `json:"type"`
		Stage   string `json:"stage"`
		ID      string `json:"id"`
		Name    string `json:"name"`
		BeginAt int64  `json:"begin_at"`
	} `json:"business"`
}

// buildQoderRequestPayload converts an OpenAI chat-completions request into the
// native Qoder agent request. The upstream always streams, even for a
// non-streaming client request.
func buildQoderRequestPayload(payload []byte, model, userID string) ([]byte, error) {
	var input struct {
		Messages            []json.RawMessage `json:"messages"`
		Tools               json.RawMessage   `json:"tools"`
		MaxTokens           *int              `json:"max_tokens"`
		MaxCompletionTokens *int              `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, fmt.Errorf("decode OpenAI request payload: %w", err)
	}
	if len(input.Messages) == 0 {
		return nil, fmt.Errorf("OpenAI request contains no messages")
	}

	messages := make([]json.RawMessage, 0, len(input.Messages))
	systemParts := make([]string, 0, 1)
	lastUserText := ""
	for i, raw := range input.Messages {
		var meta qoderMessageMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, fmt.Errorf("decode message %d: %w", i, err)
		}
		if meta.Role == "system" {
			if text := qoderContentText(meta.Content); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		messages = append(messages, append(json.RawMessage(nil), raw...))
		if meta.Role == "user" {
			lastUserText = qoderContentText(meta.Content)
		}
	}

	tools, err := qoderTools(input.Tools)
	if err != nil {
		return nil, err
	}
	maxTokens := qoderMaxOutputTokens
	requestedMax := input.MaxTokens
	if requestedMax == nil {
		requestedMax = input.MaxCompletionTokens
	}
	if requestedMax != nil && *requestedMax > 0 && *requestedMax < maxTokens {
		maxTokens = *requestedMax
	}

	isReasoning := qoderReasoningModel(model)
	modelConfig := qoderModelConfig{
		Key:             model,
		IsReasoning:     isReasoning,
		MaxOutputTokens: qoderMaxOutputTokens,
		Source:          "system",
	}
	requestSetID := qoderStableRecordID(model, messages, tools, maxTokens)

	var req qoderRequest
	req.RequestID = qoderUUID()
	req.RequestSetID = requestSetID
	req.ChatRecordID = requestSetID
	req.SessionID = qoderStableID("qoder-session", userID, model)
	req.Stream = true
	req.ChatTask = "FREE_INPUT"
	req.IsReply = true
	req.IsRetry = false
	req.Source = 1
	req.Version = "3"
	req.SessionType = "qodercli"
	req.AgentID = "agent_common"
	req.TaskID = "common"
	req.System = strings.Join(systemParts, "\n\n")
	req.Messages = messages
	req.Tools = tools
	req.Parameters.MaxTokens = maxTokens
	req.ChatContext.Extra.Context = []any{}
	req.ChatContext.Extra.ModelConfig.Key = model
	req.ChatContext.Extra.ModelConfig.IsReasoning = isReasoning
	req.ChatContext.Extra.OriginalContent = lastUserText
	req.ChatContext.Features = []any{}
	req.ChatContext.Text = lastUserText
	req.ModelConfig = modelConfig
	req.Business.Product = "cli"
	req.Business.Version = "1.0.0"
	req.Business.Type = "agent"
	req.Business.Stage = "start"
	req.Business.ID = qoderUUID()
	req.Business.Name = truncateRunes(lastUserText, 30)
	req.Business.BeginAt = time.Now().UnixMilli()

	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode Qoder request payload: %w", err)
	}
	return encoded, nil
}

func qoderTools(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []json.RawMessage{}, nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("decode OpenAI tools: %w", err)
	}
	if tools == nil {
		return []json.RawMessage{}, nil
	}
	return tools, nil
}

func qoderContentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			out.WriteString(part.Text)
		}
	}
	return out.String()
}

func qoderReasoningModel(model string) bool {
	return model == "ultimate" || model == "performance" || strings.Contains(model, "dmodel") || strings.Contains(model, "dfmodel")
}

func qoderStableRecordID(model string, messages, tools []json.RawMessage, maxTokens int) string {
	parts := make([]string, 0, len(messages)+3)
	parts = append(parts, model)
	for _, message := range messages {
		parts = append(parts, string(message))
	}
	for _, tool := range tools {
		parts = append(parts, string(tool))
	}
	parts = append(parts, fmt.Sprintf("mt=%d", maxTokens))
	return qoderStableID("qoder-record", parts...)
}

func qoderStableID(prefix string, parts ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(prefix))
	for _, part := range parts {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))[:16]
}

func qoderUUID() string {
	var value [16]byte
	if _, err := cryptorand.Read(value[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit])
}

type qoderEnvelope struct {
	StatusCodeValue int    `json:"statusCodeValue"`
	Body            string `json:"body"`
}

type qoderStreamConverter struct {
	id      string
	model   string
	created int64
	done    bool
}

func newQoderStreamConverter(model string) *qoderStreamConverter {
	return &qoderStreamConverter{
		id:      "chatcmpl-qoder-" + strings.ReplaceAll(qoderUUID(), "-", ""),
		model:   model,
		created: time.Now().Unix(),
	}
}

// convertLine unwraps one Qoder SSE envelope and emits standard OpenAI SSE.
func (c *qoderStreamConverter) convertLine(line []byte) ([][]byte, error) {
	inner, done, ignored, err := parseQoderLine(line)
	if err != nil {
		return nil, err
	}
	if ignored || c.done {
		return nil, nil
	}
	if done {
		c.done = true
		return nil, nil
	}
	normalized, err := c.normalizeChunk(inner)
	if err != nil {
		return nil, err
	}
	return [][]byte{normalized}, nil
}

func (c *qoderStreamConverter) normalizeChunk(inner []byte) ([]byte, error) {
	var chunk map[string]any
	decoder := json.NewDecoder(bytes.NewReader(inner))
	decoder.UseNumber()
	if err := decoder.Decode(&chunk); err != nil {
		return nil, fmt.Errorf("decode Qoder response body: %w", err)
	}
	if chunk == nil {
		return nil, fmt.Errorf("Qoder response body is not a JSON object")
	}
	if id, ok := chunk["id"].(string); ok && id != "" {
		c.id = id
	} else {
		chunk["id"] = c.id
	}
	chunk["object"] = "chat.completion.chunk"
	if _, ok := chunk["created"]; !ok {
		chunk["created"] = c.created
	}
	if c.model != "" {
		chunk["model"] = c.model
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI stream chunk: %w", err)
	}
	return encoded, nil
}

func (c *qoderStreamConverter) finish() [][]byte {
	c.done = true
	return nil
}

func parseQoderLine(line []byte) (inner []byte, done, ignored bool, err error) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte(":")) {
		return nil, false, true, nil
	}
	if bytes.HasPrefix(trimmed, []byte(sseDataPrefix)) {
		trimmed = bytes.TrimSpace(trimmed[len(sseDataPrefix):])
	} else if trimmed[0] != '{' {
		return nil, false, true, nil
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil, true, false, nil
	}

	var envelope qoderEnvelope
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, false, false, fmt.Errorf("decode Qoder SSE envelope: %w", err)
	}
	if envelope.StatusCodeValue == 0 && envelope.Body == "" {
		var direct qoderChunk
		if err := json.Unmarshal(trimmed, &direct); err == nil && len(direct.Choices) > 0 {
			return append([]byte(nil), trimmed...), false, false, nil
		}
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &metadata); err == nil {
			if _, ok := metadata["firstTokenDuration"]; ok {
				return nil, false, true, nil
			}
		}
	}
	if envelope.StatusCodeValue != 200 {
		body := envelope.Body
		if body == "" {
			body = string(trimmed)
		}
		if len(body) > 512 {
			body = body[:512]
		}
		return nil, false, false, fmt.Errorf("Qoder upstream status %d: %s", envelope.StatusCodeValue, body)
	}
	if envelope.Body == "" {
		return nil, false, true, nil
	}
	if envelope.Body == "[DONE]" {
		return nil, true, false, nil
	}
	return []byte(envelope.Body), false, false, nil
}

type qoderChunk struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string               `json:"content"`
			ReasoningContent string               `json:"reasoning_content"`
			ToolCalls        []qoderChunkToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

type qoderChunkToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type qoderToolCallAggregate struct {
	Index     int
	ID        string
	Type      string
	Name      string
	Arguments strings.Builder
}

type qoderCompletionMessage struct {
	Role             string                `json:"role"`
	Content          string                `json:"content"`
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	ToolCalls        []qoderCompletionTool `json:"tool_calls,omitempty"`
}

type qoderCompletionTool struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type qoderCompletion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int                    `json:"index"`
		Message      qoderCompletionMessage `json:"message"`
		FinishReason *string                `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage,omitempty"`
}

// convertQoderSSEResponse aggregates a native Qoder SSE response for an
// OpenAI non-streaming client.
func convertQoderSSEResponse(raw []byte, model string) ([]byte, error) {
	converter := newQoderStreamConverter(model)
	var content strings.Builder
	var reasoning strings.Builder
	toolCalls := make(map[int]*qoderToolCallAggregate)
	var finishReason *string
	var usage json.RawMessage
	id := converter.id
	created := converter.created

	lines := bytes.Split(raw, []byte{'\n'})
	for _, line := range lines {
		inner, done, ignored, err := parseQoderLine(bytes.TrimSuffix(line, []byte{'\r'}))
		if err != nil {
			return nil, err
		}
		if done || ignored {
			continue
		}
		var chunk qoderChunk
		if err := json.Unmarshal(inner, &chunk); err != nil {
			return nil, fmt.Errorf("decode Qoder response body: %w", err)
		}
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Created != 0 {
			created = chunk.Created
		}
		if len(bytes.TrimSpace(chunk.Usage)) > 0 && !bytes.Equal(bytes.TrimSpace(chunk.Usage), []byte("null")) {
			usage = append(usage[:0], chunk.Usage...)
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			for _, fragment := range choice.Delta.ToolCalls {
				state := toolCalls[fragment.Index]
				if state == nil {
					state = &qoderToolCallAggregate{Index: fragment.Index, Type: "function"}
					toolCalls[fragment.Index] = state
				}
				if fragment.ID != "" {
					state.ID = fragment.ID
				}
				if fragment.Type != "" {
					state.Type = fragment.Type
				}
				if fragment.Function.Name != "" {
					state.Name = fragment.Function.Name
				}
				state.Arguments.WriteString(fragment.Function.Arguments)
			}
			if choice.FinishReason != nil {
				reason := *choice.FinishReason
				finishReason = &reason
			}
		}
	}

	completion := qoderCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Usage:   usage,
	}
	completion.Choices = make([]struct {
		Index        int                    `json:"index"`
		Message      qoderCompletionMessage `json:"message"`
		FinishReason *string                `json:"finish_reason"`
	}, 1)
	completion.Choices[0].Message.Role = "assistant"
	completion.Choices[0].Message.Content = content.String()
	completion.Choices[0].Message.ReasoningContent = reasoning.String()
	completion.Choices[0].FinishReason = finishReason

	indexes := make([]int, 0, len(toolCalls))
	for index := range toolCalls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		state := toolCalls[index]
		tool := qoderCompletionTool{ID: state.ID, Type: state.Type}
		tool.Function.Name = state.Name
		tool.Function.Arguments = state.Arguments.String()
		completion.Choices[0].Message.ToolCalls = append(completion.Choices[0].Message.ToolCalls, tool)
	}

	encoded, err := json.Marshal(completion)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI completion: %w", err)
	}
	return encoded, nil
}

// sseSplitter buffers raw upstream byte chunks and yields complete SSE lines.
type sseSplitter struct {
	buf strings.Builder
}

func (s *sseSplitter) feed(raw []byte) []string {
	s.buf.Write(raw)
	data := s.buf.String()
	var lines []string
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] != '\n' {
			continue
		}
		lines = append(lines, strings.TrimRight(data[start:i], "\r"))
		start = i + 1
	}
	s.buf.Reset()
	s.buf.WriteString(data[start:])
	return lines
}
