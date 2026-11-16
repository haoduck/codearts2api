package upstream

import (
	"errors"
	"testing"
)

func TestIsSessionBrokenError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"code only", errors.New("HISTORY_LOAD_FAILED"), true},
		{"upstream stream error", &UpstreamError{Code: "HISTORY_LOAD_FAILED", Msg: "历史文件存在异常，会话暂不可用，请新建任务"}, true},
		{"wrapped in api error", &ApiError{Status: 502, Message: `{"error_code":"HISTORY_LOAD_FAILED"}`}, true},
		{"chinese message", errors.New("会话暂不可用，请新建任务"), true},
		{"concurrent limit is not session broken", &ApiError{Status: 400, Message: "TM.00001041 并发会话数已达上限"}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSessionBrokenError(c.err); got != c.want {
				t.Fatalf("IsSessionBrokenError(%v)=%v want %v", c.err, got, c.want)
			}
		})
	}
}
