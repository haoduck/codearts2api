package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

func TestChatCompletionsUsage(t *testing.T) {
	const answer = "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\",\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n"
	const nativeUsage = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30,\"completion_tokens_details\":{\"reasoning_tokens\":8}}}\n\n"
	const done = "data: [DONE]\n\n"
	estimate := `{"prompt_tokens":1,"completion_tokens":3,"total_tokens":4}`
	exact := `{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"completion_tokens_details":{"reasoning_tokens":8}}`
	for _, tc := range []struct {
		name, options, wire, want string
		stream                    bool
	}{
		{"estimate", `,"stream_options":{"include_usage":true}`, answer + done, estimate, true},
		{"native", `,"stream_options":{"include_usage":true}`, answer + nativeUsage + done, exact, true},
		{"omitted", "", answer + nativeUsage + done, "", true},
		{"disabled", `,"stream_options":{"include_usage":false}`, answer + nativeUsage + done, "", true},
		{"null", `,"stream_options":null`, answer + done, "", true},
		{"nonstream_estimate", "", answer + done, estimate, false},
		{"nonstream_native", "", answer + nativeUsage + done, exact, false},
		{"legacy_done", `,"stream_options":{"include_usage":true}`, "event: onAnswer\ndata: {\"text\":\"OK\"}\n\nevent: done\ndata: {\"error_code\":\"0\"}\n\n", `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`, true},
		{"eof", `,"stream_options":{"include_usage":true}`, answer, estimate, true},
		{"error", `,"stream_options":{"include_usage":true}`, answer + "event: done\ndata: {\"error_code\":\"failed\",\"error_msg\":\"failed\"}\n\n", "", true},
		{"tools", `,"stream_options":{"include_usage":true}`, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"function\":{\"name\":\"test\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" + nativeUsage + done, exact, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.wire)
			}))
			defer up.Close()
			a := auth.New("usage-account", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
			p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
			if err != nil {
				t.Fatal(err)
			}
			p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, up.URL)
			h := NewHandler(Config{Pool: p, DefaultModel: "glm-5.2", MaxRotate: 1})
			stream := "false"
			if tc.stream {
				stream = "true"
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":`+stream+`,"messages":[{"role":"user","content":"hi"}]`+tc.options+`}`))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var got map[string]any
			if !tc.stream {
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				got, _ = body["usage"].(map[string]any)
			} else {
				data := strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n")
				if data[len(data)-1] != "data: [DONE]" {
					t.Fatalf("missing DONE: %s", rec.Body.String())
				}
				usageCount := 0
				var id any
				for i, event := range data[:len(data)-1] {
					if strings.HasPrefix(event, "event: error") {
						continue
					}
					var chunk map[string]any
					if err := json.Unmarshal([]byte(strings.TrimPrefix(event, "data: ")), &chunk); err != nil {
						t.Fatal(err)
					}
					if id == nil {
						id = chunk["id"]
					}
					if chunk["id"] != id || chunk["model"] != "glm-5.2" || chunk["object"] != "chat.completion.chunk" {
						t.Fatalf("bad metadata: %#v", chunk)
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usageCount++
						got = u
						choices, ok := chunk["choices"].([]any)
						if !ok || len(choices) != 0 || i != len(data)-2 {
							t.Fatalf("usage must be empty-choices chunk before DONE: %#v", chunk)
						}
					} else if tc.want != "" {
						if value, exists := chunk["usage"]; !exists || value != nil {
							t.Fatalf("missing null usage: %#v", chunk)
						}
					}
					if tc.options == "" || strings.Contains(tc.options, "false") || strings.Contains(tc.options, "null") {
						if _, ok := chunk["usage"]; ok {
							t.Fatalf("unexpected usage: %#v", chunk)
						}
					}
				}
				if usageCount > 1 {
					t.Fatalf("duplicate usage chunks: %d", usageCount)
				}
				if tc.name == "error" && !strings.Contains(rec.Body.String(), "event: error") {
					t.Fatal("missing error event")
				}
				if tc.name == "tools" && !strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
					t.Fatal("missing tool finish")
				}
			}
			var want map[string]any
			if tc.want != "" {
				if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("usage=%#v want=%#v; body=%s", got, want, rec.Body.String())
			}
		})
	}
}

// 用量按 天×账号×模型 聚合，并能在重启后从磁盘恢复。
func TestUsageStoreAggregatesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s := newUsageStore(path)
	s.now = func() time.Time { return time.Date(2026, 11, 4, 10, 0, 0, 0, time.Local) }

	s.add("acct-a", "GLM-5.2", usageCounts{Requests: 1, PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150})
	s.add("acct-a", "GLM-5.2", usageCounts{Requests: 1, PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15})
	s.add("acct-b", "glm-5.3", usageCounts{Requests: 1, PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10})

	rep := s.snapshot(7)
	if rep.Total.Requests != 3 || rep.Total.TotalTokens != 175 {
		t.Fatalf("total=%+v，want 3 请求 / 175 token", rep.Total)
	}
	if rep.Today.TotalTokens != 175 {
		t.Fatalf("today=%+v，want 175", rep.Today)
	}
	if len(rep.ByAccount) != 2 || rep.ByAccount[0].Key != "acct-a" || rep.ByAccount[0].TotalTokens != 165 {
		t.Fatalf("by_account=%+v（应按 total_tokens 降序）", rep.ByAccount)
	}
	if len(rep.ByModel) != 2 || rep.ByModel[0].Key != "GLM-5.2" {
		t.Fatalf("by_model=%+v", rep.ByModel)
	}

	s.flush()
	s2 := newUsageStore(path)
	s2.now = s.now
	if got := s2.snapshot(7).Total.TotalTokens; got != 175 {
		t.Fatalf("重新加载后 total=%d，want 175", got)
	}
}

// 时间窗口：days 只统计最近 N 天（含今天）。
func TestUsageStoreWindow(t *testing.T) {
	s := newUsageStore(filepath.Join(t.TempDir(), "usage.json"))
	day := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return day }
	s.add("a", "m", usageCounts{Requests: 1, TotalTokens: 100})
	s.now = func() time.Time { return day.AddDate(0, 0, -10) }
	s.add("a", "m", usageCounts{Requests: 1, TotalTokens: 900})
	s.now = func() time.Time { return day }

	if got := s.snapshot(7).Total.TotalTokens; got != 100 {
		t.Fatalf("7 天窗口=%d，want 100（10 天前的不算）", got)
	}
	if got := s.snapshot(30).Total.TotalTokens; got != 1000 {
		t.Fatalf("30 天窗口=%d，want 1000", got)
	}
}

// 上游 usage 的字段解析：float64 / int 两种来源都要吃得下。
func TestUsageCountsFromMap(t *testing.T) {
	c := usageCountsFromMap(map[string]any{
		"prompt_tokens": float64(12), "completion_tokens": float64(34), "total_tokens": float64(46),
		"completion_tokens_details": map[string]any{"reasoning_tokens": float64(20)},
	})
	if c.PromptTokens != 12 || c.CompletionTokens != 34 || c.TotalTokens != 46 || c.ReasoningTokens != 20 {
		t.Fatalf("counts=%+v", c)
	}
	if c := usageCountsFromMap(map[string]any{"prompt_tokens": 5, "completion_tokens": 6}); c.TotalTokens != 11 {
		t.Fatalf("缺 total 时应相加，got %d", c.TotalTokens)
	}
}

