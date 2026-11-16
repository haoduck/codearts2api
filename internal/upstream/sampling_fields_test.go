package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// captureChatBody 起一个假的聊天端点，返回它收到的请求体。
func captureChatBody(t *testing.T, opts ChatOptions) map[string]any {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewWithChatEndpoint(5*time.Second, srv.URL)
	cred := SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "st"}
	rc, err := c.ChatStreamWithOptions(context.Background(), "0123456789abcdef0123456789abcdef",
		[]ChatMessage{{Role: "user", Content: "hi"}}, "", cred, "user", "GLM-5.2", opts, false)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	return body
}

// 采样/格式类参数要原样透传给上游（此前被网关静默丢弃）。
func TestChatForwardsSamplingFields(t *testing.T) {
	fp, pp := 0.5, 0.25
	seed, topK := 42, 20
	parallel := false
	body := captureChatBody(t, ChatOptions{
		Stop:              []string{"\n\n", "END"},
		FrequencyPenalty:  &fp,
		PresencePenalty:   &pp,
		Seed:              &seed,
		ResponseFormat:    map[string]any{"type": "json_object"},
		TopK:              &topK,
		ParallelToolCalls: &parallel,
	})

	if stops, _ := body["stop"].([]any); len(stops) != 2 || stops[0] != "\n\n" {
		t.Fatalf("stop 未透传: %#v", body["stop"])
	}
	if body["frequency_penalty"] != 0.5 || body["presence_penalty"] != 0.25 {
		t.Fatalf("penalty 未透传: %#v", body)
	}
	if body["seed"] != float64(42) || body["top_k"] != float64(20) {
		t.Fatalf("seed/top_k 未透传: %#v", body)
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls 未透传: %#v", body["parallel_tool_calls"])
	}
	rf, _ := body["response_format"].(map[string]any)
	if rf["type"] != "json_object" {
		t.Fatalf("response_format 未透传: %#v", body["response_format"])
	}
}

// 未设置时不发这些字段（保持上游默认行为，不凭空塞值）。
func TestChatOmitsUnsetSamplingFields(t *testing.T) {
	body := captureChatBody(t, ChatOptions{})
	for _, key := range []string{
		"stop", "frequency_penalty", "presence_penalty", "seed",
		"response_format", "top_k", "parallel_tool_calls",
	} {
		if v, ok := body[key]; ok {
			t.Fatalf("%s 未设置时不应发送: %#v", key, v)
		}
	}
}
