package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// max_tokens 超过上游硬上限时必须被钳制：InferHub 对超限值统一回
// InferHub.001001005.400 The request param is invalid（不提示具体字段）。
func TestChatBodyClampsMaxTokensToUpstreamLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int
		want int
	}{
		{"under limit", 4096, 4096},
		{"exactly at limit", MaxTokensLimit, MaxTokensLimit},
		{"just above limit", MaxTokensLimit + 1, MaxTokensLimit},
		{"client default", 131072, MaxTokensLimit},
		{"absurd", 1 << 30, MaxTokensLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := chatBodyV2("chat", []ChatMessage{{Role: "user", Content: "hi"}}, "glm-5.2",
				ChatOptions{MaxTokens: &tc.in})
			if got := body["max_tokens"]; got != tc.want {
				t.Fatalf("max_tokens=%v want %d", got, tc.want)
			}
		})
	}
}

// 上游对 top_p=0 返回 InferHub.001001028.406 top_p cannot be zero，
// 而 top_p=0 在 OpenAI 语义里就是「不限制」，因此不应下发该字段。
func TestChatBodyOmitsZeroTopP(t *testing.T) {
	zero := 0.0
	body := chatBodyV2("chat", []ChatMessage{{Role: "user", Content: "hi"}}, "glm-5.2",
		ChatOptions{TopP: &zero})
	if _, ok := body["top_p"]; ok {
		t.Fatalf("top_p=0 must be omitted, got %v", body["top_p"])
	}

	half := 0.5
	body = chatBodyV2("chat", []ChatMessage{{Role: "user", Content: "hi"}}, "glm-5.2",
		ChatOptions{TopP: &half})
	if body["top_p"] != 0.5 {
		t.Fatalf("valid top_p must be forwarded, got %v", body["top_p"])
	}
}

// 端到端确认发往上游的 body 确实已钳制。
func TestSendChatV2ClampsMaxTokensOnTheWire(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewWithChatEndpoint(5*time.Second, srv.URL)
	body := map[string]any{
		"model": "GLM-5.2", "stream": true, "max_tokens": 1 << 20,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	// chatBodyV2 是服务层的唯一出口，直接对其钳制后的结果发请求验证链路。
	body["max_tokens"] = ClampMaxTokens(body["max_tokens"].(int))
	rc, err := c.SendChatV2(context.Background(), body, "trace",
		SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "token"}, "token", false)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	_, _ = io.Copy(io.Discard, rc)
	rc.Close()
	if got["max_tokens"] != float64(MaxTokensLimit) {
		t.Fatalf("upstream received max_tokens=%v want %d", got["max_tokens"], MaxTokensLimit)
	}
}

func TestClampMaxTokensKeepsValidValues(t *testing.T) {
	for _, v := range []int{1, 1024, MaxTokensLimit} {
		if got := ClampMaxTokens(v); got != v {
			t.Fatalf("ClampMaxTokens(%d)=%d, must stay unchanged", v, got)
		}
	}
}

func TestSendChatV2SurfacesEmbeddedParamInvalidAsBadRequest(t *testing.T) {
	for _, payload := range []string{
		`{"error_code":"InferHub.001001005.400","error_msg":"invalid arguments"}`,
		`{"error_code":"400","error_msg":"The request param is invalid, Please check it"}`,
	} {
		t.Run(payload, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+payload+"\n\n")
			}))
			defer backend.Close()
			c := NewWithChatEndpoint(time.Second, backend.URL)
			_, err := c.SendChatV2(context.Background(), map[string]any{"model": "GLM-5.2"}, "", SignCredential{}, "", false)
			var apiErr *ApiError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest || !strings.Contains(apiErr.Message, "invalid") {
				t.Fatalf("parameter error was treated as an upstream failure: %T %v", err, err)
			}
		})
	}
}
