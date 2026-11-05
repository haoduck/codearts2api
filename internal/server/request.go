// OpenAI chat.completions 请求解析与会话指纹。
//
// 支持完整 OpenAI 消息模型：system/user/assistant/tool 四种角色、
// string 与分片 content、assistant 的 tool_calls、tool 结果的 tool_call_id。
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"codearts2api/internal/upstream"
)

// openAIToolCall 一次函数调用（assistant 历史或响应里用）。
type openAIToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串（OpenAI 线格式）
}

// openAIMessage 拍平后的一条消息。
type openAIMessage struct {
	Role             string
	Text             string
	ReasoningContent string
	ToolCalls        []openAIToolCall
	ToolCallID       string // role=tool 时对应的 call id
	Name             string // role=tool 时的函数名（部分客户端会带）
}

// toolChoiceOpenAI 归一化后的 tool_choice。
type toolChoiceOpenAI struct {
	Mode     string // "" / "none" / "auto" / "required" / "function"
	Function string // Mode=="function" 时指定的函数名
}

// chatRequest 完整请求体。
type chatRequest struct {
	Model           string
	Stream          bool
	IncludeUsage    bool
	ReasoningEffort string
	MaxTokens       *int
	Temperature     *float64
	TopP            *float64
	Messages        []openAIMessage
	Tools           []map[string]any
	ToolChoice      toolChoiceOpenAI
	ConversationID  string
}

// parseChatRequest 解析并校验请求体。
func parseChatRequest(body []byte) (*chatRequest, error) {
	var raw struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		ReasoningEffort     string            `json:"reasoning_effort"`
		MaxTokens           *int              `json:"max_tokens"`
		MaxCompletionTokens *int              `json:"max_completion_tokens"`
		Temperature         *float64          `json:"temperature"`
		TopP                *float64          `json:"top_p"`
		ConversationID      string            `json:"conversation_id"`
		Messages            []json.RawMessage `json:"messages"`
		Tools               []map[string]any  `json:"tools"`
		ToolChoice          json.RawMessage   `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	if len(raw.Messages) == 0 {
		return nil, errors.New("messages is empty")
	}
	req := &chatRequest{
		Model:           raw.Model,
		Stream:          raw.Stream,
		IncludeUsage:    raw.StreamOptions.IncludeUsage,
		ReasoningEffort: strings.ToLower(strings.TrimSpace(raw.ReasoningEffort)),
		MaxTokens:       raw.MaxTokens,
		Temperature:     raw.Temperature,
		TopP:            raw.TopP,
		ConversationID:  raw.ConversationID,
		Tools:           raw.Tools,
		ToolChoice:      parseToolChoice(raw.ToolChoice),
	}
	if raw.MaxCompletionTokens != nil {
		req.MaxTokens = raw.MaxCompletionTokens
	}
	if req.MaxTokens != nil && *req.MaxTokens <= 0 {
		return nil, errors.New("max_tokens must be greater than 0")
	}
	// 上游对 max_tokens 有 65536 硬上限，超出即回 InferHub.001001005.400
	// The request param is invalid（实测 2026-10-16，跨模型一致）。这里提前钳制，
	// 客户端仍可按 OpenAI 语义发任意值，不必自己适配上游限制。
	if req.MaxTokens != nil && *req.MaxTokens > upstream.MaxTokensLimit {
		clamped := upstream.MaxTokensLimit
		req.MaxTokens = &clamped
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return nil, errors.New("temperature must be between 0 and 2")
	}
	if req.TopP != nil && (*req.TopP < 0 || *req.TopP > 1) {
		return nil, errors.New("top_p must be between 0 and 1")
	}
	for i, rm := range raw.Messages {
		m, err := parseMessage(rm)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		if m.Role == "user" && strings.TrimSpace(m.Text) == "" && len(rm) > 0 {
			// 能读的字段都读不出来：把原始片段打出来，便于定位客户端到底把
			// 文本放哪了（这是「模型只回一句问候语」这类现象的唯一线索）。
			log.Printf("request: messages[%d] role=user content 解析为空 raw=%s", i, truncateMsg(string(rm), 240))
		}
		req.Messages = append(req.Messages, m)
	}
	// 必须有一条真能发出内容的 user/tool 消息。空 user 消息不能放行：
	// 上游会把它当成「继续上一个会话」，回一段与本次请求无关的内容。
	hasSendable, hasUser := false, false
	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			hasUser = true
			if strings.TrimSpace(m.Text) != "" {
				hasSendable = true
			}
		case "tool":
			hasSendable = true
		}
	}
	if !hasSendable {
		if hasUser {
			return nil, errors.New("user 消息的 content 解析为空：文本要放在 content 字符串里，或 [{\"type\":\"text\",\"text\":\"...\"}] 分片里" +
				"（空内容会被上游当成「继续上一个会话」，返回与本次请求无关的内容）")
		}
		return nil, errors.New("no user or tool message found")
	}
	return req, nil
}

// parseMessage 解析单条消息：content 支持 string 与 [{type:text,text}] 分片。
func parseMessage(rm json.RawMessage) (openAIMessage, error) {
	var probe struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ReasoningContent string          `json:"reasoning_content"`
		ToolCalls        json.RawMessage `json:"tool_calls"`
		ToolCallID       string          `json:"tool_call_id"`
		Name             string          `json:"name"`
	}
	if err := json.Unmarshal(rm, &probe); err != nil {
		return openAIMessage{}, err
	}
	m := openAIMessage{
		Role:             strings.ToLower(strings.TrimSpace(probe.Role)),
		Text:             flattenContent(probe.Content),
		ReasoningContent: probe.ReasoningContent,
		ToolCallID:       probe.ToolCallID,
		Name:             probe.Name,
	}
	if len(probe.ToolCalls) > 0 && string(probe.ToolCalls) != "null" {
		var calls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		}
		if err := json.Unmarshal(probe.ToolCalls, &calls); err == nil {
			for _, c := range calls {
				m.ToolCalls = append(m.ToolCalls, openAIToolCall{
					ID:        c.ID,
					Name:      c.Function.Name,
					Arguments: rawJSONString(c.Function.Arguments),
				})
			}
		}
	}
	return m, nil
}

// flattenContent 拍平 string / 分片 content 为纯文本。
//
// 不同客户端把文本放在不同形状里：字符串、[{type:text,text}]、OpenAI Responses
// 的 input_text/output_text、Anthropic 的 {type:text,text}，甚至
// {"text":{"value":"..."}} 这类嵌套对象。解析不出来就返回空串，但空串
// **绝不能原样发给上游**：CodeArts 会把「空 user 消息」当成「继续上一个会话」，
// 回一段与本次请求无关的历史内容（实测会串出同账号别的会话，表现为一句
// 莫名其妙的问候语）。因此 parseChatRequest 会直接拒绝这种请求。
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(textFromPart(p))
		}
		return sb.String()
	}
	return textFromPart(raw)
}

// textFromPart 从单个 content 分片里取文本（兼容嵌套 content/value）。
func textFromPart(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for _, key := range []string{"text", "input_text", "output_text", "content", "value"} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			return t
		case map[string]any:
			for _, nested := range []string{"value", "text"} {
				if str, ok := t[nested].(string); ok {
					return str
				}
			}
		case []any:
			if nested, err := json.Marshal(t); err == nil {
				return flattenContent(nested)
			}
		}
	}
	return ""
}

// rawJSONString 把 arguments 统一成 JSON 字符串（兼容字符串与对象两种线格式）。
func rawJSONString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "{}"
	}
	out, err := json.Marshal(v) // Go map marshal 按 key 排序，天然归一化
	if err != nil {
		return "{}"
	}
	return string(out)
}

// parseToolChoice 归一化 tool_choice：字符串或 {"type":"function","function":{"name":...}}。
func parseToolChoice(raw json.RawMessage) toolChoiceOpenAI {
	if len(raw) == 0 || string(raw) == "null" {
		return toolChoiceOpenAI{Mode: "auto"}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(s) {
		case "none":
			return toolChoiceOpenAI{Mode: "none"}
		case "required":
			return toolChoiceOpenAI{Mode: "required"}
		default:
			return toolChoiceOpenAI{Mode: "auto"}
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Function.Name != "" {
		return toolChoiceOpenAI{Mode: "function", Function: obj.Function.Name}
	}
	return toolChoiceOpenAI{Mode: "auto"}
}

// ---------------------------------------------------------------------------
// 会话指纹（无状态客户端的历史对齐）
// ---------------------------------------------------------------------------

// canonical 消息归一化串：指纹链的输入。client 回发的 assistant 消息
// 由我们生成（tool_calls id 也是我们发的），因此可逐字节复算。
func canonical(m openAIMessage) string {
	var sb strings.Builder
	sb.WriteString(m.Role)
	sb.WriteString("\x00")
	sb.WriteString(m.Text)
	for _, c := range m.ToolCalls {
		sb.WriteString("\x00")
		sb.WriteString(c.ID)
		sb.WriteString("\x00")
		sb.WriteString(c.Name)
		sb.WriteString("\x00")
		sb.WriteString(normalizeArgs(c.Arguments))
	}
	if m.ToolCallID != "" {
		sb.WriteString("\x00tool:")
		sb.WriteString(m.ToolCallID)
	}
	return sb.String()
}

// normalizeArgs 归一化 arguments（key 排序、去空白）。
func normalizeArgs(args string) string {
	if args == "" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return args
	}
	out, err := json.Marshal(v)
	if err != nil {
		return args
	}
	return string(out)
}

// chainFingerprint 链式指纹：fp_i = sha256(fp_{i-1} + canonical(m_i))。
func chainFingerprint(prev string, m openAIMessage) string {
	sum := sha256.Sum256([]byte(prev + "\x01" + canonical(m)))
	return hex.EncodeToString(sum[:16])
}

// fingerprintOf 计算整段消息列表的链式指纹。
func fingerprintOf(msgs []openAIMessage) string {
	fp := ""
	for _, m := range msgs {
		fp = chainFingerprint(fp, m)
	}
	return fp
}

// conversationIDFor 为不带会话 ID 的 OpenAI 客户端提供稳定的 CodeArts chat_id。
// Agent 工具链的历史会持续增长，但首条 user 消息不变，因此可作为会话锚点。
func conversationIDFor(req *chatRequest, headerValue string) string {
	for _, explicit := range []string{req.ConversationID, headerValue} {
		explicit = strings.TrimSpace(explicit)
		if explicit == "" {
			continue
		}
		if validChatID(explicit) {
			return strings.ToLower(explicit)
		}
		sum := sha256.Sum256([]byte(explicit))
		return hex.EncodeToString(sum[:16])
	}
	for _, message := range req.Messages {
		// 空内容的 user 消息不能当锚点：所有解析失败的请求都会算出同一个
		// chat_id，等于把这些请求全部塞进上游同一个会话（会串出别人的历史）。
		if message.Role != "user" || strings.TrimSpace(message.Text) == "" {
			continue
		}
		anchor := canonical(message)
		if len(req.Tools) > 0 {
			// OpenCode 会用同一用户消息先请求无工具的会话标题，再启动工具 Agent。
			// 两者必须分开，否则标题生成会污染内置 Agent 会话。
			anchor += "\x00tools"
		}
		sum := sha256.Sum256([]byte(anchor))
		return hex.EncodeToString(sum[:16])
	}
	return ""
}
