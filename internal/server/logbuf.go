// 运行日志环形缓冲：把标准库 log 的输出同时收进内存，供面板/接口查看。
//
// 为什么需要：本服务所有运行信息（token 续期、账号冷却、上游错误、探测结论）
// 都只走 log.Printf → 容器 stdout，用户必须 `docker logs` 才能看到。排查
// 「模型只回一句问候语」这类问题时，没有日志等于瞎猜。
//
// 这里只做内存镜像，不落盘、不改变 docker logs 的行为。
package server

import (
	"io"
	"log"
	"os"
	"sync"
)

// logRingCapacity 内存里保留的日志行数（约几百 KB）。
const logRingCapacity = 1000

// logEntry 一行日志 + 单调递增游标，前端据此增量拉取。
type logEntry struct {
	Seq  uint64 `json:"seq"`
	Line string `json:"line"`
}

// logRing 定长环形缓冲（超过容量丢最旧的）。
type logRing struct {
	mu      sync.Mutex
	seq     uint64
	entries []logEntry
}

func (r *logRing) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.entries = append(r.entries, logEntry{Seq: r.seq, Line: line})
	if over := len(r.entries) - logRingCapacity; over > 0 {
		// 一次多丢一些，避免每行都搬整个切片。
		r.entries = append([]logEntry(nil), r.entries[over:]...)
	}
}

// snapshot 取日志。
//
// after == 0 表示「首次打开」：返回**最新的** limit 行（而不是最旧的），
// 这样面板一打开看到的就是刚发生的事。after > 0 时按游标增量返回。
// next 是最后一条返回行的游标，没有返回行时等于当前最新游标。
func (r *logRing) snapshot(after uint64, limit int) ([]logEntry, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	latest := r.seq
	if len(r.entries) == 0 {
		return nil, latest
	}
	if after == 0 {
		start := len(r.entries) - limit
		if limit <= 0 || start < 0 {
			start = 0
		}
		out := make([]logEntry, len(r.entries)-start)
		copy(out, r.entries[start:])
		return out, out[len(out)-1].Seq
	}
	if after >= latest {
		return nil, latest
	}
	start := 0
	for start < len(r.entries) && r.entries[start].Seq <= after {
		start++
	}
	if start >= len(r.entries) {
		// 游标太旧：请求的那段已经被环形缓冲挤掉了。
		return nil, latest
	}
	end := len(r.entries)
	if limit > 0 && end-start > limit {
		end = start + limit
	}
	out := make([]logEntry, end-start)
	copy(out, r.entries[start:end])
	return out, out[len(out)-1].Seq
}

// logSink 进程级日志镜像。
var (
	logSink   = &logRing{}
	logTeeOne sync.Once
)

// teeWriter 先写原输出（容器 stdout/stderr），再镜像到环形缓冲。
type teeWriter struct {
	out io.Writer
	ring *logRing
}

func (w teeWriter) Write(p []byte) (int, error) {
	line := string(p)
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	if line != "" {
		w.ring.add(line)
	}
	return w.out.Write(p)
}

// InstallLogTee 让 log.Printf 同时进入面板可见的缓冲（幂等）。
func InstallLogTee() {
	logTeeOne.Do(func() {
		log.SetOutput(teeWriter{out: os.Stderr, ring: logSink})
	})
}
