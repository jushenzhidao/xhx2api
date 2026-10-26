// responses.go OpenAI Responses 入站：POST /v1/responses（Codex 的入口）。
//
// 与 Messages 入口同一套做法：请求翻成 chat 形态复用主链路，出口把 chat 的
// JSON / SSE 翻译回 Responses 的线上形状。
//
// 刻意丢弃：store / previous_response_id / include / prompt_cache_key 的语义
// 由客户端维护（网关无状态，不存会话）；text.format 的结构化输出约束上游 chat
// 不支持，透传会被拒，所以丢掉。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// responses 处理 Responses 请求。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, h.cfg.MaxBodyBytes)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chatBody, err := responsesToChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(chatBody, &probe)
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chatBody))
	r2.ContentLength = int64(len(chatBody))
	rw := newResponsesWriter(w, probe.Stream, requestModelOf(chatBody))
	defer rw.finish()
	h.chatCompletions(rw, r2)
}

// ---------------------------------------------------------------------------
// 入站转换：Responses → Chat

// responsesToChat 把 Responses 请求体翻译成 chat 请求体。
func responsesToChat(src []byte) ([]byte, error) {
	var in map[string]any
	if err := json.Unmarshal(src, &in); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	out := map[string]any{}
	if model, _ := in["model"].(string); model != "" {
		out["model"] = model
	}
	messages := make([]any, 0, 8)
	if instructions, _ := in["instructions"].(string); strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	messages = append(messages, responsesInput(in["input"])...)
	out["messages"] = messages
	if v, ok := in["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := in["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := in["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := in["stream"].(bool); ok {
		out["stream"] = v
	}
	if tools, ok := responsesTools(in["tools"]); ok {
		out["tools"] = tools
	}
	if tc, ok := responsesToolChoice(in["tool_choice"]); ok {
		out["tool_choice"] = tc
	}
	// reasoning.effort：上游不认档位，但透传不会报错（未知字段被忽略）；
	// 有客户端依赖它决定思考长度，丢掉反而更差。
	if reasoning, ok := in["reasoning"].(map[string]any); ok {
		if effort, _ := reasoning["effort"].(string); effort != "" {
			out["reasoning_effort"] = effort
		}
	}
	return json.Marshal(out)
}

// responsesInput 把 input（字符串或条目数组）转成 chat messages。
func responsesInput(v any) []any {
	switch typed := v.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []any{map[string]any{"role": "user", "content": typed}}
	case []any:
		out := make([]any, 0, len(typed))
		for _, raw := range typed {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if itemType, _ := item["type"].(string); itemType == "function_call_output" {
				out = append(out, map[string]any{
					"role":         "tool",
					"tool_call_id": item["call_id"],
					"content":      stringifyOutput(item["output"]),
				})
				continue
			}
			if itemType, _ := item["type"].(string); itemType == "function_call" {
				// 历史里的工具调用：作为 assistant 的 tool_calls 回传。
				out = append(out, map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{map[string]any{
						"id": item["call_id"], "type": "function",
						"function": map[string]any{
							"name": item["name"], "arguments": stringifyOutput(item["arguments"]),
						},
					}},
				})
				continue
			}
			role, _ := item["role"].(string)
			if role == "" {
				role = "user"
			}
			out = append(out, map[string]any{
				"role": mapResponsesRole(role), "content": responsesContent(item["content"]),
			})
		}
		return out
	}
	return nil
}

// mapResponsesRole：Responses 用 developer 表示高优先级指令，chat 只有 system。
func mapResponsesRole(role string) string {
	switch role {
	case "developer":
		return "system"
	case "assistant", "system", "user", "tool":
		return role
	}
	return "user"
}

// responsesContent 把 Responses 的内容块转成 chat 的 content。
func responsesContent(v any) any {
	switch typed := v.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		var parts []any
		for _, raw := range typed {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch pType, _ := part["type"].(string); pType {
			case "input_text", "output_text", "text", "summary_text":
				if t, ok := part["text"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "input_image", "image_url":
				url, _ := part["image_url"].(string)
				if url == "" {
					if inner, ok := part["image_url"].(map[string]any); ok {
						url, _ = inner["url"].(string)
					}
				}
				if url != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			case "refusal":
				if t, ok := part["refusal"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		if len(parts) == 1 {
			if p, ok := parts[0].(map[string]any); ok && p["type"] == "text" {
				return p["text"]
			}
		}
		return parts
	}
	return ""
}

// stringifyOutput 把 function_call_output 的 output 拍平成字符串。
func stringifyOutput(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case nil:
		return ""
	case []any:
		var sb strings.Builder
		for _, raw := range typed {
			if part, ok := raw.(map[string]any); ok {
				if t, ok := part["text"].(string); ok {
					sb.WriteString(t)
					continue
				}
			}
			sb.WriteString(mustJSON(raw))
		}
		return sb.String()
	}
	return mustJSON(v)
}

// responsesTools 把 Responses 的扁平工具声明转成 chat 的嵌套形状。
func responsesTools(v any) ([]any, bool) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	out := make([]any, 0, len(list))
	for _, raw := range list {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// Responses 的 function 是平铺的；内置工具（web_search / file_search 等）
		// 没有本地执行器，跳过。
		if t, _ := tool["type"].(string); t != "function" && t != "" {
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}
		params, _ := tool["parameters"].(map[string]any)
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": name, "description": tool["description"], "parameters": params,
			},
		})
	}
	return out, len(out) > 0
}

// responsesToolChoice 映射 tool_choice。
func responsesToolChoice(v any) (any, bool) {
	switch typed := v.(type) {
	case string:
		switch typed {
		case "auto", "required", "none":
			return typed, true
		}
	case map[string]any:
		if t, _ := typed["type"].(string); t == "function" {
			if name, _ := typed["name"].(string); name != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}, true
			}
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// 出站：chat → Responses

// chatToResponses 把非流式 chat.completion 转成 response 对象。
func chatToResponses(obj map[string]any, model string) map[string]any {
	output := make([]any, 0, 4)
	choices, _ := obj["choices"].([]any)
	var message map[string]any
	var finish string
	if len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			message, _ = choice["message"].(map[string]any)
			finish, _ = choice["finish_reason"].(string)
		}
	}
	if message == nil {
		message = map[string]any{}
	}
	if reasoning, _ := message["reasoning_content"].(string); reasoning != "" {
		output = append(output, map[string]any{
			"type": "reasoning", "id": "rs_" + randomHex(16), "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	text, _ := message["content"].(string)
	if text != "" {
		output = append(output, map[string]any{
			"type": "message", "id": "msg_" + randomHex(16), "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		})
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			callID, _ := call["id"].(string)
			output = append(output, map[string]any{
				"type": "function_call", "id": "fc_" + randomHex(16), "call_id": orDefaultStr(callID, "call_"+randomHex(12)),
				"name": name, "arguments": orDefaultStr(args, "{}"), "status": "completed",
			})
		}
	}
	if len(output) == 0 {
		output = append(output, map[string]any{
			"type": "message", "id": "msg_" + randomHex(16), "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
		})
	}
	id, _ := obj["id"].(string)
	prompt, completion, total := tokenCounts(obj["usage"])
	return map[string]any{
		"id":                  respID(id),
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              responsesStatus(finish),
		"model":               model,
		"output":              output,
		"parallel_tool_calls": true,
		"usage": map[string]any{
			"input_tokens": prompt, "output_tokens": completion, "total_tokens": total,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		},
	}
}

// responsesStatus 把 finish_reason 映射成 response 状态。
func responsesStatus(finish string) string {
	switch finish {
	case "length":
		return "incomplete"
	case "content_filter":
		return "incomplete"
	}
	return "completed"
}

func respID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "resp_" + randomHex(20)
	}
	if strings.HasPrefix(id, "resp_") {
		return id
	}
	if strings.HasPrefix(id, "chatcmpl-") {
		return "resp_" + strings.TrimPrefix(id, "chatcmpl-")
	}
	return "resp_" + id
}

func orDefaultStr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// ---------------------------------------------------------------------------
// 出站流式

// responsesWriter 包住 ResponseWriter，把 chat SSE 翻译成 Responses 事件流。
type responsesWriter struct {
	http.ResponseWriter
	stream bool
	model  string

	status   int
	buf      bytes.Buffer
	lineBuf  lineAccumulator
	streamer *responsesStream
}

func newResponsesWriter(w http.ResponseWriter, stream bool, model string) *responsesWriter {
	return &responsesWriter{ResponseWriter: w, stream: stream, model: model}
}

func (w *responsesWriter) WriteHeader(code int) { w.status = code }

func (w *responsesWriter) Write(p []byte) (int, error) {
	if w.status >= 400 {
		w.buf.Write(p)
		return len(p), nil
	}
	if !w.stream {
		w.buf.Write(p)
		return len(p), nil
	}
	for _, frame := range w.lineBuf.push(p) {
		w.handleFrame(frame)
	}
	return len(p), nil
}

// Flush 空实现：真正的 flush 在翻译器写出事件后做。
func (w *responsesWriter) Flush() {}

func (w *responsesWriter) handleFrame(data string) {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return
	}
	if trimmed == "[DONE]" {
		w.ensureStreamer().finish()
		return
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
		return
	}
	if errObj, ok := chunk["error"].(map[string]any); ok {
		msg, _ := errObj["message"].(string)
		w.ensureStreamer().fail(msg)
		return
	}
	w.ensureStreamer().onChunk(chunk)
}

func (w *responsesWriter) ensureStreamer() *responsesStream {
	if w.streamer == nil {
		w.streamer = newResponsesStream(w.ResponseWriter, w.model)
	}
	return w.streamer
}

// finish 收尾。
func (w *responsesWriter) finish() {
	if w.status >= 400 {
		// Responses 的错误形状与 chat 一致（error 对象），直接把 body 转出去。
		var payload map[string]any
		if json.Unmarshal(w.buf.Bytes(), &payload) == nil {
			writeJSON(w.ResponseWriter, w.status, payload)
			return
		}
		writeOpenAIError(w.ResponseWriter, w.status, "proxy_error", extractErrorMessage(w.buf.Bytes()))
		return
	}
	if w.stream {
		if w.streamer == nil {
			writeOpenAIError(w.ResponseWriter, http.StatusBadGateway, "empty_stream", "上游没有返回任何内容")
			return
		}
		w.streamer.finish()
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(w.buf.Bytes(), &obj); err != nil {
		writeOpenAIError(w.ResponseWriter, http.StatusBadGateway, "proxy_error", "网关收到的上游响应无法解析")
		return
	}
	if errObj, ok := obj["error"].(map[string]any); ok {
		code, _ := errObj["code"].(string)
		status := http.StatusBadGateway
		switch code {
		case "insufficient_quota", "rate_limit_exceeded":
			status = http.StatusTooManyRequests
		case "account_session_expired":
			status = http.StatusUnauthorized
		case "model_not_found", "model_hidden", "invalid_request":
			status = http.StatusBadRequest
		}
		writeJSON(w.ResponseWriter, status, obj)
		return
	}
	writeJSON(w.ResponseWriter, http.StatusOK, chatToResponses(obj, w.model))
}

// responsesStream 是 Responses SSE 翻译器。
//
// 只发 Codex / 新版 Agent 真正消费的事件：created → output_item.added →
// content_part.added → output_text.delta → *.done → completed。多余事件不发，
// 少发一个客户端只是不显示，发错形状才会报错。
type responsesStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string

	respID   string
	created  bool
	finished bool

	// text 项
	textOpen   bool
	textItemID string
	textIndex  int
	text       strings.Builder
	// reasoning 项
	reasonOpen bool
	reasonID   string
	reasonIdx  int
	reason     strings.Builder
	// 工具项
	tools     map[int64]*respToolItem
	toolOrder []int64

	nextIndex  int
	stopReason string
	promptTok  int64
	completTok int64
}

type respToolItem struct {
	itemID  string
	callID  string
	name    string
	index   int
	args    strings.Builder
	started bool
}

func newResponsesStream(w http.ResponseWriter, model string) *responsesStream {
	fl, _ := w.(http.Flusher)
	return &responsesStream{
		w: w, flusher: fl, model: model,
		respID: "resp_" + randomHex(20),
		tools:  map[int64]*respToolItem{},
	}
}

// emit 写一个具名事件。
func (s *responsesStream) emit(event string, payload map[string]any) {
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
	_, _ = s.w.Write([]byte("event: " + event + "\ndata: " + string(raw) + "\n\n"))
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// responseEnvelope 构造 response 对象骨架（事件里到处要带）。
func (s *responsesStream) responseEnvelope(status string, output []any) map[string]any {
	return map[string]any{
		"id": s.respID, "object": "response", "created_at": time.Now().Unix(),
		"status": status, "model": s.model, "output": output,
		"parallel_tool_calls": true,
	}
}

// ensureCreated 发 response.created。
func (s *responsesStream) ensureCreated() {
	if s.created {
		return
	}
	s.created = true
	s.emit("response.created", map[string]any{
		"type": "response.created", "response": s.responseEnvelope("in_progress", []any{}),
	})
}

// onChunk 吃一个 chat 增量。
func (s *responsesStream) onChunk(chunk map[string]any) {
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
	s.ensureCreated()
	if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
		s.openReasoning()
		s.reason.WriteString(reasoning)
		s.emit("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "item_id": s.reasonID,
			"output_index": s.reasonIdx, "summary_index": 0, "delta": reasoning,
		})
	}
	if text, ok := delta["content"].(string); ok && text != "" {
		s.closeReasoning()
		s.openText()
		s.text.WriteString(text)
		s.emit("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": s.textItemID,
			"output_index": s.textIndex, "content_index": 0, "delta": text,
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

// openText 声明 text 输出项。
func (s *responsesStream) openText() {
	if s.textOpen {
		return
	}
	s.textOpen = true
	s.textItemID = "msg_" + randomHex(16)
	s.textIndex = s.nextIndex
	s.nextIndex++
	s.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": s.textIndex,
		"item": map[string]any{
			"type": "message", "id": s.textItemID, "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	})
	s.emit("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "item_id": s.textItemID,
		"output_index": s.textIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

// openReasoning 声明 reasoning 输出项。
func (s *responsesStream) openReasoning() {
	if s.reasonOpen {
		return
	}
	s.reasonOpen = true
	s.reasonID = "rs_" + randomHex(16)
	s.reasonIdx = s.nextIndex
	s.nextIndex++
	s.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": s.reasonIdx,
		"item": map[string]any{"type": "reasoning", "id": s.reasonID, "status": "in_progress", "summary": []any{}},
	})
}

// closeReasoning 关闭 reasoning 项。
func (s *responsesStream) closeReasoning() {
	if !s.reasonOpen {
		return
	}
	s.reasonOpen = false
	text := s.reason.String()
	s.emit("response.reasoning_summary_text.done", map[string]any{
		"type": "response.reasoning_summary_text.done", "item_id": s.reasonID,
		"output_index": s.reasonIdx, "summary_index": 0, "text": text,
	})
	s.emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": s.reasonIdx,
		"item": map[string]any{
			"type": "reasoning", "id": s.reasonID, "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": text}},
		},
	})
}

// onToolDelta 处理工具调用增量。
func (s *responsesStream) onToolDelta(index int64, call map[string]any) {
	fn, _ := call["function"].(map[string]any)
	name, _ := fn["name"].(string)
	args, _ := fn["arguments"].(string)
	id, _ := call["id"].(string)
	item := s.tools[index]
	if item == nil {
		s.closeText()
		s.closeReasoning()
		item = &respToolItem{
			itemID: "fc_" + randomHex(16), callID: orDefaultStr(id, "call_"+randomHex(12)),
			name: name, index: s.nextIndex,
		}
		s.nextIndex++
		s.tools[index] = item
		s.toolOrder = append(s.toolOrder, index)
		item.started = true
		s.emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": item.index,
			"item": map[string]any{
				"type": "function_call", "id": item.itemID, "call_id": item.callID,
				"name": item.name, "arguments": "", "status": "in_progress",
			},
		})
	}
	if name != "" && item.name == "" {
		item.name = name
	}
	if args != "" {
		item.args.WriteString(args)
		s.emit("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": item.itemID,
			"output_index": item.index, "delta": args,
		})
	}
}

// closeText 关闭 text 项。
func (s *responsesStream) closeText() {
	if !s.textOpen {
		return
	}
	s.textOpen = false
	text := s.text.String()
	s.emit("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": s.textItemID,
		"output_index": s.textIndex, "content_index": 0, "text": text,
	})
	s.emit("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": s.textItemID,
		"output_index": s.textIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	s.emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": s.textIndex,
		"item": map[string]any{
			"type": "message", "id": s.textItemID, "status": "completed",
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		},
	})
}

// finish 收尾：关块 → response.completed。
func (s *responsesStream) finish() {
	if s.finished {
		return
	}
	s.ensureCreated()
	s.closeText()
	s.closeReasoning()
	output := make([]any, 0, len(s.toolOrder)+2)
	if s.reason.Len() > 0 {
		output = append(output, map[string]any{
			"type": "reasoning", "id": s.reasonID, "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": s.reason.String()}},
		})
	}
	if s.text.Len() > 0 {
		output = append(output, map[string]any{
			"type": "message", "id": s.textItemID, "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": s.text.String(), "annotations": []any{}}},
		})
	}
	for _, idx := range s.toolOrder {
		item := s.tools[idx]
		if item == nil {
			continue
		}
		args := item.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		s.emit("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": item.itemID,
			"output_index": item.index, "arguments": args,
		})
		s.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": item.index,
			"item": map[string]any{
				"type": "function_call", "id": item.itemID, "call_id": item.callID,
				"name": item.name, "arguments": args, "status": "completed",
			},
		})
		output = append(output, map[string]any{
			"type": "function_call", "id": item.itemID, "call_id": item.callID,
			"name": item.name, "arguments": args, "status": "completed",
		})
	}
	final := s.responseEnvelope(responsesStatus(s.stopReason), output)
	final["usage"] = map[string]any{
		"input_tokens": s.promptTok, "output_tokens": s.completTok,
		"total_tokens":          s.promptTok + s.completTok,
		"input_tokens_details":  map[string]any{"cached_tokens": 0},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	}
	s.emit("response.completed", map[string]any{"type": "response.completed", "response": final})
	s.finished = true
}

// fail 在流里报错并收尾。
func (s *responsesStream) fail(msg string) {
	if s.finished {
		return
	}
	s.ensureCreated()
	s.emit("response.failed", map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id": s.respID, "object": "response", "status": "failed", "model": s.model,
			"error": map[string]any{"code": "upstream_error", "message": msg},
		},
	})
	s.finished = true
}
