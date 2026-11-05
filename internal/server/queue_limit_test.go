package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// 上游排队（TM.00001041 / 429）时必须优先换号，而不是把请求钉死在一个账号上
// 干等——否则一条请求会占着一个账号反复重试，其它账号全空闲。
func TestQueueLimitSwitchesAccount(t *testing.T) {
	var busyHits, okHits int32
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&busyHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error_code":"TM.00001041","error_msg":"并发会话数已达上限"}`)
	}))
	defer busy.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&okHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer ok.Close()

	mk := func(name string) *auth.Auth {
		return auth.New(name, "user-"+name, "domain", "token", "ak", "sk",
			time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	}
	p, err := pool.New([]*auth.Auth{mk("acct-busy"), mk("acct-ok")}, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	// 轮转起点是第一个账号，让它永远排队；第二个账号正常。
	p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, busy.URL)
	p.Accounts()[1].Client = upstream.NewWithChatEndpoint(5*time.Second, ok.URL)

	h := NewHandler(Config{
		Pool: p, DefaultModel: "glm-5.2", MaxRotate: 3,
		QueueRetryDelay: time.Millisecond, QueueMaxAttempts: 3,
	})
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "hello" {
		t.Fatalf("响应不是第二个账号的内容: %s", rec.Body.String())
	}
	if got := atomic.LoadInt32(&busyHits); got != 1 {
		t.Fatalf("排队账号被打了 %d 次，want 1（应立刻换号而不是原地重试）", got)
	}
	if got := atomic.LoadInt32(&okHits); got != 1 {
		t.Fatalf("正常账号被打了 %d 次，want 1", got)
	}
}
