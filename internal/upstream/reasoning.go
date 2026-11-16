package upstream

import (
	"fmt"
	"strings"
)

// reasoningEfforts 各模型可接受的 reasoning_effort 档位。
//
// 来源：上游对非法档位会回一条带允许列表的校验错误；这里按模型登记，
// 与 workbuddy2api-hub 的 REASONING_EFFORTS 一致。模型不在表里时不做限制
// （原样透传），避免凭空收紧客户端的合法取值。
var reasoningEfforts = map[string][]string{
	"deepseek-v4.1-flash":      {"none", "low", "medium", "high", "xhigh", "max"},
	"deepseek-v4.1-flash-1017": {"none", "low", "medium", "high", "xhigh", "max"},
	"glm-5.2":                  {"none", "minimal", "low", "medium", "high", "xhigh", "max"},
	"glm-5.2-sft-harmony":      {"none", "minimal", "low", "medium", "high", "xhigh", "max"},
	"glm-5.3":                  {"none", "minimal", "low", "medium", "high", "xhigh", "max"},
	// 始终思考的模型：上游只接受 low/high/max，"none" 关不掉思考。
	"glm-5.3-flash": {"low", "high", "max"},
}

// NormalizeReasoningEffort 按模型校验 reasoning_effort。
//
//   - 空值：原样返回（不传该字段）。
//   - 模型不在表里：原样返回，不做限制。
//   - 档位非法：若请求的是 "none" 而该模型关不掉思考，回退到第一个可用档位
//     （不因为一个可选参数把整条请求判死）；其余非法值返回错误，由调用方回 400。
func NormalizeReasoningEffort(model, effort string) (string, error) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return "", nil
	}
	allowed := reasoningEfforts[strings.ToLower(CanonicalModel(model))]
	if len(allowed) == 0 {
		return effort, nil
	}
	for _, v := range allowed {
		if v == effort {
			return effort, nil
		}
	}
	if effort == "none" {
		return allowed[0], nil
	}
	return "", fmt.Errorf("reasoning_effort %q is not supported by %s (allowed: %s)",
		effort, model, strings.Join(allowed, "/"))
}
