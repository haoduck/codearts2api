package upstream

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/auth"
)

// 1x1 红色 PNG，用来验证上游是否接受 image_url 多模态输入。
const tinyRedPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestLiveImageInput 真实上游验证：/api/v2/chat/completions 是否接受 OpenAI 风格的
// content 数组（text + image_url）。
//
//	CA2A_LIVE_IMAGE=1 go test ./internal/upstream -run TestLiveImageInput -v
func TestLiveImageInput(t *testing.T) {
	if os.Getenv("CA2A_LIVE_IMAGE") != "1" {
		t.Skip("设置 CA2A_LIVE_IMAGE=1 才跑真实上游")
	}
	auths, err := auth.LoadDir("../../auths")
	if err != nil || len(auths) == 0 {
		t.Fatalf("load auths: %v", err)
	}
	var acct *auth.Auth
	for _, a := range auths {
		if !a.Expired() {
			acct = a
			break
		}
	}
	if acct == nil {
		t.Fatal("没有未过期的账号")
	}
	token, ak, sk := acct.Credentials()
	cred := SignCredential{AccessKeyID: ak, SecretAccessKey: sk, SecurityToken: token}

	parts := []any{
		map[string]any{"type": "text", "text": "这张图是什么颜色？只回答颜色词。"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": tinyRedPNG}},
	}
	c := New(90 * time.Second)
	for _, model := range []string{"Qwen3-VL-235B", "kimi-k2.6-vl", "GLM-5.2"} {
		rc, err := c.ChatStreamWithOptions(context.Background(), randomHexLower(16),
			[]ChatMessage{{Role: "user", Content: parts}}, "", cred, acct.UserName, model, ChatOptions{}, false)
		if err != nil {
			t.Logf("%-16s 失败: %v", model, truncateStr(err.Error(), 160))
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		text := string(raw)
		if strings.Contains(text, "error_code") && !strings.Contains(text, `"error_code":"0"`) {
			t.Logf("%-16s 内嵌错误: %s", model, truncateStr(text, 200))
			continue
		}
		t.Logf("%-16s ✔ 接受图片输入，响应片段: %s", model, truncateStr(text, 200))
	}
}

// 让编译器知道 errors 被用到（与 live_ot_session_test.go 共用包）。
var _ = errors.Is
