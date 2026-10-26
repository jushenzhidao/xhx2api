// wire.go 少量共享的线上细节：SSE 首帧预读、思考标签、响应 id 与数字归一。
package upstream

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// thinkTags 是上游可能把思考内容包在正文里的标签。
//
// 上游正常把思考放在 delta.reasoning_content 里，但个别模型会把
// <thinking>…</thinking> 直接写进 content。两种都剥掉，客户端才不会看到
// 一串标签（Claude Code 之类的客户端不会自己处理它们）。
var thinkTags = [][2]string{
	{"<thinking>", "</thinking>"},
	{"<reason>", "</reason>"},
	{"<thought>", "</thought>"},
}

// randomUUIDOrEmpty 生成 v4 UUID；失败返回空串（响应 id 会退化成时间戳）。
func randomUUIDOrEmpty() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 32)
	hex.Encode(dst, b[:])
	return string(dst[0:8]) + "-" + string(dst[8:12]) + "-" + string(dst[12:16]) + "-" +
		string(dst[16:20]) + "-" + string(dst[20:32])
}

// asInt64 把 JSON 数字/数字字符串读成 int64。
func asInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case json.Number:
		n, err := typed.Int64()
		return n, err == nil
	case string:
		if n, ok := intOf(typed); ok {
			return int64(n), true
		}
	}
	return 0, false
}

// floatOf 把 JSON 数字/数字串读成 float64。
func floatOf(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int64:
		return float64(typed), true
	case json.Number:
		f, err := typed.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return f, err == nil
	}
	return 0, false
}

// int64Of 把 JSON 数字/数字串读成 int64（0 = 没有）。
func int64Of(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// peekReader 允许在把流交给调用方之前先看一帧，之后仍能原样重放。
//
// 为什么必须「看一眼」：上游用 HTTP 200 + SSE 首帧 {"error":…} 表达限流/欠费。
// 不预读就会先把 200 发给客户端，等发现错误时已经改不了状态码、也没法换号重试。
type peekReader struct {
	src  io.ReadCloser
	buf  bytes.Buffer // 已读出、尚未交出去的全部字节
	head sseEvent     // 首帧解析结果
	done bool
}

func newPeekReader(src io.ReadCloser) *peekReader { return &peekReader{src: src} }

// peekFrame 读到第一个完整 SSE 帧并解析（不消费：stream() 会把它一起吐出来）。
func (p *peekReader) peekFrame() (sseEvent, error) {
	if p.done {
		return p.head, nil
	}
	p.done = true
	reader := bufio.NewReaderSize(p.src, 64*1024)
	var frame []string
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			p.buf.Write(line)
			trimmed := strings.TrimRight(string(line), "\r\n")
			if trimmed == "" {
				if len(frame) > 0 {
					break
				}
			} else if strings.HasPrefix(trimmed, "data:") {
				frame = append(frame, strings.TrimPrefix(trimmed, "data:"))
			}
		}
		if err != nil {
			break
		}
	}
	// reader 里可能还有没读完的字节：全部落进 buf，重放时一起给出去。
	if rest, err := io.ReadAll(reader); err == nil && len(rest) > 0 {
		p.buf.Write(rest)
	}
	if len(frame) == 0 {
		p.head = sseEvent{Kind: sseSkip}
		return p.head, nil
	}
	p.head = parseSSELine(strings.Join(frame, "\n"))
	return p.head, nil
}

// stream 返回可读流：先重放已读字节，再继续读上游。
func (p *peekReader) stream() io.ReadCloser {
	return &replayReader{head: bytes.NewReader(p.buf.Bytes()), src: p.src}
}

type replayReader struct {
	head *bytes.Reader
	src  io.ReadCloser
}

func (r *replayReader) Read(p []byte) (int, error) {
	if r.head != nil && r.head.Len() > 0 {
		return r.head.Read(p)
	}
	return r.src.Read(p)
}

func (r *replayReader) Close() error {
	r.head = nil
	return r.src.Close()
}
