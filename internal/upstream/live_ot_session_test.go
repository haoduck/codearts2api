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

// TestLiveOTSessionQueueLimit 真实上游 A/B：对**同一个账号**连续发多条请求，
// 对比「不带 x-ot-session-id」与「带」时上游并发会话上限（TM.00001041）的差异。
//
// 只在显式开启时运行（默认跳过，避免 CI 打真实上游）：
//
//	CA2A_LIVE_OT=1 go test ./internal/upstream -run TestLiveOTSessionQueueLimit -v
//
// 需要 ../../auths 下有可用账号。
func TestLiveOTSessionQueueLimit(t *testing.T) {
	if os.Getenv("CA2A_LIVE_OT") != "1" {
		t.Skip("设置 CA2A_LIVE_OT=1 才跑真实上游")
	}
	auths, err := auth.LoadDir("../../auths")
	if err != nil || len(auths) == 0 {
		t.Fatalf("load auths: %v (n=%d)", err, len(auths))
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
	t.Logf("账号=%s uid=%s remaining=%s ot=%s",
		acct.UserName, acct.UserID, acct.Remaining().Round(time.Minute), OTSessionIDFor(acct.UserID))

	// 让上游把之前请求占用的会话槽位释放掉（实测残留约 60 秒）。
	t.Log("等待 65s 让上游会话槽位释放…")
	time.Sleep(65 * time.Second)

	const rounds = 6
	c := New(90 * time.Second)
	run := func(label, otSession string) (ok, queued int, other []string) {
		for i := 0; i < rounds; i++ {
			rc, err := c.ChatStreamWithOptions(context.Background(), randomHexLower(16),
				[]ChatMessage{{Role: "user", Content: "用两个字回答：在吗"}},
				"", cred, acct.UserName, "GLM-5.2", ChatOptions{OTSessionID: otSession}, false)
			if err != nil {
				if isQueueLimited(err) {
					queued++
				} else {
					other = append(other, truncateStr(err.Error(), 120))
				}
			} else {
				_, _ = io.Copy(io.Discard, rc)
				rc.Close()
				ok++
			}
			time.Sleep(time.Second)
		}
		t.Logf("%s: ok=%d queued=%d other=%d %v", label, ok, queued, len(other), other)
		return
	}

	okA, qA, _ := run("A 不带 x-ot-session-id", "")
	time.Sleep(65 * time.Second)
	okB, qB, _ := run("B 带 x-ot-session-id", OTSessionIDFor(acct.UserID))

	t.Logf("结论：A(无头) ok=%d/%d 排队=%d ； B(有头) ok=%d/%d 排队=%d", okA, rounds, qA, okB, rounds, qB)
	if qA == 0 && qB == 0 {
		t.Skip("两种模式都没触发排队，本次无法对比（账号会话槽位可能已被其它请求占用或上游放宽）")
	}
	if qA > 0 && qB == 0 {
		t.Logf("✔ 会话统计头有效：无头时排队 %d 次，带头后 0 次", qA)
	}
}

// isQueueLimited 判断是否为上游并发会话上限错误。
func isQueueLimited(err error) bool {
	if err == nil {
		return false
	}
	var ae *ApiError
	if errors.As(err, &ae) && ae.Status == 429 {
		return true
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "tm.00001041") ||
		strings.Contains(low, "并发会话") ||
		strings.Contains(low, "concurrent session") ||
		strings.Contains(low, "maximum number of active")
}
