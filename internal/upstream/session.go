package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

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

// OTSessionEnabled 默认开启；设 CA2A_OT_SESSION=0 可退回旧行为（便于对照）。
func OTSessionEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CA2A_OT_SESSION"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// OTSessionIDFor 返回**账号级稳定**的会话 id（同一账号永远同一个，重启/续期不变）。
//
// 网关按 x-ot-session-id 统计「每账号的并发会话数」：
//   - 不发这个头时，每个请求都算一个新会话，会话结束后还要占位约 60 秒，
//     单账号 3 个名额很快被自己的历史请求占满 → TM.00001041 排队；
//   - Chat-Id / Session-Id 不参与这个计数。
//
// 因此这里给每个账号一个固定值，让网关把同一账号的所有请求算作同一个会话。
// 该头必须在**签名之后**追加（不进入 SignedHeaders）。
func OTSessionIDFor(accountUID string) string {
	if !OTSessionEnabled() || accountUID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("codearts-ot-session:" + accountUID))
	return hex.EncodeToString(sum[:16])
}

