// ring.go 运行日志的环形缓冲。
//
// 面板要能看到「最近发生了什么」，但日志同时还要写 stderr（容器日志收集）。
// 所以这里实现 io.Writer，由 main 用 io.MultiWriter 把 log 包与业务日志镜像
// 进来：不改任何已有的 log.Printf 调用，也不引入日志框架。
package panel

import (
	"strings"
	"sync"
	"time"
)

// LogLine 是一行日志。
type LogLine struct {
	Time string `json:"time"`
	Text string `json:"text"`
}

// Ring 是固定容量的环形缓冲。
type Ring struct {
	mu    sync.Mutex
	lines []LogLine
	next  int
	full  bool
	// partial 缓存没有以换行结尾的半行（一次 Write 可能只写半行）。
	partial string
}

// NewRing 建一个容量为 size 的缓冲。
func NewRing(size int) *Ring {
	if size <= 0 {
		size = 500
	}
	return &Ring{lines: make([]LogLine, size)}
}

// Write 实现 io.Writer。
func (r *Ring) Write(p []byte) (int, error) {
	if r == nil {
		return len(p), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	text := r.partial + string(p)
	for {
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(text[:idx], "\r")
		text = text[idx+1:]
		if line != "" {
			r.pushLocked(line)
		}
	}
	// 半行留到下次；但不能无限增长（某些库会一次写很长的非换行内容）。
	if len(text) > 8192 {
		r.pushLocked(text[:8192] + " …")
		text = ""
	}
	r.partial = text
	return len(p), nil
}

func (r *Ring) pushLocked(line string) {
	r.lines[r.next] = LogLine{Time: time.Now().Format("15:04:05"), Text: line}
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
}

// Snapshot 返回最近 n 行（新的在前）。
func (r *Ring) Snapshot(n int) []LogLine {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	size := r.next
	if r.full {
		size = len(r.lines)
	}
	if n <= 0 || n > size {
		n = size
	}
	out := make([]LogLine, 0, n)
	for i := 0; i < n; i++ {
		idx := (r.next - 1 - i + len(r.lines)*2) % len(r.lines)
		if !r.full && idx >= size {
			break
		}
		line := r.lines[idx]
		if line.Text == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// Add 供不经 io.Writer 的路径直接写一行（如业务事件）。
func (r *Ring) Add(text string) {
	if r == nil || text == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pushLocked(text)
}
