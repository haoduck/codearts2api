package upstream

import "strings"

// IsSessionBrokenError 判断是否为「上游会话历史损坏」类错误。
//
// 上游把会话历史按 chat_id 存成服务端文件，文件不一致/损坏时返回
// HISTORY_LOAD_FAILED（error_msg：「历史文件存在异常，会话暂不可用，请新建任务」）。
//
// 这是**会话级**故障，不是账号故障：
//   - 换一个新的 chat_id（新会话）就能恢复——客户端本来每轮都重放完整历史；
//   - 绝不能因此冷却/禁用账号（曾把健康账号记错误、累计到阈值冷却 10 分钟）。
func IsSessionBrokenError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, key := range []string{
		"HISTORY_LOAD_FAILED",
		"历史文件存在异常",
		"会话暂不可用",
	} {
		if strings.Contains(msg, key) {
			return true
		}
	}
	return false
}
