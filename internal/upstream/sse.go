// sse.go 解析小浣熊 chat 的 SSE 流，并把增量累积成非流式响应。
//
// 上游的线上形状就是 OpenAI：一帧一个完整 chunk（没有信封），错误用
// {"error":{...}} 或 {code:非0,message:...} 表达。网关的三个协议入口共用
// 这一层，所以「解析/判错/累积」只能有一份实现——各写一份必然漂移。
package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ErrReasoningExhausted 表示输出额度被思考吃光：finish_reason=length、
// 正文为空、但思考内容非空。
//
// 实测 sn-sensenova-6-8-flash 对同一句「只回复两个字」的思考量在 90-4000+ token
// 之间波动，把 max_tokens 抬到 1024 仍会偶尔撞上。这时上游不报错，只回一个
// 空正文 + length——客户端看到的是「模型坏了」，实际是预算不够。
var ErrReasoningExhausted = errors.New("思考耗尽输出额度，正文为空")

// ReasoningExhausted 判断一次非流式聚结果是否属于「思考吃光额度」。
func ReasoningExhausted(obj map[string]any) bool {
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return false
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return false
	}
	if finish, _ := choice["finish_reason"].(string); finish != "length" {
		return false
	}
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		return false
	}
	if c, _ := msg["content"].(string); strings.TrimSpace(c) != "" {
		return false
	}
	r, _ := msg["reasoning_content"].(string)
	return strings.TrimSpace(r) != ""
}

// StreamError 是流内业务错误（HTTP 200 但帧里是 error）。
type StreamError struct {
	KindValue      ErrKind
	Status         int
	Msg            string
	RetryAfterSecs int64
	// ServiceUnavailable = serviceAvailable:false：此刻排不上，等也没用。
	ServiceUnavailable bool
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("upstream %s (status %d): %s", e.KindValue, e.Status, e.Msg)
}

// Kind 返回错误分类（供 pool 记账）。
func (e *StreamError) Kind() ErrKind { return e.KindValue }

// AsError 把流错误摊成统一的 *Error，便于上层用一条错误策略处理。
func (e *StreamError) AsError() *Error {
	return &Error{Kind: e.KindValue, Status: e.Status, Msg: e.Msg,
		RetryAfterSecs: e.RetryAfterSecs, ServiceUnavailable: e.ServiceUnavailable}
}

// sseEventKind 是一行 SSE 的语义分类。
type sseEventKind int

const (
	sseSkip sseEventKind = iota
	sseDone
	sseChunk
	sseError
)

// sseEvent 是一条 SSE 事件的解析结果。
type sseEvent struct {
	Kind    sseEventKind
	Chunk   map[string]any // Kind==sseChunk：内层 OpenAI chunk
	Status  int
	Class   ErrKind
	Raw     string
	Message string
	// RetryAfterSecs 是信封里给的重试间隔（排队态），0 = 没给。
	RetryAfterSecs int64
	// ServiceUnavailable = serviceAvailable:false。
	ServiceUnavailable bool
}

// lineBuffer 按字节缓冲 SSE 行：只在完整行上解码，避免把多字节字符切坏。
type lineBuffer struct{ tail []byte }

// push 吃一段上游字节，吐出其中完整的 data: 行（已去掉 data: 前缀）。
func (b *lineBuffer) push(chunk []byte) []string {
	b.tail = append(b.tail, chunk...)
	out := []string{}
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

// finish 在流结束时吐出没有以换行收尾的最后一行。
func (b *lineBuffer) finish() []string {
	if len(b.tail) == 0 {
		return nil
	}
	line := strings.TrimSuffix(string(b.tail), "\r")
	b.tail = nil
	if strings.HasPrefix(line, "data:") {
		data := strings.TrimPrefix(line, "data:")
		if strings.TrimSpace(data) != "" {
			return []string{data}
		}
	}
	return nil
}

// parseSSELine 解析一行 data: 之后的内容。
func parseSSELine(data string) sseEvent {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return sseEvent{Kind: sseSkip}
	}
	if trimmed == "[DONE]" {
		return sseEvent{Kind: sseDone}
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
		return sseEvent{Kind: sseSkip}
	}
	if errObj, ok := chunk["error"].(map[string]any); ok {
		raw := trimmed
		if encoded, err := json.Marshal(errObj); err == nil {
			raw = string(encoded)
		}
		class := classifyErrorText(raw)
		return sseEvent{Kind: sseError, Status: statusOfError(chunk, errObj), Class: class,
			Raw: truncate(raw, 500), Message: messageOf(class),
			RetryAfterSecs: retryAfterOf(raw), ServiceUnavailable: queueUnavailable(raw)}
	}
	if code, ok := intOf(chunk["code"]); ok && code != 0 {
		class := classifyErrorText(trimmed)
		status := 502
		if class == ErrSoftRate {
			status = http.StatusTooManyRequests
		}
		msg := text(chunk, "message", "msg")
		if msg == "" {
			msg = messageOf(class)
		}
		return sseEvent{Kind: sseError, Status: status, Class: class, Raw: truncate(trimmed, 500),
			Message: msg, RetryAfterSecs: retryAfterOf(trimmed), ServiceUnavailable: queueUnavailable(trimmed)}
	}
	return sseEvent{Kind: sseChunk, Chunk: chunk}
}

// statusOfError 取错误对象里的状态码（没有就按分类给）。
func statusOfError(chunk, errObj map[string]any) int {
	for _, src := range []map[string]any{errObj, chunk} {
		for _, key := range []string{"status", "status_code", "statusCode"} {
			if n, ok := intOf(src[key]); ok && n >= 400 {
				return n
			}
		}
	}
	if classifyErrorText(text(errObj, "message", "type")) == ErrSoftRate {
		return http.StatusTooManyRequests
	}
	return 502
}

// classifyErrorText 分类流内错误：与 HTTP 的 Classify 共用同一张判据表。
//
// 这里最容易犯的错是「文案宽判」：`token` 单独出现就记登录态失效——而
// "max_tokens 超限" 之类的报文到处都有，几次下来账号就被自动禁用；
// `insufficient` 单独出现就记额度不足——"context window is insufficient"
// 同样命中，一句参数错换来 12 小时冷却。
func classifyErrorText(raw string) ErrKind {
	lower := strings.ToLower(raw)
	if queueUnavailable(raw) {
		return ErrBusy
	}
	if quotaSignal(lower) {
		return ErrPlanLimit
	}
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many") || strings.Contains(raw, "429") {
		return ErrSoftRate
	}
	if sessionDeadSignal(lower) || strings.Contains(raw, "401") {
		return ErrSessionDead
	}
	if strings.Contains(lower, "server") || strings.Contains(raw, "500") || strings.Contains(raw, "502") {
		return ErrServer
	}
	return ErrNone
}

// messageOf 按分类给出面向用户的中文文案。
func messageOf(kind ErrKind) string {
	switch kind {
	case ErrPlanLimit:
		return "当前账号额度不足或套餐不支持该模型"
	case ErrSoftRate:
		return "请求过于频繁，请稍后重试"
	case ErrSessionDead:
		return "登录态已失效，请重新登录"
	case ErrBusy:
		return "上游模型排队中（模型暂不可服务）：这不是登录态或额度问题"
	case ErrServer:
		return "上游服务异常"
	}
	return "上游拒绝了本次请求"
}

// ---------------------------------------------------------------------------
// 累积器（流式与非流式共用的语义层）

type toolState struct {
	id        string
	name      string
	arguments strings.Builder
}

// accumulator 把内层 chunk 累积成最终的非流式响应。
type accumulator struct {
	id        string
	model     string
	created   int64
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int64]*toolState
	finish    string
	usage     map[string]any
	sawAny    bool
	// forceModel 是客户端请求的模型名：出站帧一律用它，压掉上游的内部名。
	forceModel string
}

func newAccumulator() *accumulator {
	return &accumulator{id: newResponseID(), created: time.Now().Unix(), tools: map[int64]*toolState{}}
}

// setModel 预置出站模型名（首帧到达前就能定）。
func (a *accumulator) setModel(model string) { a.forceModel = model }

// add 吃掉一个内层 chunk，返回需要转发给客户端的增量。
func (a *accumulator) add(chunk map[string]any) []delta {
	if id, ok := chunk["id"].(string); ok && id != "" {
		a.id = id
	}
	if model, ok := chunk["model"].(string); ok && model != "" {
		a.model = model
	}
	if a.forceModel != "" {
		a.model = a.forceModel
	}
	if created, ok := intOf(chunk["created"]); ok && created > 0 {
		a.created = int64(created)
	}
	if usage, ok := chunk["usage"].(map[string]any); ok {
		a.usage = usage
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return nil
	}
	if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
		a.finish = finish
	}
	d, _ := choice["delta"].(map[string]any)
	if d == nil {
		return nil
	}
	out := []delta{}
	if reasoning, ok := d["reasoning_content"].(string); ok && reasoning != "" {
		cleaned := stripThinkingTags(reasoning)
		if cleaned != "" {
			a.reasoning.WriteString(cleaned)
			out = append(out, delta{kind: deltaReasoning, text: cleaned})
			a.sawAny = true
		}
	}
	if content, ok := d["content"].(string); ok && content != "" {
		a.content.WriteString(content)
		out = append(out, delta{kind: deltaContent, text: content})
		a.sawAny = true
	}
	if calls, ok := d["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			index, _ := asInt64(call["index"])
			state := a.tools[index]
			if state == nil {
				state = &toolState{}
				a.tools[index] = state
			}
			id, _ := call["id"].(string)
			if id != "" {
				state.id = id
			}
			function, _ := call["function"].(map[string]any)
			name, _ := function["name"].(string)
			if name != "" {
				state.name = name
			}
			args, _ := function["arguments"].(string)
			state.arguments.WriteString(args)
			out = append(out, delta{kind: deltaToolCall, index: index, id: id, name: name, arguments: args})
			a.sawAny = true
		}
	}
	return out
}

// result 组装非流式 chat.completion。
func (a *accumulator) result() map[string]any {
	message := map[string]any{"role": "assistant", "content": a.content.String()}
	if a.reasoning.Len() > 0 {
		message["reasoning_content"] = a.reasoning.String()
	}
	if len(a.tools) > 0 {
		indexes := make([]int64, 0, len(a.tools))
		for index := range a.tools {
			indexes = append(indexes, index)
		}
		sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
		calls := make([]any, 0, len(indexes))
		for _, index := range indexes {
			state := a.tools[index]
			args := state.arguments.String()
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":   state.id,
				"type": "function",
				"function": map[string]any{
					"name":      state.name,
					"arguments": args,
				},
			})
		}
		message["tool_calls"] = calls
	}
	finish := a.finish
	if len(a.tools) > 0 {
		finish = "tool_calls"
	}
	if finish == "" {
		finish = "stop"
	}
	usage := a.usage
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	return map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": a.created,
		"model":   a.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
}

// delta 是一条要写给客户端的增量。
type delta struct {
	kind      int
	text      string
	index     int64
	id        string
	name      string
	arguments string
}

const (
	deltaContent = iota
	deltaReasoning
	deltaToolCall
)

// ---------------------------------------------------------------------------
// 非流式

// Aggregate 把上游 SSE 汇聚成一个 chat.completion 对象。
func Aggregate(r io.Reader, model string) (map[string]any, error) {
	acc := newAccumulator()
	acc.setModel(model)
	buf := &lineBuffer{}
	reader := bufio.NewReaderSize(r, 64*1024)
	handle := func(data string) (map[string]any, bool, error) {
		event := parseSSELine(data)
		switch event.Kind {
		case sseError:
			return nil, true, (&StreamError{KindValue: event.Class, Status: event.Status, Msg: event.Raw,
				RetryAfterSecs: event.RetryAfterSecs, ServiceUnavailable: event.ServiceUnavailable}).AsError()
		case sseDone:
			if !acc.sawAny {
				return nil, true, ErrEmptyStream
			}
			return acc.result(), true, nil
		case sseChunk:
			acc.add(event.Chunk)
		}
		return nil, false, nil
	}
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			for _, data := range buf.push(raw) {
				if out, done, herr := handle(data); herr != nil {
					return nil, herr
				} else if done {
					return out, nil
				}
			}
		}
		if err == io.EOF {
			for _, data := range buf.finish() {
				if out, done, herr := handle(data); herr != nil {
					return nil, herr
				} else if done {
					return out, nil
				}
			}
			if !acc.sawAny {
				return nil, ErrEmptyStream
			}
			return acc.result(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// ---------------------------------------------------------------------------
// 流式

// Stream 转发 SSE（不关心错误回调）。
func Stream(w http.ResponseWriter, r io.Reader, model string) error {
	_, err := StreamWithError(w, r, nil, model)
	return err
}

// StreamWithError 把上游的内层 chunk 转成 OpenAI SSE 帧写给客户端。
//
// 遇到流内业务错误时回调 onErr（非 nil）并注入错误帧收尾；返回末帧 usage。
// model 是客户端请求的模型名：上游会把帧里的 model 换成它的内部名，出站帧
// 必须改回来（否则客户端会看到 "raccoon-8c4485" 这种它不认识的名字）。
func StreamWithError(w http.ResponseWriter, r io.Reader, onErr func(*StreamError), model string) (map[string]any, error) {
	fl, _ := w.(http.Flusher)
	var wrote bool
	ensureHeaders := func() {
		if wrote {
			return
		}
		wrote = true
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
	}
	write := func(frame map[string]any) error {
		ensureHeaders()
		raw, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, "data: "+string(raw)+"\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	writeRaw := func(s string) error {
		ensureHeaders()
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	acc := newAccumulator()
	acc.setModel(model)
	guard := &reasoningGuard{}
	roleSent := false
	frame := func(d map[string]any, finish string) map[string]any {
		choice := map[string]any{"index": 0, "delta": d}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		return map[string]any{
			"id":      acc.id,
			"object":  "chat.completion.chunk",
			"created": acc.created,
			"model":   acc.model,
			"choices": []any{choice},
		}
	}
	// send 写一帧；首帧前补 role（OpenAI 客户端按它认领流）。
	send := func(d map[string]any, finish string) error {
		if !roleSent {
			roleSent = true
			if err := write(frame(map[string]any{"role": "assistant", "content": ""}, "")); err != nil {
				return err
			}
		}
		chunk := frame(d, finish)
		if acc.usage != nil && finish != "" {
			chunk["usage"] = acc.usage
		}
		return write(chunk)
	}
	done := func() error { return writeRaw("data: [DONE]\n\n") }

	handle := func(event sseEvent) (bool, error) {
		switch event.Kind {
		case sseError:
			se := &StreamError{KindValue: event.Class, Status: event.Status, Msg: event.Message,
				RetryAfterSecs: event.RetryAfterSecs, ServiceUnavailable: event.ServiceUnavailable}
			if onErr != nil {
				onErr(se)
			}
			if !wrote {
				// 一个字节都没发：让调用方决定换号重试。
				return true, se.AsError()
			}
			_ = write(map[string]any{"error": map[string]any{"message": event.Message, "type": "proxy_error", "detail": event.Raw}})
			_ = done()
			return true, nil
		case sseDone:
			return true, nil
		case sseChunk:
			deltas := acc.add(event.Chunk)
			for _, d := range deltas {
				if d.kind != deltaReasoning {
					continue
				}
				hit := guard.Push(d.text)
				if hit == nil {
					continue
				}
				// 思考链机械复读：模型侧问题，不罚号，但要把这段流掐掉——
				// 否则客户端会一直等一段永远重复的思考。
				se := &StreamError{KindValue: ErrReasoningLoop, Status: 502,
					Msg: "思考链复读（" + hit.Kind + "：" + hit.Detail + "），已中断本次生成"}
				if onErr != nil {
					onErr(se)
				}
				_ = write(map[string]any{"error": map[string]any{
					"message": se.Msg, "type": "proxy_error", "code": "upstream_reasoning_loop"}})
				_ = done()
				return true, nil
			}
			for _, d := range deltas {
				payload := map[string]any{}
				switch d.kind {
				case deltaReasoning:
					payload["reasoning_content"] = d.text
				case deltaToolCall:
					call := map[string]any{"index": d.index, "type": "function", "function": map[string]any{}}
					if d.id != "" {
						call["id"] = d.id
					}
					if d.name != "" {
						call["function"].(map[string]any)["name"] = d.name
					}
					if d.arguments != "" {
						call["function"].(map[string]any)["arguments"] = d.arguments
					}
					payload["tool_calls"] = []any{call}
				default:
					payload["content"] = d.text
				}
				if err := send(payload, ""); err != nil {
					return true, err
				}
			}
		}
		return false, nil
	}

	buf := &lineBuffer{}
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			for _, data := range buf.push(raw) {
				stop, herr := handle(parseSSELine(data))
				if herr != nil {
					return nil, herr
				}
				if stop {
					return finishStream(acc, send, done)
				}
			}
		}
		if err == io.EOF {
			for _, data := range buf.finish() {
				stop, herr := handle(parseSSELine(data))
				if herr != nil {
					return nil, herr
				}
				if stop {
					return finishStream(acc, send, done)
				}
			}
			return finishStream(acc, send, done)
		}
		if err != nil {
			return nil, err
		}
	}
}

// finishStream 收尾：空流报错（调用方还能换号），否则补 finish_reason 与 [DONE]。
func finishStream(acc *accumulator, send func(map[string]any, string) error, done func() error) (map[string]any, error) {
	if !acc.sawAny && acc.finish == "" {
		return nil, ErrEmptyStream
	}
	finish := acc.finish
	if len(acc.tools) > 0 {
		finish = "tool_calls"
	}
	if finish == "" {
		finish = "stop"
	}
	// 思考吃光额度：正文一个字都没有，只发了思考。这时代码层面没有 error，
	// 客户端会收到一个「空的正常回复」。补一帧 error 说清楚，比静默空响好。
	if finish == "length" && acc.content.Len() == 0 && acc.reasoning.Len() > 0 && len(acc.tools) == 0 {
		_ = send(map[string]any{"error": map[string]any{
			"message": "思考耗尽输出额度：模型把 max_tokens 全用于思考，正文为空。请提高 max_tokens 后重试",
			"type":    "invalid_request_error",
			"code":    "reasoning_exhausted",
		}}, "stop")
		_ = done()
		return acc.usage, ErrReasoningExhausted
	}
	if err := send(map[string]any{}, finish); err != nil {
		return nil, err
	}
	if err := done(); err != nil {
		return nil, err
	}
	return acc.usage, nil
}

// newResponseID 生成 chatcmpl-<24 hex>。
func newResponseID() string {
	uuid := randomUUIDOrEmpty()
	compact := strings.ReplaceAll(uuid, "-", "")
	if len(compact) > 24 {
		compact = compact[:24]
	}
	if compact == "" {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + compact
}

// stripThinkingTags 去掉文本里可能包裹思考内容的标签。
func stripThinkingTags(text string) string {
	out := text
	for _, pair := range thinkTags {
		out = strings.ReplaceAll(out, pair[0], "")
		out = strings.ReplaceAll(out, pair[1], "")
	}
	return out
}
