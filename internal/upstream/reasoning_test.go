package upstream

import (
	"strings"
	"testing"
)

// reasoning_effort 按模型校验：非法档位 400，关不掉思考的模型回退到第一个档位。
func TestNormalizeReasoningEffort(t *testing.T) {
	cases := []struct {
		model, in, want string
		wantErr         bool
	}{
		{"GLM-5.2", "", "", false},
		{"GLM-5.2", "high", "high", false},
		{"GLM-5.2", " HIGH ", "high", false},
		{"glm-5.2", "minimal", "minimal", false},
		// 始终思考的模型：none 关不掉，回退第一个可用档位
		{"glm-5.3-flash", "none", "low", false},
		{"glm-5.3-flash", "medium", "", true},
		{"glm-5.3-flash", "max", "max", false},
		// 未知模型不做限制（原样透传）
		{"some-new-model", "weird", "weird", false},
	}
	for _, c := range cases {
		got, err := NormalizeReasoningEffort(c.model, c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s/%q err=%v，wantErr=%v", c.model, c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("%s/%q => %q，want %q", c.model, c.in, got, c.want)
		}
	}
}

// 报错信息要列出允许的档位，便于客户端自查。
func TestNormalizeReasoningEffortErrorMentionsAllowed(t *testing.T) {
	_, err := NormalizeReasoningEffort("glm-5.3-flash", "medium")
	if err == nil {
		t.Fatal("应当报错")
	}
	if msg := err.Error(); !strings.Contains(msg, "low/high/max") {
		t.Fatalf("错误信息未列出允许档位: %s", msg)
	}
}
