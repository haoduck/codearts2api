package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// chat_id 必须按「账号 + 代数」隔离：上游会话历史是按 chat_id 存的服务端文件，
// 同一 chat_id 在多个账号间漂移会各留一份不一致的历史（HISTORY_LOAD_FAILED）。
func TestScopedChatIDIsolatesAccountAndGeneration(t *testing.T) {
	const conv = "conv-abc"
	a := scopedChatID(conv, "acct-A", 0)
	b := scopedChatID(conv, "acct-B", 0)
	if a == b {
		t.Fatal("不同账号必须得到不同 chat_id")
	}
	if len(a) != 32 {
		t.Fatalf("chat_id 必须是 32 位十六进制，got %q", a)
	}
	if got := scopedChatID(conv, "acct-A", 0); got != a {
		t.Fatalf("同账号同代数必须稳定: %q vs %q", got, a)
	}
	if got := scopedChatID(conv, "acct-A", 1); got == a {
		t.Fatal("代数 +1 必须换新的 chat_id")
	}
	if got := scopedChatID("", "acct-A", 0); len(got) != 32 {
		t.Fatalf("空会话应给随机 32 位 id，got %q", got)
	}
}

// 上游会话历史损坏（HISTORY_LOAD_FAILED）时必须：
//  1. 换一个新 chat_id（新会话）重试，而不是把错误直接甩给客户端；
//  2. 不冷却、不记账号错误（账号是健康的，坏的是会话历史文件）。
func TestSessionBrokenErrorRetriesWithNewChatID(t *testing.T) {
	var mu sync.Mutex
	var chatIDs []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id, _ := body["chat_id"].(string)
		mu.Lock()
		chatIDs = append(chatIDs, id)
		first := len(chatIDs) == 1
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if first {
			_, _ = io.WriteString(w, "event: done\ndata: {\"error_code\":\"HISTORY_LOAD_FAILED\",\"error_msg\":\"历史文件存在异常，会话暂不可用，请新建任务\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()

	a := auth.New("acct-1", "tester", "domain", "token", "ak", "sk",
		time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, up.URL)

	h := NewHandler(Config{Pool: p, DefaultModel: "glm-5.2", MaxRotate: 3})
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}],"stream":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "recovered") {
		t.Fatalf("没有从新会话恢复: %s", rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(chatIDs) != 2 {
		t.Fatalf("上游被调用 %d 次，want 2（第二次应换新会话）", len(chatIDs))
	}
	if chatIDs[0] == chatIDs[1] {
		t.Fatalf("重试复用了同一个 chat_id=%s", chatIDs[0])
	}

	st := p.List()[0]
	if cooling, _ := st["cooling"].(bool); cooling {
		t.Fatalf("会话级错误不应冷却账号: %#v", st)
	}
	if ec, _ := st["err_count"].(int); ec != 0 {
		t.Fatalf("会话级错误不应记账号错误: %#v", st)
	}
}
