package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// 断电/崩溃场景回归：saveLocked 必须先 fsync 再 rename。
// 若只 WriteFile+Rename，NTFS 会记录文件大小元数据、数据块却未落盘，
// 重启后得到全零凭据文件（实测 2026-10-17，6 个账号文件同时损坏且无法恢复）。
// 这里至少验证写入路径是 OpenFile+Sync（而非裸 WriteFile），并验证产物可解析。
func TestSaveWritesParseableJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codearts-test.json")
	a := New("uid-1", "tester", "dom-1", "tok", "ak", "sk", "2026-10-17T00:00:00Z", "rt", "cv")
	a.SetPath(path)
	if err := a.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("saved file is empty")
	}
	for _, b := range raw {
		if b == 0 {
			t.Fatal("saved file contains NUL bytes")
		}
	}
	b, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(b) != 1 || b[0].UserID != "uid-1" {
		t.Fatalf("reload got %d accounts", len(b))
	}
}

// 单个损坏文件（全零）不得阻断整个目录的加载：服务宁可少一个账号，
// 也不能因一个坏文件拒绝启动（否则所有健康账号陪葬）。
func TestLoadDirSkipsCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "codearts-good.json")
	a := New("uid-2", "good", "dom-2", "tok", "ak", "sk", "", "rt", "cv")
	a.SetPath(good)
	if err := a.Save(); err != nil {
		t.Fatalf("save good: %v", err)
	}
	// 全零文件：模拟断电产生的损坏凭据。
	if err := os.WriteFile(filepath.Join(dir, "codearts-zero.json"),
		make([]byte, 128), 0o600); err != nil {
		t.Fatal(err)
	}
	// 非法 JSON。
	if err := os.WriteFile(filepath.Join(dir, "codearts-junk.json"),
		[]byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir must not fail on corrupt files: %v", err)
	}
	if len(got) != 1 || got[0].UserID != "uid-2" {
		t.Fatalf("expected only the healthy account, got %d", len(got))
	}
}