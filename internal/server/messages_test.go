package server

import (
	"strings"
	"testing"
)

func msg(role, text string, calls ...openAIToolCall) openAIMessage {
	return openAIMessage{Role: role, Text: text, ToolCalls: calls}
}

func call(id, name string) openAIToolCall {
	return openAIToolCall{ID: id, Name: name, Arguments: "{}"}
}

// developer 角色要映射成 system（上游只认 system/user/assistant/tool）。
func TestNormalizeUpstreamMessagesMapsDeveloper(t *testing.T) {
	out := normalizeUpstreamMessages([]openAIMessage{
		msg("developer", "you are codex"),
		msg("user", "hi"),
	})
	if len(out) != 2 || out[0].Role != "system" || out[0].Text != "you are codex" {
		t.Fatalf("developer 未映射: %+v", out)
	}
}

// 相邻的 assistant tool_calls 要合并成一条（否则上游判序列断裂）。
func TestMergeAdjacentToolCalls(t *testing.T) {
	a := msg("assistant", "", call("c1", "read"))
	a.ReasoningContent = "think-1"
	b := msg("assistant", "", call("c2", "write"))
	b.ReasoningContent = "think-2"
	out := normalizeUpstreamMessages([]openAIMessage{
		msg("user", "go"),
		a, b,
		msg("tool", "r1", /* tool_call_id 由下面的字段设置 */),
	})
	if len(out) != 3 {
		t.Fatalf("应合并成 3 条，got %d: %+v", len(out), out)
	}
	if len(out[1].ToolCalls) != 2 || out[1].ToolCalls[0].ID != "c1" || out[1].ToolCalls[1].ID != "c2" {
		t.Fatalf("tool_calls 未合并: %+v", out[1].ToolCalls)
	}
	if out[1].ReasoningContent != "think-1\nthink-2" {
		t.Fatalf("reasoning_content 未拼接: %q", out[1].ReasoningContent)
	}
}

// 形态二：纯文本 assistant 并入前一条无正文的 tool_calls assistant。
func TestMergeAdjacentToolCallsTextForm(t *testing.T) {
	out := normalizeUpstreamMessages([]openAIMessage{
		msg("user", "go"),
		msg("assistant", "", call("c1", "read")),
		msg("assistant", "补充说明"),
	})
	if len(out) != 2 {
		t.Fatalf("应合并成 2 条，got %d", len(out))
	}
	if out[1].Text != "补充说明" || len(out[1].ToolCalls) != 1 {
		t.Fatalf("文本未并入: %+v", out[1])
	}
}

// 不相邻的不能合并（中间隔了 user）。
func TestMergeAdjacentToolCallsRequiresAdjacency(t *testing.T) {
	out := normalizeUpstreamMessages([]openAIMessage{
		msg("assistant", "", call("c1", "read")),
		msg("user", "再来"),
		msg("assistant", "", call("c2", "write")),
	})
	if len(out) != 3 {
		t.Fatalf("不相邻不应合并，got %d", len(out))
	}
}

// 夹在工具结果之间的非结果消息要挪到整批之后（配对不能被打断）。
func TestRepackToolResultBlocks(t *testing.T) {
	asst := msg("assistant", "", call("c1", "a"), call("c2", "b"))
	r1 := openAIMessage{Role: "tool", Text: "r1", ToolCallID: "c1"}
	intruder := msg("system", "notice")
	r2 := openAIMessage{Role: "tool", Text: "r2", ToolCallID: "c2"}
	out := normalizeUpstreamMessages([]openAIMessage{msg("user", "go"), asst, r1, intruder, r2})

	roles := make([]string, 0, len(out))
	for _, m := range out {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,tool,tool,system" {
		t.Fatalf("重排结果=%v，want user,assistant,tool,tool,system", roles)
	}
	if out[2].ToolCallID != "c1" || out[3].ToolCallID != "c2" {
		t.Fatalf("结果顺序被改变: %+v", out[2:4])
	}
}

// 结果前面插队也要挪后。
func TestRepackToolResultBeforeFirstResult(t *testing.T) {
	asst := msg("assistant", "", call("c1", "a"))
	out := normalizeUpstreamMessages([]openAIMessage{
		msg("user", "go"), asst,
		msg("system", "notice"),
		openAIMessage{Role: "tool", Text: "r1", ToolCallID: "c1"},
	})
	roles := make([]string, 0, len(out))
	for _, m := range out {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,tool,system" {
		t.Fatalf("重排结果=%v，want user,assistant,tool,system", roles)
	}
}
