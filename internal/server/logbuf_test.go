package server

import (
	"bytes"
	"fmt"
	"testing"
)

// 首次打开（after=0）应当拿到**最新**的 N 行，而不是缓冲里最旧的 N 行。
func TestLogRingFirstSnapshotReturnsNewest(t *testing.T) {
	r := &logRing{}
	for i := 1; i <= 20; i++ {
		r.add(fmt.Sprintf("line-%d", i))
	}
	lines, next := r.snapshot(0, 5)
	if len(lines) != 5 {
		t.Fatalf("返回 %d 行，want 5", len(lines))
	}
	if lines[0].Line != "line-16" || lines[4].Line != "line-20" {
		t.Fatalf("首次拉取内容=%q..%q，want line-16..line-20", lines[0].Line, lines[4].Line)
	}
	if next != lines[len(lines)-1].Seq {
		t.Fatalf("next=%d，want 最后一行游标 %d", next, lines[len(lines)-1].Seq)
	}
}

// 增量拉取：只返回游标之后的新行；游标已最新时返回空且游标不变。
func TestLogRingSnapshotIncremental(t *testing.T) {
	r := &logRing{}
	r.add("a")
	r.add("b")
	lines, next := r.snapshot(0, 10)
	if len(lines) != 2 || next != 2 {
		t.Fatalf("lines=%d next=%d，want 2/2", len(lines), next)
	}
	r.add("c")
	lines, next = r.snapshot(next, 10)
	if len(lines) != 1 || lines[0].Line != "c" || next != 3 {
		t.Fatalf("增量 lines=%v next=%d，want [c]/3", lines, next)
	}
	if l, n := r.snapshot(next, 10); len(l) != 0 || n != next {
		t.Fatalf("游标已最新时应返回空，got lines=%d next=%d", len(l), n)
	}
}

// 超过容量时丢最旧的，游标继续单调递增（前端不会因为回收而重复/漏行）。
func TestLogRingEvictsOldest(t *testing.T) {
	r := &logRing{}
	total := logRingCapacity + 37
	for i := 1; i <= total; i++ {
		r.add(fmt.Sprintf("l%d", i))
	}
	lines, next := r.snapshot(0, logRingCapacity)
	if len(lines) != logRingCapacity {
		t.Fatalf("返回 %d 行，want %d", len(lines), logRingCapacity)
	}
	if uint64(total) != next {
		t.Fatalf("next=%d，want %d", next, total)
	}
	if want := fmt.Sprintf("l%d", total-logRingCapacity+1); lines[0].Line != want {
		t.Fatalf("最旧一行=%q，want %q", lines[0].Line, want)
	}
	// 游标比缓冲里最旧的还旧（那段已被挤掉）：不能报错，直接从现存最旧的一行
	// 开始返回，前端最多多轮就追平。
	stale, staleNext := r.snapshot(1, 10)
	if len(stale) != 10 || stale[0].Line != lines[0].Line {
		t.Fatalf("过期游标应从现存最旧行开始，got %d 行 首行=%q", len(stale), stale[0].Line)
	}
	if staleNext != stale[len(stale)-1].Seq {
		t.Fatalf("过期游标 next=%d，want 最后一行游标 %d", staleNext, stale[len(stale)-1].Seq)
	}
}

// teeWriter 必须原样透传给原输出（容器日志不能丢），同时镜像进缓冲。
func TestTeeWriterMirrorsAndPassesThrough(t *testing.T) {
	r := &logRing{}
	var out bytes.Buffer
	w := teeWriter{out: &out, ring: r}
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("world\r\n")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello\nworld\r\n" {
		t.Fatalf("透传内容=%q", out.String())
	}
	lines, _ := r.snapshot(0, 10)
	if len(lines) != 2 || lines[0].Line != "hello" || lines[1].Line != "world" {
		t.Fatalf("镜像内容=%v，want [hello world]", lines)
	}
}
