package server

import (
	"strings"
	"testing"
)

// 工具声明要修成上游能接受的 schema：缺 function 对象、非法名字、重复名都处理掉。
func TestNormalizeToolDefs(t *testing.T) {
	tools := []map[string]any{
		// 正常形态（带类型别名与脏 required）
		{"type": "function", "function": map[string]any{
			"name": "read_file",
			"parameters": map[string]any{
				"type": "dict",
				"properties": map[string]any{
					"path": map[string]any{"type": "str"},
				},
				"required": []any{"path", nil, 42},
			},
		}},
		// 顶层 name 形态 → 包成 function
		{"name": "bash", "description": "run", "parameters": map[string]any{"type": "object"}},
		// 裸服务端工具 → 丢弃
		{"type": "web_search"},
		// 非法名字 → 丢弃
		{"type": "function", "function": map[string]any{"name": "读文件"}},
		// 重复名 → 只留一个
		{"type": "function", "function": map[string]any{"name": "read_file"}},
		// 名字超长 → 丢弃
		{"type": "function", "function": map[string]any{"name": strings.Repeat("a", 65)}},
	}
	out := normalizeToolDefs(tools)
	if len(out) != 2 {
		t.Fatalf("应剩 2 个工具，got %d: %+v", len(out), out)
	}
	names := make([]string, 0, len(out))
	for _, item := range out {
		if item["type"] != "function" {
			t.Fatalf("type 应为 function: %+v", item)
		}
		fn, ok := item["function"].(map[string]any)
		if !ok {
			t.Fatalf("缺 function 对象: %+v", item)
		}
		names = append(names, fn["name"].(string))
	}
	if strings.Join(names, ",") != "read_file,bash" {
		t.Fatalf("工具名=%v，want read_file,bash", names)
	}

	rf := out[0]["function"].(map[string]any)["parameters"].(map[string]any)
	if rf["type"] != "object" {
		t.Fatalf("parameters.type=%v，want object", rf["type"])
	}
	props := rf["properties"].(map[string]any)
	if props["path"].(map[string]any)["type"] != "string" {
		t.Fatalf("嵌套类型别名未修: %+v", props["path"])
	}
	if req, _ := rf["required"].([]any); len(req) != 1 || req[0] != "path" {
		t.Fatalf("required 未清理: %#v", rf["required"])
	}

	bash := out[1]["function"].(map[string]any)["parameters"].(map[string]any)
	if bash["type"] != "object" || bash["properties"] == nil {
		t.Fatalf("bash parameters=%+v", bash)
	}
}

// 没有 parameters 时补 {type:object, properties:{}}。
func TestNormalizeToolDefsFillsParameters(t *testing.T) {
	out := normalizeToolDefs([]map[string]any{{"type": "function", "function": map[string]any{"name": "noargs"}}})
	if len(out) != 1 {
		t.Fatalf("out=%+v", out)
	}
	params := out[0]["function"].(map[string]any)["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Fatalf("params=%+v", params)
	}
	if p, ok := params["properties"].(map[string]any); !ok || len(p) != 0 {
		t.Fatalf("properties 应为空对象: %+v", params["properties"])
	}
}

// 不改动调用方传入的数据。
func TestNormalizeToolDefsDoesNotMutateInput(t *testing.T) {
	fn := map[string]any{"name": "x"}
	tools := []map[string]any{{"type": "function", "function": fn}}
	_ = normalizeToolDefs(tools)
	if _, ok := fn["parameters"]; ok {
		t.Fatal("不应修改调用方的 function map")
	}
}

// 空输入 / 全部不可用时返回 nil（不生成空 tools 数组）。
func TestNormalizeToolDefsEmpty(t *testing.T) {
	if got := normalizeToolDefs(nil); got != nil {
		t.Fatalf("nil 输入应返回 nil，got %v", got)
	}
	if got := normalizeToolDefs([]map[string]any{{"type": "web_search"}}); got != nil {
		t.Fatalf("全部不可用时应返回 nil，got %v", got)
	}
}
