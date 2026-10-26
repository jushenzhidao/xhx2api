// messages_stream.go 把 chat 的 SSE 增量翻译成 Anthropic 的 SSE 事件流。
//
// Anthropic 的线上形状是一串具名事件：
//
//	message_start → (content_block_start → content_block_delta* → content_block_stop)*
//	              → message_delta → message_stop
//
// 与 chat 的关键差异：
//   - 块（thinking / text / tool_use）必须先声明再增量，且各自有 index；
//   - 工具调用的参数是 input_json_delta 的 partial_json 拼接（不是完整 JSON）；
//   - stop_reason 在 message_delta 里给，usage 也在这里补 output_tokens。
package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// anthBlock 是一个打开中的内容块。
type anthBlock struct {
	index int
	kind  string // thinking / text / tool_use
	// 工具块专用
	toolID string
	name   string
}

// anthropicStream 是 Anthropic SSE 翻译器。
type anthropicStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string

	started  bool
	finished bool
	msgID    string

	nextIndex int
	current   *anthBlock           // 当前打开的 thinking/text 块
	tools     map[int64]*anthBlock // chat tool index → 已打开的 tool_use 块
	toolOrder []int64              // 按打开顺序（收尾时按 index 排序）

	hasTool    bool
	stopReason string
	promptTok  int64
	completTok int64
}

func newAnthropicStream(w http.ResponseWriter, model string) *anthropicStream {
	fl, _ := w.(http.Flusher)
	return &anthropicStream{
		w: w, flusher: fl, model: model,
		msgID: "msg_" + randomHex(24),
		tools: map[int64]*anthBlock{},
	}
}

// emit 写一个具名事件。
func (s *anthropicStream) emit(event string, payload map[string]any) {
	if s.finished {
		return
	}
	h := s.w.Header()
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var sb strings.Builder
	sb.WriteString("event: ")
	sb.WriteString(event)
	sb.WriteString("\ndata: ")
	sb.Write(raw)
	sb.WriteString("\n\n")
	_, _ = s.w.Write([]byte(sb.String()))
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// ensureStart 发 message_start（只在第一次真正需要输出时发）。
func (s *anthropicStream) ensureStart() {
	if s.started {
		return
	}
	s.started = true
	s.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.msgID, "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": s.promptTok, "output_tokens": 0},
		},
	})
}

// onChunk 吃一个 chat 增量帧。
func (s *anthropicStream) onChunk(chunk map[string]any) {
	if usage, ok := chunk["usage"].(map[string]any); ok {
		p, c, _ := tokenCounts(usage)
		if p > 0 {
			s.promptTok = p
		}
		if c > 0 {
			s.completTok = c
		}
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return
	}
	if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
		s.stopReason = finish
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return
	}
	s.ensureStart()
	if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
		s.openBlock("thinking")
		s.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.current.index,
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
		})
	}
	if text, ok := delta["content"].(string); ok && text != "" {
		s.openBlock("text")
		s.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.current.index,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	}
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			index, _ := asInt64Any(call["index"])
			s.onToolDelta(index, call)
		}
	}
}

// onToolDelta 处理工具调用的增量。
func (s *anthropicStream) onToolDelta(index int64, call map[string]any) {
	fn, _ := call["function"].(map[string]any)
	name, _ := fn["name"].(string)
	args, _ := fn["arguments"].(string)
	id, _ := call["id"].(string)
	block := s.tools[index]
	if block == nil {
		// 新的工具调用：先关掉正在输出的文本块，再声明 tool_use 块。
		s.closeCurrent()
		block = &anthBlock{index: s.nextIndex, kind: "tool_use", toolID: anthToolID(id), name: name}
		s.nextIndex++
		s.tools[index] = block
		s.toolOrder = append(s.toolOrder, index)
		s.hasTool = true
		input := map[string]any{}
		s.emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": block.index,
			"content_block": map[string]any{
				"type": "tool_use", "id": block.toolID, "name": block.name, "input": input,
			},
		})
	}
	if name != "" && block.name == "" {
		block.name = name
	}
	if args != "" {
		s.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": block.index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
	}
}

// openBlock 打开 thinking/text 块（同类型复用，换类型先关旧的）。
func (s *anthropicStream) openBlock(kind string) {
	if s.current != nil && s.current.kind == kind {
		return
	}
	s.closeCurrent()
	block := &anthBlock{index: s.nextIndex, kind: kind}
	s.nextIndex++
	s.current = block
	var contentBlock map[string]any
	switch kind {
	case "thinking":
		contentBlock = map[string]any{"type": "thinking", "thinking": ""}
	default:
		contentBlock = map[string]any{"type": "text", "text": ""}
	}
	s.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": block.index, "content_block": contentBlock,
	})
}

// closeCurrent 关闭当前 thinking/text 块。
func (s *anthropicStream) closeCurrent() {
	if s.current == nil {
		return
	}
	s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.current.index})
	s.current = nil
}

// finish 收尾：关块 → message_delta（stop_reason + usage）→ message_stop。
func (s *anthropicStream) finish() {
	if s.finished {
		return
	}
	s.ensureStart()
	s.closeCurrent()
	// 工具块按打开顺序关闭（index 升序，与声明顺序一致）。
	indexes := append([]int64(nil), s.toolOrder...)
	sort.Slice(indexes, func(i, j int) bool { return s.tools[indexes[i]].index < s.tools[indexes[j]].index })
	for _, idx := range indexes {
		if block := s.tools[idx]; block != nil {
			s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": block.index})
		}
	}
	stop := anthropicStop(s.stopReason, s.hasTool)
	s.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": s.completTok},
	})
	s.emit("message_stop", map[string]any{"type": "message_stop"})
	s.finished = true
}

// emitError 在流中间注入一个错误事件并收尾。
//
// Anthropic 的错误事件是 event: error + {"type":"error","error":{...}}；客户端
// 看到它就停止累积。已经发出的内容保留（客户端会显示半截回答，比静默截断好）。
func (s *anthropicStream) emitError(msg, typ string) {
	if s.finished {
		return
	}
	s.ensureStart()
	s.emit("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
	s.finished = true
}
