package server

import (
	"encoding/json"
	"strings"
	"testing"

	"codearts2api/internal/upstream"
)

// 上游 max_tokens 硬上限为 65536，超出即 InferHub.001001005.400。
// 解析层必须提前钳制，客户端无需自行适配（实测 2026-10-16）。
func TestParseChatRequestClampsMaxTokensToUpstreamLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"max_tokens under limit", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":4096}`, 4096},
		{"max_tokens at limit", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":65536}`, 65536},
		{"max_tokens over limit", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":131072}`, upstream.MaxTokensLimit},
		{"max_completion_tokens over limit", `{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":1000000}`, upstream.MaxTokensLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := parseChatRequest([]byte(tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if req.MaxTokens == nil || *req.MaxTokens != tc.want {
				t.Fatalf("MaxTokens=%v want %d", req.MaxTokens, tc.want)
			}
		})
	}
}

// 未提供 max_tokens 时保持 nil，交给上游默认值（不得凭空写死一个上限）。
func TestParseChatRequestLeavesMaxTokensUnset(t *testing.T) {
	req, err := parseChatRequest([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.MaxTokens != nil {
		t.Fatalf("MaxTokens=%v, want nil", *req.MaxTokens)
	}
}

// 非法值仍然必须报错，不能被钳制逻辑顺手放过。
func TestParseChatRequestRejectsNonPositiveMaxTokens(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"hi"}],"max_tokens":0}`,
		`{"messages":[{"role":"user","content":"hi"}],"max_tokens":-1}`,
	} {
		if _, err := parseChatRequest([]byte(body)); err == nil {
			t.Fatalf("expected error for %s", body)
		}
	}
}

func TestBuildUpstreamMessagesNewChat(t *testing.T) {
	req := &chatRequest{
		Messages: []openAIMessage{
			{Role: "system", Text: "be nice"},
			{Role: "user", Text: "hi"},
			{Role: "assistant", Text: "hello"},
			{Role: "user", Text: "again"},
		},
	}
	msgs := buildUpstreamMessages(req, false)
	if len(msgs) != 4 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[0].Content != "be nice" || msgs[3].Role != "user" || msgs[3].Content != "again" {
		t.Fatalf("native messages lost roles/content: %#v", msgs)
	}
}

func TestBuildUpstreamMessagesContinueChat(t *testing.T) {
	req := &chatRequest{
		Messages: []openAIMessage{
			{Role: "system", Text: "be nice"},
			{Role: "user", Text: "hi"},
			{Role: "user", Text: "again"},
		},
	}
	msgs := buildUpstreamMessages(req, false)
	if len(msgs) != 3 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	// V2 不依赖服务端持续 Agent 语义状态：完整历史以原生 role 重放。
	if msgs[0].Role != "system" || msgs[1].Content != "hi" || msgs[2].Content != "again" {
		t.Fatalf("continue omitted native history: %#v", msgs)
	}
}

func TestBuildUpstreamMessagesContinueToolTurnReplaysStatelessHistory(t *testing.T) {
	req := &chatRequest{
		Messages: []openAIMessage{
			{Role: "system", Text: "be nice"},
			{Role: "user", Text: "inspect the repository"},
			{Role: "assistant", ToolCalls: []openAIToolCall{{ID: "call_1", Name: "bash", Arguments: `{"command":"pwd"}`}}},
			{Role: "tool", ToolCallID: "call_1", Name: "bash", Text: "/workspace"},
		},
	}
	msgs := buildUpstreamMessages(req, true)
	if len(msgs) != 4 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	if msgs[1].Role != "user" || msgs[1].Content != "inspect the repository" {
		t.Fatalf("tool continuation omitted original task: %#v", msgs)
	}
	if msgs[2].Role != "assistant" || len(msgs[2].ToolCalls) != 1 || msgs[2].ToolCalls[0].Function.Name != "bash" {
		t.Fatalf("assistant tool call was not native: %#v", msgs[2])
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "call_1" || msgs[3].Content != "/workspace" {
		t.Fatalf("tool result was not native: %#v", msgs[3])
	}
}

func TestBuildUpstreamMessagesDropsIncompleteToolBatch(t *testing.T) {
	req := &chatRequest{Messages: []openAIMessage{
		{Role: "user", Text: "inspect"},
		{Role: "assistant", Text: "still useful", ToolCalls: []openAIToolCall{
			{ID: "call_1", Name: "read", Arguments: `{}`},
			{ID: "call_2", Name: "bash", Arguments: `{}`},
		}},
		{Role: "tool", ToolCallID: "call_1", Text: "partial"},
		{Role: "user", Text: "continue"},
	}}
	msgs := buildUpstreamMessages(req, true)
	if len(msgs) != 3 {
		t.Fatalf("orphan tool result should be dropped: %#v", msgs)
	}
	if msgs[1].Role != "assistant" || len(msgs[1].ToolCalls) != 0 || msgs[1].Content != "still useful" {
		t.Fatalf("incomplete tool call batch should be stripped: %#v", msgs[1])
	}
}

func TestParseChatRequest(t *testing.T) {
	body := `{
		"model":"glm-5.2",
		"stream":true,
		"reasoning_effort":"high",
		"max_completion_tokens":8192,
		"temperature":0.3,
		"top_p":0.8,
		"conversation_id":"conv-1",
		"messages":[
			{"role":"system","content":"be nice"},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"hello","reasoning_content":"thought"},
			{"role":"user","content":[{"type":"text","text":"again"}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		],
		"tools":[{"type":"function","function":{"name":"f1","parameters":{"type":"object","properties":{}}}}],
		"tool_choice":"required"
	}`
	req, err := parseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "glm-5.2" || !req.Stream || req.ConversationID != "conv-1" {
		t.Fatalf("basic fields: %+v", req)
	}
	if req.ReasoningEffort != "high" || req.MaxTokens == nil || *req.MaxTokens != 8192 {
		t.Fatalf("generation options: %+v", req)
	}
	if req.Temperature == nil || *req.Temperature != 0.3 || req.TopP == nil || *req.TopP != 0.8 {
		t.Fatalf("sampling options: %+v", req)
	}
	if len(req.Messages) != 5 {
		t.Fatalf("messages=%d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Text != "be nice" {
		t.Fatalf("sys=%+v", req.Messages[0])
	}
	if req.Messages[3].Text != "again" {
		t.Fatalf("fragmented content=%q", req.Messages[3].Text)
	}
	if req.Messages[2].ReasoningContent != "thought" {
		t.Fatalf("reasoning content=%q", req.Messages[2].ReasoningContent)
	}
	if req.Messages[4].Role != "tool" || req.Messages[4].ToolCallID != "call_1" {
		t.Fatalf("tool msg=%+v", req.Messages[4])
	}
	if len(req.Tools) != 1 || req.ToolChoice.Mode != "required" {
		t.Fatalf("tools=%v choice=%+v", req.Tools, req.ToolChoice)
	}
}

func TestParseChatRequestErrors(t *testing.T) {
	if _, err := parseChatRequest([]byte(`{}`)); err == nil {
		t.Fatal("expected error for empty messages")
	}
	if _, err := parseChatRequest([]byte(`{"messages":[{"role":"assistant","content":"x"}]}`)); err == nil {
		t.Fatal("expected error: no user/tool message")
	}
	if _, err := parseChatRequest([]byte(`{"max_tokens":0,"messages":[{"role":"user","content":"x"}]}`)); err == nil {
		t.Fatal("expected error: invalid max_tokens")
	}
	if _, err := parseChatRequest([]byte(`{"top_p":2,"messages":[{"role":"user","content":"x"}]}`)); err == nil {
		t.Fatal("expected error: invalid top_p")
	}
}

func TestParseAssistantToolCalls(t *testing.T) {
	body := `{"messages":[{"role":"assistant","content":"","tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}}
	]},{"role":"user","content":"done"}]}`
	req, err := parseChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages[0].ToolCalls) != 1 {
		t.Fatalf("tool calls=%d", len(req.Messages[0].ToolCalls))
	}
	c := req.Messages[0].ToolCalls[0]
	if c.ID != "call_1" || c.Name != "f1" || c.Arguments != `{"a":1}` {
		t.Fatalf("call=%+v", c)
	}
}

func TestRawJSONString(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"{\"a\":1}"`, `{"a":1}`},
		{`{"a":1,"b":2}`, `{"a":1,"b":2}`},
		{``, `{}`},
		{`null`, `{}`},
	}
	for _, c := range cases {
		if got := rawJSONString(json.RawMessage(c.in)); got != c.want {
			t.Fatalf("rawJSONString(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestFingerprintStableAndReplay(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "hello"},
		{Role: "user", Text: "again"},
	}
	fp1 := fingerprintOf(msgs)
	fp2 := fingerprintOf(msgs)
	if fp1 != fp2 {
		t.Fatalf("fingerprint not stable: %s vs %s", fp1, fp2)
	}
	// 网关生成的 assistant tool_calls 被客户端原样回发时应能复算。
	base := []openAIMessage{{Role: "user", Text: "q"}}
	assistant := openAIMessage{Role: "assistant", Text: "", ToolCalls: []openAIToolCall{
		{ID: "call_x", Name: "f", Arguments: `{"p":1}`},
	}}
	after := chainFingerprint(fingerprintOf(base), assistant)
	replay := append([]openAIMessage{}, base...)
	replay = append(replay, assistant)
	if fingerprintOf(replay) != after {
		t.Fatalf("replay fingerprint mismatch: %s vs %s", fingerprintOf(replay), after)
	}
}

func TestConversationIDStaysStableAsOpenAIHistoryGrows(t *testing.T) {
	first := openAIMessage{Role: "user", Text: "开始一个长时间代码审计"}
	round1 := &chatRequest{Messages: []openAIMessage{
		{Role: "system", Text: "time=1"},
		first,
	}}
	round2 := &chatRequest{Messages: []openAIMessage{
		{Role: "system", Text: "time=2"},
		first,
		{Role: "assistant", ToolCalls: []openAIToolCall{{ID: "call_1", Name: "bash", Arguments: `{"command":"pwd"}`}}},
		{Role: "tool", ToolCallID: "call_1", Text: "/workspace"},
	}}

	id1 := conversationIDFor(round1, "")
	id2 := conversationIDFor(round2, "")
	if id1 == "" || id1 != id2 || !validChatID(id1) {
		t.Fatalf("conversation IDs are not stable valid chat IDs: %q vs %q", id1, id2)
	}
}

func TestConversationIDSeparatesTitleChatFromToolAgent(t *testing.T) {
	first := openAIMessage{Role: "user", Text: "inspect the repository"}
	title := &chatRequest{Messages: []openAIMessage{first}}
	agent := &chatRequest{
		Messages: []openAIMessage{first},
		Tools:    []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}},
	}
	if conversationIDFor(title, "") == conversationIDFor(agent, "") {
		t.Fatal("title generation and tool agent must not share CodeArts built-in chat state")
	}
}

func TestFlattenContent(t *testing.T) {
	if flattenContent(json.RawMessage(`"plain"`)) != "plain" {
		t.Fatal("string content")
	}
	if flattenContent(json.RawMessage(`[{"type":"text","text":"a"},{"type":"image_url","imageUrl":{"url":"x"}}]`)) != "a" {
		t.Fatal("parts content")
	}
	if flattenContent(json.RawMessage(`null`)) != "" {
		t.Fatal("null content")
	}
}

var _ = strings.TrimSpace

// 客户端把文本放在各种形状里；解析不出来就会变成「空 user 消息」。
func TestFlattenContentShapes(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"string", `"你好"`, "你好"},
		{"text parts", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "ab"},
		{"input_text", `[{"type":"input_text","text":"x"}]`, "x"},
		{"nested text value", `[{"type":"text","text":{"value":"y"}}]`, "y"},
		{"anthropic tool_result", `[{"type":"tool_result","content":[{"type":"text","text":"z"}]}]`, "z"},
		{"object content", `{"text":"w"}`, "w"},
		{"image only", `[{"type":"image_url","image_url":{"url":"data:x"}}]`, ""},
		{"null", `null`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := flattenContent(json.RawMessage(tc.raw)); got != tc.want {
				t.Fatalf("flattenContent(%s)=%q want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// 空 user 内容必须拒：上游会把「空 user 消息」当成「继续上一个会话」，
// 回一段与本次请求无关的内容（实测串出别的会话，表现为一句莫名其妙的问候语）。
func TestParseChatRequestRejectsEmptyUserContent(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":""}]}`,
		`{"messages":[{"role":"user","content":"   "}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:x"}}]}]}`,
	} {
		_, err := parseChatRequest([]byte(body))
		if err == nil {
			t.Fatalf("body=%s 应当报错", body)
		}
		if !strings.Contains(err.Error(), "content 解析为空") {
			t.Fatalf("body=%s err=%v, want content 解析为空", body, err)
		}
	}
	// 多轮工具链里 tool 结果为空是合法的，不能拦。
	if _, err := parseChatRequest([]byte(`{"messages":[{"role":"tool","tool_call_id":"c1","content":""}]}`)); err != nil {
		t.Fatalf("空 tool 结果应放行: %v", err)
	}
	// 分片文本照常放行。
	if _, err := parseChatRequest([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)); err != nil {
		t.Fatalf("分片文本应放行: %v", err)
	}
}

// 空 user 消息不能当 chat_id 锚点：否则所有解析失败的请求共用一个上游会话。
func TestConversationIDForIgnoresEmptyUserMessages(t *testing.T) {
	mk := func(text string) *chatRequest {
		return &chatRequest{Messages: []openAIMessage{
			{Role: "user", Text: ""},
			{Role: "user", Text: text},
		}}
	}
	a := conversationIDFor(mk("问题A"), "")
	b := conversationIDFor(mk("问题B"), "")
	if a == "" || b == "" {
		t.Fatalf("anchor 不应为空: a=%q b=%q", a, b)
	}
	if a == b {
		t.Fatalf("不同问题算出同一个 chat_id=%q（空 user 消息被当成了锚点）", a)
	}
	if got := conversationIDFor(&chatRequest{Messages: []openAIMessage{{Role: "user", Text: ""}}}, ""); got != "" {
		t.Fatalf("全空消息的 anchor=%q，want 空（交给调用方生成随机 chat_id）", got)
	}
}

