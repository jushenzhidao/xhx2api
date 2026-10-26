// messages.go Anthropic Messages 入站：POST /v1/messages 与 /v1/messages/count_tokens。
//
// 做法是「复用 chat 链路 + 包装 ResponseWriter」：请求先翻译成 chat 形态，交给
// chatCompletions（选号/续期/重试/记账全部沿用），出口再把 chat 的 JSON 或 SSE
// 翻译回 Anthropic 的线上形状。这样两个协议的账号策略不会分叉。
//
// 刻意丢弃的字段（上游 chat 没有对应物）：
//
//	top_k / cache_control / context_management / betas / metadata / service_tier
//
// 内置服务端工具（web_search / code_execution / bash_* 等）没有本地执行器，
// 丢弃并在 system 里写明，避免模型假装调用了一个不存在的工具。
//
// thinking 块：上游 reasoning_content 没有 signature（Anthropic 的真 thinking
// 块要求签名），所以作为 thinking 块下发展示；客户端回传的 thinking 块丢弃，
// 不触发签名校验。
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// messages 处理 Anthropic Messages 请求。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, h.cfg.MaxBodyBytes)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	chatBody, err := messagesToChat(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(chatBody, &probe)
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chatBody))
	r2.ContentLength = int64(len(chatBody))
	rw := newMessagesWriter(w, probe.Stream, requestModelOf(chatBody))
	defer rw.finish()
	h.chatCompletions(rw, r2)
}

// countTokens 回答 Anthropic 的 token 估算请求。
//
// 上游没有计数接口，按「字符数/4 + 结构开销」粗估：Claude Code 用它判断上下文
// 余量，估偏 10% 比 404 让它自己猜要好。中文按 1 字符 ≈ 0.6 token 估（比 /4 更准）。
func (h *Handler) countTokens(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r, h.cfg.MaxBodyBytes)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	chars := countChars(obj["system"]) + countChars(obj["messages"]) + countChars(obj["tools"])
	estimate := int(float64(chars) * 0.55)
	if estimate < 1 {
		estimate = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimate})
}

// countChars 统计一段任意 JSON 结构里的可见字符数。
func countChars(v any) int {
	switch typed := v.(type) {
	case string:
		return len([]rune(typed))
	case []any:
		total := 0
		for _, item := range typed {
			total += countChars(item)
		}
		return total
	case map[string]any:
		total := 0
		for _, item := range typed {
			total += countChars(item)
		}
		return total
	}
	return 0
}

// requestModelOf 从 body 里取 model（emit 的 message 里要回显客户端请求的名字）。
func requestModelOf(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Model
}

// ---------------------------------------------------------------------------
// 入站转换：Anthropic Messages → OpenAI Chat

// messagesToChat 把 Anthropic 请求体翻译成 chat 请求体。
func messagesToChat(src []byte) ([]byte, error) {
	var in map[string]any
	if err := json.Unmarshal(src, &in); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	out := map[string]any{}
	if model, _ := in["model"].(string); model != "" {
		out["model"] = model
	}
	// system 可能是字符串或 text 块数组；Ascend 的客户端两种都会发。
	system := systemText(in["system"])
	rawMessages, _ := in["messages"].([]any)
	messages := convertAnthropicMessages(rawMessages)
	if system != "" {
		messages = append([]any{map[string]any{"role": "system", "content": system}}, messages...)
	}
	out["messages"] = messages
	if v, ok := in["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	if v, ok := in["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := in["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := in["stop_sequences"].([]any); ok && len(v) > 0 {
		out["stop"] = v
	}
	if v, ok := in["stream"].(bool); ok {
		out["stream"] = v
	}
	tools, skipped := anthropicTools(in["tools"])
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if len(skipped) > 0 {
		// 没有执行器的内置工具：不转成 function（否则模型会调用一个永远失败的
		// 工具），而是在 system 里说明「该能力不可用」。
		note := skippedToolsNote(skipped)
		if existing, ok := out["messages"].([]any); ok && len(existing) > 0 {
			if first, ok := existing[0].(map[string]any); ok {
				if role, _ := first["role"].(string); role == "system" {
					first["content"] = strings.TrimSpace(fmt.Sprintf("%v\n%s", first["content"], note))
				} else {
					out["messages"] = append([]any{map[string]any{"role": "system", "content": note}}, existing...)
				}
			}
		}
	}
	if tc, ok := mapAnthropicToolChoice(in["tool_choice"]); ok {
		out["tool_choice"] = tc
	}
	return json.Marshal(out)
}

// convertAnthropicMessages 把 Anthropic 的 messages 数组转成 chat 的 messages。
func convertAnthropicMessages(in []any) []any {
	out := make([]any, 0, len(in)+2)
	for _, raw := range in {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch content := msg["content"].(type) {
		case string:
			out = append(out, map[string]any{"role": role, "content": content})
		case []any:
			if role == "assistant" {
				out = append(out, assistantMessage(content)...)
				continue
			}
			out = append(out, userMessages(content)...)
		}
	}
	return out
}

// assistantMessage：assistant 的 text 拼成 content，tool_use 转 tool_calls，
// thinking 丢弃（没有 signature，回传给上游会被判格式错误）。
func assistantMessage(blocks []any) []any {
	var texts []string
	var calls []any
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch blockType(block) {
		case "text":
			if t, ok := block["text"].(string); ok && t != "" {
				texts = append(texts, t)
			}
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			args := mustJSON(block["input"])
			calls = append(calls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			})
		case "thinking", "redacted_thinking":
			// 丢弃
		}
	}
	msg := map[string]any{"role": "assistant", "content": strings.Join(texts, "")}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
		if msg["content"] == "" {
			msg["content"] = nil
		}
	}
	return []any{msg}
}

// userMessages：user 消息里可能混着 tool_result，必须拆开。
//
// OpenAI 要求 tool 消息紧跟对应的 assistant tool_calls，且 role=tool 只能承载
// 工具结果。所以同一 Anthropic user 消息里的 tool_result 先输出，剩余的文本/图片
// 再作为一条 user 消息。
func userMessages(blocks []any) []any {
	var toolMsgs []any
	var parts []any
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch blockType(block) {
		case "tool_result":
			toolMsgs = append(toolMsgs, map[string]any{
				"role":         "tool",
				"tool_call_id": block["tool_use_id"],
				"content":      toolResultContent(block["content"], block["is_error"]),
			})
		case "text":
			if t, ok := block["text"].(string); ok {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "image":
			parts = append(parts, imagePart(block))
		case "document":
			if t := documentText(block); t != "" {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		}
	}
	out := make([]any, 0, 2)
	out = append(out, toolMsgs...)
	if len(parts) == 1 {
		if p, ok := parts[0].(map[string]any); ok && p["type"] == "text" {
			out = append(out, map[string]any{"role": "user", "content": p["text"]})
			return out
		}
	}
	if len(parts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": parts})
	}
	return out
}

// blockType 取块类型（Anthropic 的 type 字段）。
func blockType(block map[string]any) string {
	t, _ := block["type"].(string)
	return t
}

// imagePart 把 Anthropic 的 image 块转成 OpenAI 的 image_url 块。
func imagePart(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return map[string]any{"type": "text", "text": "[图片]"}
	}
	switch srcType, _ := source["type"].(string); srcType {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if mediaType == "" {
			mediaType = "image/png"
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{
			"url": "data:" + mediaType + ";base64," + data,
		}}
	case "url":
		url, _ := source["url"].(string)
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
	}
	return map[string]any{"type": "text", "text": "[不支持的图片源]"}
}

// documentText 提取 document 块的文本（PDF 的 base64 没有本地解析器，只取纯文本块）。
func documentText(block map[string]any) string {
	if source, ok := block["source"].(map[string]any); ok {
		if srcType, _ := source["type"].(string); srcType == "text" {
			if data, ok := source["data"].(string); ok {
				return data
			}
		}
	}
	if t, ok := block["title"].(string); ok {
		return "[文档] " + t
	}
	return ""
}

// toolResultContent 把 tool_result 的内容拍平成字符串。
//
// Anthropic 允许 content 是字符串或块数组（含图片）。OpenAI 的 tool 消息只收
// 字符串，所以图片被转成「[图片]」占位——这是有损的，但比丢整条结果好。
func toolResultContent(v any, isError any) string {
	var sb strings.Builder
	switch typed := v.(type) {
	case string:
		sb.WriteString(typed)
	case []any:
		for _, raw := range typed {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch blockType(block) {
			case "text":
				if t, ok := block["text"].(string); ok {
					sb.WriteString(t)
				}
			case "image":
				sb.WriteString("[图片]")
			}
		}
	default:
		if typed != nil {
			sb.WriteString(mustJSON(typed))
		}
	}
	if flags, ok := isError.(bool); ok && flags {
		return "ERROR: " + sb.String()
	}
	return sb.String()
}

// systemText 把 system（字符串或 text 块数组）拍平成字符串。
func systemText(v any) string {
	switch typed := v.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		var sb strings.Builder
		for _, raw := range typed {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if blockType(block) == "text" {
				if t, ok := block["text"].(string); ok {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(t)
				}
			}
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

// anthropicTools 把 Anthropic 工具声明转成 chat function 声明。
//
// 有 input_schema 的是自定义工具；只有 type 没有 input_schema 的是 Anthropic
// 内置工具（web_search_20250305 之类），本地没有执行器，收集名字交给调用方提示。
func anthropicTools(v any) (out []any, skipped []string) {
	list, _ := v.([]any)
	for _, raw := range list {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tool["name"].(string)
		schema, hasSchema := tool["input_schema"].(map[string]any)
		if name == "" {
			continue
		}
		if !hasSchema {
			skipped = append(skipped, toolLabel(tool))
			continue
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": tool["description"],
				"parameters":  schema,
			},
		})
	}
	return out, skipped
}

// toolLabel 给内置工具起一个可读名字（type 通常是 web_search_20250305 这种带版本号）。
func toolLabel(tool map[string]any) string {
	if name, _ := tool["name"].(string); name != "" {
		return name
	}
	if t, _ := tool["type"].(string); t != "" {
		if i := strings.LastIndex(t, "_"); i > 0 {
			if _, err := fmt.Sscanf(t[i+1:], "%d", new(int)); err == nil {
				return t[:i]
			}
		}
		return t
	}
	return "unknown_tool"
}

// skippedToolsNote 生成「内置工具不可用」的 system 提示。
func skippedToolsNote(names []string) string {
	return fmt.Sprintf("[网关提示] 以下服务端工具在当前通道不可用，请不要尝试调用：%s。",
		strings.Join(names, "、"))
}

// mapAnthropicToolChoice 把 Anthropic 的 tool_choice 映射成 chat 的。
func mapAnthropicToolChoice(v any) (any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	switch t, _ := m["type"].(string); t {
	case "auto":
		return "auto", true
	case "any":
		// Anthropic 的 any = 必须调用某个工具，对应 OpenAI 的 required。
		return "required", true
	case "none":
		return "none", true
	case "tool":
		name, _ := m["name"].(string)
		if name == "" {
			return nil, false
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}, true
	}
	return nil, false
}

// mustJSON 把任意值序列化成字符串（失败返回 "{}"）。
func mustJSON(v any) string {
	if v == nil {
		return "{}"
	}
	if s, ok := v.(string); ok {
		return s
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// 出站：chat → Anthropic

// chatToAnthropicMessage 把非流式 chat.completion 转成 Anthropic message。
func chatToAnthropicMessage(obj map[string]any, model string) map[string]any {
	content := make([]any, 0, 4)
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
		content = append(content, map[string]any{"type": "thinking", "thinking": reasoning})
	}
	if text, _ := message["content"].(string); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
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
			id, _ := call["id"].(string)
			content = append(content, map[string]any{
				"type": "tool_use", "id": anthToolID(id), "name": name, "input": parseToolInput(args),
			})
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	id, _ := obj["id"].(string)
	return map[string]any{
		"id":            msgID(id),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthropicStop(finish, len(content) > 0 && hasToolUse(content)),
		"stop_sequence": nil,
		"usage":         anthropicUsage(obj["usage"]),
	}
}

func hasToolUse(content []any) bool {
	for _, raw := range content {
		if block, ok := raw.(map[string]any); ok && blockType(block) == "tool_use" {
			return true
		}
	}
	return false
}

// parseToolInput 把 arguments 字符串解析成对象（Anthropic 的 input 必须是对象）。
func parseToolInput(args string) any {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return map[string]any{}
	}
	var out any
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return map[string]any{"_raw": trimmed}
	}
	return out
}

// anthropicStop 把 finish_reason 映射成 Anthropic 的 stop_reason。
func anthropicStop(finish string, hasTools bool) string {
	if hasTools {
		return "tool_use"
	}
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	case "stop", "":
		return "end_turn"
	}
	return "end_turn"
}

// anthropicUsage 转用量字段。
func anthropicUsage(v any) map[string]any {
	m, _ := v.(map[string]any)
	prompt, completion, _ := tokenCounts(m)
	return map[string]any{"input_tokens": prompt, "output_tokens": completion}
}

// msgID 把 chatcmpl-xxx 转成 msg_xxx（Anthropic 客户端按前缀识别）。
func msgID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "msg_" + randomHex(12)
	}
	for _, prefix := range []string{"chatcmpl-", "msg_"} {
		if strings.HasPrefix(id, prefix) {
			return "msg_" + strings.TrimPrefix(id, prefix)
		}
	}
	return "msg_" + id
}

// anthToolID 保证工具调用 id 是 Anthropic 的 toolu_ 形状（客户端会校验）。
func anthToolID(id string) string {
	if id == "" {
		return "toolu_" + randomHex(12)
	}
	if strings.HasPrefix(id, "toolu_") {
		return id
	}
	if strings.HasPrefix(id, "call_") {
		return "toolu_" + strings.TrimPrefix(id, "call_")
	}
	return id
}

func randomHex(n int) string {
	const hexChars = "0123456789abcdef"
	buf := make([]byte, n)
	seed := uint32(0)
	for i := range buf {
		seed = seed*1103515245 + 12345
		buf[i] = hexChars[(seed>>16)%16]
	}
	return string(buf)
}

// ---------------------------------------------------------------------------
// 出站：SSE 翻译器

// messagesWriter 包住真实 ResponseWriter，把 chat 的输出翻译成 Anthropic 形状。
//
// 三种情况：
//   - 非流式成功：缓冲整个 JSON，finish 时转换后写出；
//   - 流式成功：逐帧解析 chat SSE，转成 Anthropic 事件流；
//   - 错误（status >= 400）：缓冲错误体，finish 时转换成 Anthropic 错误。
type messagesWriter struct {
	http.ResponseWriter
	stream bool
	model  string

	status    int
	wroteHead bool
	buf       bytes.Buffer // 非流式/错误体的累积
	lineBuf   lineAccumulator

	streamer *anthropicStream
}

func newMessagesWriter(w http.ResponseWriter, stream bool, model string) *messagesWriter {
	return &messagesWriter{ResponseWriter: w, stream: stream, model: model}
}

func (w *messagesWriter) WriteHeader(code int) {
	// 不立即写：错误体要转换成 Anthropic 形状，流式要先补 message_start。
	w.status = code
}

func (w *messagesWriter) Write(p []byte) (int, error) {
	if w.status >= 400 {
		w.buf.Write(p)
		return len(p), nil
	}
	if !w.stream {
		w.buf.Write(p)
		return len(p), nil
	}
	// 流式：按行解析 chat SSE。
	for _, frame := range w.lineBuf.push(p) {
		w.handleFrame(frame)
	}
	return len(p), nil
}

// Flush 是空实现：真正的 flush 由翻译器在写出 Anthropic 事件后触发，
// 避免把未翻译的 chat 帧提前推给客户端。
func (w *messagesWriter) Flush() {}

// handleFrame 处理一行 chat SSE。
func (w *messagesWriter) handleFrame(data string) {
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
		w.ensureStreamer().emitError(msg, "api_error")
		return
	}
	w.ensureStreamer().onChunk(chunk)
}

func (w *messagesWriter) ensureStreamer() *anthropicStream {
	if w.streamer == nil {
		w.streamer = newAnthropicStream(w.ResponseWriter, w.model)
	}
	return w.streamer
}

// finish 收尾：错误转换 / 非流式转换 / 流式补尾。
func (w *messagesWriter) finish() {
	if w.status >= 400 {
		msg := extractErrorMessage(w.buf.Bytes())
		writeAnthropicError(w.ResponseWriter, w.status, anthropicErrType(w.status), msg)
		return
	}
	if w.stream {
		if w.streamer == nil {
			// 一帧都没收到：给客户端一个明确的错误，而不是空流。
			writeAnthropicError(w.ResponseWriter, http.StatusBadGateway, "api_error", "上游没有返回任何内容")
			return
		}
		w.streamer.finish()
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(w.buf.Bytes(), &obj); err != nil {
		writeAnthropicError(w.ResponseWriter, http.StatusBadGateway, "api_error", "网关收到的上游响应无法解析")
		return
	}
	if errObj, ok := obj["error"].(map[string]any); ok {
		msg, _ := errObj["message"].(string)
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
		writeAnthropicError(w.ResponseWriter, status, anthropicErrType(status), msg)
		return
	}
	writeJSON(w.ResponseWriter, http.StatusOK, chatToAnthropicMessage(obj, w.model))
}

// anthropicErrType 把 HTTP 状态映射成 Anthropic 错误类型。
func anthropicErrType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// writeAnthropicError 写 Anthropic 形状的错误。
func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// extractErrorMessage 从错误体里取人类可读文案。
func extractErrorMessage(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		text := strings.TrimSpace(string(body))
		if text == "" {
			return "上游请求失败"
		}
		return text
	}
	if errObj, ok := payload["error"].(map[string]any); ok {
		if msg, ok := errObj["message"].(string); ok && msg != "" {
			return msg
		}
	}
	if msg, ok := payload["message"].(string); ok && msg != "" {
		return msg
	}
	return "上游请求失败"
}

// lineAccumulator 按行切 SSE（与 upstream 的 lineBuffer 同构，但这是 server 侧
// 的独立实现：两个包各自解析自己那一段，避免互相 import）。
type lineAccumulator struct{ tail []byte }

func (b *lineAccumulator) push(chunk []byte) []string {
	b.tail = append(b.tail, chunk...)
	var out []string
	start := 0
	for i := 0; i < len(b.tail); i++ {
		if b.tail[i] != '\n' {
			continue
		}
		line := strings.TrimSuffix(string(b.tail[start:i]), "\r")
		if strings.HasPrefix(line, "data:") {
			out = append(out, strings.TrimPrefix(line, "data:"))
		}
		start = i + 1
	}
	b.tail = b.tail[start:]
	return out
}

// errBodyTooLarge 是 readBody 的超限错误（供上层判别）。
var errBodyTooLarge = errors.New("请求体过大")
