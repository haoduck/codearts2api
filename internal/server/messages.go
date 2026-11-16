// 上游消息归一：把客户端回放的 OpenAI 历史修成上游能接受的最小一致形状。
//
// 对照 workbuddy2api-hub 的 normalize_roles / merge_adjacent_tool_calls /
// repack_tool_result_blocks / cleanup_orphan_tool_calls 四件套。上游对
// tool_calls 与 tool 结果的**相邻性**要求很严，形状不对会直接 400 并让整条
// 对话废掉（每次重试都重放同一份历史，换账号也没用），所以必须在网关侧修。
package server

import "strings"

// normalizeUpstreamMessages 依次做三件事（第 4 件 cleanup 由
// pairedToolMessages 在 buildUpstreamMessages 里完成）：
//  1. developer → system：上游只认 system/user/assistant/tool，OpenAI 新版
//     developer 角色（Codex CLI 等在用）原样发过去会被拒；
//  2. 合并相邻的 assistant tool_calls：客户端把并行批次拆成多条时，
//     上游判为 tool_call 序列断裂；
//  3. 把夹在工具结果之间的非结果消息挪到整批之后：否则配对读起来是断的。
func normalizeUpstreamMessages(msgs []openAIMessage) []openAIMessage {
	out := make([]openAIMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "developer" {
			m.Role = "system"
		}
		out = append(out, m)
	}
	out = mergeAdjacentToolCalls(out)
	out = repackToolResultBlocks(out)
	return out
}

// joinReasoning 拼接 reasoning_content（多轮思考要求原样回传）。
func joinReasoning(prev, next string) string {
	if next == "" {
		return prev
	}
	if prev == "" {
		return next
	}
	return prev + "\n" + next
}

// mergeAdjacentToolCalls 合并相邻的 assistant tool_calls 消息。
//
// 两种形态（都要求严格相邻，不发明语义）：
//   - 后一条声明了 tool_calls 且没有正文 → 合并进前一条的 tool_calls；
//   - 后一条是纯文本、前一条是有 tool_calls 但没正文的 assistant → 正文并上去。
func mergeAdjacentToolCalls(msgs []openAIMessage) []openAIMessage {
	if len(msgs) < 2 {
		return msgs
	}
	out := make([]openAIMessage, 0, len(msgs))
	changed := false
	for _, m := range msgs {
		if m.Role == "assistant" && len(out) > 0 {
			prev := &out[len(out)-1]
			if prev.Role == "assistant" {
				// 形态一：本条只有 tool_calls，无正文
				if len(m.ToolCalls) > 0 && strings.TrimSpace(m.Text) == "" && len(prev.ToolCalls) > 0 {
					prev.ToolCalls = append(prev.ToolCalls, m.ToolCalls...)
					prev.ReasoningContent = joinReasoning(prev.ReasoningContent, m.ReasoningContent)
					changed = true
					continue
				}
				// 形态二：本条是纯文本，上一条有 tool_calls 但没正文
				if len(m.ToolCalls) == 0 && strings.TrimSpace(m.Text) != "" &&
					len(prev.ToolCalls) > 0 && strings.TrimSpace(prev.Text) == "" {
					prev.Text = m.Text
					prev.ReasoningContent = joinReasoning(prev.ReasoningContent, m.ReasoningContent)
					changed = true
					continue
				}
			}
		}
		out = append(out, m)
	}
	if !changed {
		return msgs
	}
	return out
}

// repackToolResultBlocks 保证 tool_calls 批次与它的结果相邻。
//
// 只重排、不改内容：把夹在结果之间（或结果之前）的非结果消息挪到整批之后，
// 结果的相对顺序不变。
func repackToolResultBlocks(msgs []openAIMessage) []openAIMessage {
	if len(msgs) < 3 {
		return msgs
	}
	out := make([]openAIMessage, 0, len(msgs))
	changed := false
	for i := 0; i < len(msgs); {
		m := msgs[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			out = append(out, m)
			i++
			continue
		}
		want := make(map[string]bool, len(m.ToolCalls))
		for _, c := range m.ToolCalls {
			if c.ID != "" {
				want[c.ID] = true
			}
		}
		out = append(out, m)
		i++
		var results, between []openAIMessage
		for i < len(msgs) {
			mm := msgs[i]
			if mm.Role == "tool" {
				if !want[mm.ToolCallID] {
					break
				}
				results = append(results, mm)
				i++
				continue
			}
			// 任何 assistant 消息都会结束这一批；其它角色是「插队的」，
			// 无论它在第一个结果之前还是两个结果之间，都挪到整批之后。
			if mm.Role == "assistant" {
				break
			}
			between = append(between, mm)
			i++
		}
		if len(results) > 0 {
			out = append(out, results...)
			out = append(out, between...)
			if len(between) > 0 {
				changed = true
			}
		} else {
			out = append(out, between...)
		}
	}
	if !changed {
		return msgs
	}
	return out
}
