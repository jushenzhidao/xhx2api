// Command livecheck 是网关的「真号联调」验收工具。
//
// 它不碰上游、只打网关自己的 HTTP 接口——这正是用户实际会走的链路，
// 所以能一次覆盖：鉴权、模型目录、账号池、三协议（流式/非流式）、用量记账。
//
// 用法：
//
//	go run ./cmd/livecheck -base http://127.0.0.1:7867 -key sk-xhx-xxx
//	go run ./cmd/livecheck -base ... -key ... -skip-chat   # 只跑不消耗积分的检查
//	go run ./cmd/livecheck -base ... -key ... -model raccoon-chat-ml-5-5 -verbose
//
// 退出码：0 全部通过；1 有失败项（可直接用在 CI / 部署脚本里）。
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// result 是一项检查的结果。
type result struct {
	name    string
	ok      bool
	detail  string
	elapsed time.Duration
	warn    bool
}

func main() {
	base := flag.String("base", "http://127.0.0.1:7867", "网关地址")
	key := flag.String("key", "", "api_key（留空则读环境变量 XHX_API_KEY）")
	model := flag.String("model", "", "指定模型（默认取 /v1/models 的第一个）")
	skipChat := flag.Bool("skip-chat", false, "跳过会消耗积分的对话检查")
	withImage := flag.Bool("image", false, "额外验证生图（慢：20-200s，当前实测不扣分）")
	allModels := flag.Bool("all-models", false, "逐个模型巡检（每个付费模型约 2 分，免费模型 0）")
	verbose := flag.Bool("verbose", false, "打印响应片段")
	timeout := flag.Duration("timeout", 120*time.Second, "单次请求超时")
	flag.Parse()

	if *key == "" {
		*key = os.Getenv("XHX_API_KEY")
	}
	c := &checker{base: strings.TrimRight(*base, "/"), key: *key, verbose: *verbose,
		client: &http.Client{Timeout: *timeout}}

	fmt.Printf("联调目标: %s\n\n", c.base)
	var results []result
	results = append(results, c.checkHealth())
	results = append(results, c.checkModels(*model))
	if m := c.pickModel(*model); m != "" {
		c.model = m
	}
	results = append(results, c.checkOverview())
	if !*skipChat && c.model != "" {
		results = append(results, c.checkChatStream())
		results = append(results, c.checkChatNonStream())
		results = append(results, c.checkMessages())
		results = append(results, c.checkResponses())
	}
	if *allModels {
		results = append(results, c.checkAllModels())
	}
	if *withImage {
		results = append(results, c.checkImage())
	}
	results = append(results, c.checkUsage())

	fmt.Println()
	fmt.Println("──────────── 汇总 ────────────")
	failed := 0
	warned := 0
	for _, r := range results {
		mark := "✓"
		switch {
		case !r.ok && r.warn:
			mark = "!"
			warned++
		case !r.ok:
			mark = "✗"
			failed++
		}
		fmt.Printf("%s %-22s %6s  %s\n", mark, r.name, r.elapsed.Round(time.Millisecond), r.detail)
	}
	fmt.Println()
	if failed > 0 {
		fmt.Printf("结果：%d 项失败，%d 项警告\n", failed, warned)
		os.Exit(1)
	}
	if warned > 0 {
		fmt.Printf("结果：全部通过（%d 项警告需人工确认）\n", warned)
		return
	}
	fmt.Println("结果：全部通过")
}

type checker struct {
	base    string
	key     string
	client  *http.Client
	model   string
	verbose bool
	// promptTokens/completionTokens 累计（最后一项检查用）。
	promptTokens     int64
	completionTokens int64
}

// do 发一次请求。
func (c *checker) do(method, path string, body any, headers map[string]string) (int, []byte, time.Duration, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return 0, nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	started := time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, time.Since(started), err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, time.Since(started), nil
}

func (c *checker) checkHealth() result {
	code, raw, dur, err := c.do(http.MethodGet, "/healthz", nil, nil)
	if err != nil {
		return result{"健康检查", false, "请求失败: " + err.Error(), dur, false}
	}
	if code != 200 {
		return result{"健康检查", false, fmt.Sprintf("HTTP %d: %s", code, snippet(raw)), dur, false}
	}
	return result{"健康检查", true, "200 OK", dur, false}
}

// checkModels 验证模型目录。
//
// 只拿到 1 条 = 目录从没成功从上游拉过（回落到内置兜底），这在真号联调里
// 是个警告：说明账号或网络有问题，但不阻塞对话。
func (c *checker) checkModels(explicit string) result {
	code, raw, dur, err := c.do(http.MethodGet, "/v1/models", nil, nil)
	if err != nil {
		return result{"模型目录", false, "请求失败: " + err.Error(), dur, false}
	}
	if code != 200 {
		return result{"模型目录", false, fmt.Sprintf("HTTP %d: %s", code, snippet(raw)), dur, false}
	}
	var payload struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
			MaxOutput     int64  `json:"max_output_tokens"`
			Credits       string `json:"credits"`
			BillingStatus string `json:"billing_status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return result{"模型目录", false, "响应不是 JSON: " + snippet(raw), dur, false}
	}
	n := len(payload.Data)
	if n == 0 {
		return result{"模型目录", false, "列表为空（账号不可用或目录拉取失败）", dur, false}
	}
	if explicit == "" {
		// 打印前几个模型，方便肉眼确认是不是真目录。
		names := make([]string, 0, 4)
		for i, m := range payload.Data {
			if i >= 4 {
				break
			}
			names = append(names, m.ID)
		}
		detail := fmt.Sprintf("%d 个模型（%s…）", n, strings.Join(names, ", "))
		if n == 1 {
			return result{"模型目录", false,
				fmt.Sprintf("%s —— 只有 1 条，疑似仍在用内置兜底目录（上游拉取失败？）", detail), dur, true}
		}
		return result{"模型目录", true, detail, dur, false}
	}
	for _, m := range payload.Data {
		if m.ID == explicit {
			return result{"模型目录", true, fmt.Sprintf("指定模型 %s 存在（上下文 %d / 上限 %d）",
				m.ID, m.ContextLength, m.MaxOutput), dur, false}
		}
	}
	return result{"模型目录", false, fmt.Sprintf("指定模型 %s 不在目录中", explicit), dur, false}
}

// pickModel 选一个用于对话的模型。
func (c *checker) pickModel(explicit string) string {
	if explicit != "" {
		return explicit
	}
	code, raw, _, err := c.do(http.MethodGet, "/v1/models", nil, nil)
	if err != nil || code != 200 {
		return ""
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &payload) != nil || len(payload.Data) == 0 {
		return ""
	}
	// 优先选非兜底模型（名字带 chat/ml 的通常是主力对话模型）。
	for _, m := range payload.Data {
		if strings.Contains(m.ID, "chat") {
			return m.ID
		}
	}
	return payload.Data[0].ID
}

// checkOverview 看账号池：有没有号、积分多少、今天签没签。
func (c *checker) checkOverview() result {
	code, raw, dur, err := c.do(http.MethodGet, "/panel/api/overview", nil, nil)
	if err != nil {
		return result{"账号池", false, "请求失败: " + err.Error(), dur, false}
	}
	if code != 200 {
		return result{"账号池", false, fmt.Sprintf("HTTP %d: %s", code, snippet(raw)), dur, false}
	}
	var payload struct {
		Counts struct {
			Total   int `json:"total"`
			Healthy int `json:"healthy"`
			Cooling int `json:"cooling"`
		} `json:"counts"`
		Accounts []struct {
			UID       string `json:"uid"`
			Nickname  string `json:"nickname"`
			Credits   int64  `json:"credits"`
			CheckedIn string `json:"checked_in"`
			Cooldown  string `json:"cooldown_kind"`
			LastError string `json:"last_error"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return result{"账号池", false, "响应不是 JSON", dur, false}
	}
	if payload.Counts.Total == 0 {
		return result{"账号池", false, "没有账号，请先在面板导入", dur, false}
	}
	if payload.Counts.Healthy == 0 {
		msg := fmt.Sprintf("%d 个账号但全部不可用（冷却 %d）", payload.Counts.Total, payload.Counts.Cooling)
		if len(payload.Accounts) > 0 && payload.Accounts[0].LastError != "" {
			msg += "；最近错误: " + truncate(payload.Accounts[0].LastError, 80)
		}
		return result{"账号池", false, msg, dur, false}
	}
	parts := make([]string, 0, len(payload.Accounts))
	for _, a := range payload.Accounts {
		name := a.Nickname
		if name == "" {
			name = a.UID
		}
		part := fmt.Sprintf("%s %d分", name, a.Credits)
		if a.CheckedIn != "" {
			part += " 已签"
		}
		if a.Cooldown != "" {
			part += " 冷却:" + a.Cooldown
		}
		parts = append(parts, part)
	}
	return result{"账号池", true,
		fmt.Sprintf("%d 个账号（可用 %d）: %s", payload.Counts.Total, payload.Counts.Healthy, strings.Join(parts, " | ")),
		dur, false}
}

// checkChatStream 流式对话：验证 SSE 帧、[DONE]、内容非空。
func (c *checker) checkChatStream() result {
	code, raw, dur, err := c.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    c.model,
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "只回复两个字：收到"}},
	}, nil)
	if err != nil {
		return result{"对话（流式）", false, "请求失败: " + err.Error(), dur, false}
	}
	if code != 200 {
		return result{"对话（流式）", false, fmt.Sprintf("HTTP %d: %s", code, snippet(raw)), dur, false}
	}
	var text strings.Builder
	frames, done := 0, false
	var usage map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			continue
		}
		frames++
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
			if delta, ok := choices[0].(map[string]any)["delta"].(map[string]any); ok {
				if s, ok := delta["content"].(string); ok {
					text.WriteString(s)
				}
			}
		}
	}
	if !done {
		return result{"对话（流式）", false, fmt.Sprintf("收到 %d 帧但缺少 [DONE]", frames), dur, false}
	}
	if text.Len() == 0 {
		return result{"对话（流式）", false, "流正常结束但内容为空", dur, false}
	}
	c.addUsage(usage)
	detail := fmt.Sprintf("%d 帧，回复「%s」，耗时 %s", frames, truncate(text.String(), 30), dur.Round(time.Millisecond))
	if c.verbose {
		detail += fmt.Sprintf("（原始 %d 字节）", len(raw))
	}
	return result{"对话（流式）", true, detail, dur, false}
}

// checkChatNonStream 非流式对话。
func (c *checker) checkChatNonStream() result {
	code, raw, dur, err := c.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":      c.model,
		"max_tokens": 16,
		"messages":   []any{map[string]any{"role": "user", "content": "只回复两个字：收到"}},
	}, nil)
	if err != nil {
		return result{"对话（非流式）", false, "请求失败: " + err.Error(), dur, false}
	}
	if code != 200 {
		return result{"对话（非流式）", false, fmt.Sprintf("HTTP %d: %s", code, snippet(raw)), dur, false}
	}
	var payload struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return result{"对话（非流式）", false, "响应不是 JSON: " + snippet(raw), dur, false}
	}
	if len(payload.Choices) == 0 {
		return result{"对话（非流式）", false, "choices 为空", dur, false}
	}
	content := payload.Choices[0].Message.Content
	reasoning := payload.Choices[0].Message.ReasoningContent
	if strings.TrimSpace(content) == "" {
		if strings.TrimSpace(reasoning) != "" {
			// 思考型模型把 max_tokens 全烧在 reasoning 上。网关有 1024 的下限，
			// 走到这里说明下限没生效或问题确实很复杂。
			return result{"对话（非流式）", false,
				fmt.Sprintf("正文为空、思考 %d 字（finish=%s）—— max_tokens 被思考吃光",
					len([]rune(reasoning)), payload.Choices[0].FinishReason), dur, false}
		}
		return result{"对话（非流式）", false, "内容为空（finish_reason=" + payload.Choices[0].FinishReason + "）", dur, false}
	}
	c.addUsage(payload.Usage)
	return result{"对话（非流式）", true,
		fmt.Sprintf("模型回显 %s，回复「%s」", payload.Model, truncate(content, 30)), dur, false}
}

// checkMessages Anthropic 入口（Claude Code 用的那条）。
func (c *checker) checkMessages() result {
	code, raw, dur, err := c.do(http.MethodPost, "/v1/messages", map[string]any{
		"model":      c.model,
		"max_tokens": 32,
		"messages":   []any{map[string]any{"role": "user", "content": "只回复两个字：收到"}},
	}, map[string]string{"x-api-key": c.key})
	if err != nil {
		return result{"Anthropic 入口", false, "请求失败: " + err.Error(), dur, false}
	}
	if code != 200 {
		return result{"Anthropic 入口", false, fmt.Sprintf("HTTP %d: %s", code, snippet(raw)), dur, false}
	}
	var payload struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      map[string]any
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return result{"Anthropic 入口", false, "响应不是 JSON: " + snippet(raw), dur, false}
	}
	if payload.Type != "message" || payload.Role != "assistant" {
		return result{"Anthropic 入口", false, fmt.Sprintf("形状不对: type=%s role=%s", payload.Type, payload.Role), dur, false}
	}
	text := ""
	for _, block := range payload.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	if strings.TrimSpace(text) == "" {
		return result{"Anthropic 入口", false, "content 里没有 text 块", dur, false}
	}
	return result{"Anthropic 入口", true,
		fmt.Sprintf("回复「%s」，stop_reason=%s", truncate(text, 26), payload.StopReason), dur, false}
}

// checkResponses OpenAI Responses 入口（Codex 用的那条）。
func (c *checker) checkResponses() result {
	code, raw, dur, err := c.do(http.MethodPost, "/v1/responses", map[string]any{
		"model":             c.model,
		"max_output_tokens": 32,
		"input":             "只回复两个字：收到",
	}, nil)
	if err != nil {
		return result{"Responses 入口", false, "请求失败: " + err.Error(), dur, false}
	}
	if code != 200 {
		return result{"Responses 入口", false, fmt.Sprintf("HTTP %d: %s", code, snippet(raw)), dur, false}
	}
	var payload struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return result{"Responses 入口", false, "响应不是 JSON: " + snippet(raw), dur, false}
	}
	if payload.Object != "response" {
		return result{"Responses 入口", false, "object 不是 response: " + snippet(raw), dur, false}
	}
	text := ""
	reasoningLen := 0
	for _, item := range payload.Output {
		if item.Type == "reasoning" {
			for _, part := range item.Summary {
				reasoningLen += len([]rune(part.Text))
			}
			for _, part := range item.Content {
				reasoningLen += len([]rune(part.Text))
			}
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" {
				text += part.Text
			}
		}
	}
	if strings.TrimSpace(text) == "" {
		if reasoningLen > 0 {
			return result{"Responses 入口", false,
				fmt.Sprintf("没有 output_text，只有 %d 字思考—— max_output_tokens 被思考吃光", reasoningLen), dur, false}
		}
		return result{"Responses 入口", false, "output 里没有 output_text", dur, false}
	}
	return result{"Responses 入口", true,
		fmt.Sprintf("回复「%s」，status=%s", truncate(text, 26), payload.Status), dur, false}
}

// checkUsage 确认用量真的记下来了（不然面板永远是 0）。
func (c *checker) checkUsage() result {
	code, raw, dur, err := c.do(http.MethodGet, "/panel/api/usage", nil, nil)
	if err != nil {
		return result{"用量记账", false, "请求失败: " + err.Error(), dur, true}
	}
	if code != 200 {
		return result{"用量记账", false, fmt.Sprintf("HTTP %d", code), dur, true}
	}
	var payload struct {
		Lifetime struct {
			Requests   int64 `json:"requests"`
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		} `json:"lifetime"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return result{"用量记账", false, "响应不是 JSON", dur, true}
	}
	if payload.Lifetime.Requests == 0 {
		return result{"用量记账", false, "请求数仍为 0（用量记录器没接上？）", dur, true}
	}
	return result{"用量记账", true,
		fmt.Sprintf("累计 %d 次请求，%d 输入 / %d 输出 tokens",
			payload.Lifetime.Requests, payload.Lifetime.Prompt, payload.Lifetime.Completion), dur, false}
}

func (c *checker) addUsage(usage map[string]any) {
	if usage == nil {
		return
	}
	pick := func(keys ...string) int64 {
		for _, k := range keys {
			switch v := usage[k].(type) {
			case float64:
				return int64(v)
			case json.Number:
				n, _ := v.Int64()
				return n
			}
		}
		return 0
	}
	c.promptTokens += pick("prompt_tokens", "input_tokens")
	c.completionTokens += pick("completion_tokens", "output_tokens")
}

func snippet(raw []byte) string {
	return truncate(strings.TrimSpace(string(raw)), 200)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// checkImage 验证 /v1/images/generations：要拿到真的 PNG，且响应里带 usage。
// 生图慢，所以单独给 5 分钟超时，不共用对话的 120s。
//
// 请求里故意填 dall-e-3：它是个「客户端别名」（上游实测忽略 model，网关也不转发），
// 顺便验证别名能被接受而不是 400。
//
// 默认形态是 url（网关把图落盘并给 /v1/images/<id>），所以这里两种都认：
// 有 url 就去下载再验魔数——那条路顺便把静态路由和落盘一起验了；
// 只有 base64 时（关掉 return_url，或客户端显式要 b64_json）解出来验。
func (c *checker) checkImage() result {
	const prompt = "一个纯色圆形，扁平图标风格"
	body := map[string]any{"model": "dall-e-3", "prompt": prompt, "size": "1024x1024"}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.base+"/v1/images/generations", bytes.NewReader(raw))
	if err != nil {
		return result{name: "生图", detail: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	started := time.Now()
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	elapsed := time.Since(started)
	if err != nil {
		return result{name: "生图", detail: "请求失败: " + err.Error(), elapsed: elapsed}
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return result{name: "生图", detail: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(payload), 160)), elapsed: elapsed}
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
		Size  string         `json:"size"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return result{name: "生图", detail: "响应不是 JSON: " + truncate(string(payload), 120), elapsed: elapsed}
	}
	if len(out.Data) == 0 {
		return result{name: "生图", detail: "data 为空", elapsed: elapsed}
	}
	item := out.Data[0]
	shape := "b64_json"
	var png []byte
	switch {
	case item.URL != "":
		shape = "url"
		img, ierr := fetchImage(item.URL)
		if ierr != nil {
			return result{name: "生图", detail: "下载 url 失败: " + ierr.Error(), elapsed: elapsed}
		}
		png = img
	case item.B64 != "":
		blob, derr := base64.StdEncoding.DecodeString(item.B64)
		if derr != nil {
			return result{name: "生图", detail: "base64 解不开: " + derr.Error(), elapsed: elapsed}
		}
		png = blob
	default:
		return result{name: "生图", detail: "既没有 url 也没有 b64_json", elapsed: elapsed}
	}
	if len(png) < 8 || string(png[1:4]) != "PNG" {
		return result{name: "生图", detail: "返回的不是 PNG（魔数不对）", elapsed: elapsed}
	}
	total, _ := out.Usage["total_tokens"].(float64)
	return result{
		name:    "生图",
		ok:      true,
		detail:  fmt.Sprintf("%s · %.0fKB · %d tokens · %s", out.Size, float64(len(png))/1024, int64(total), shape),
		elapsed: elapsed,
	}
}

// fetchImage 下载网关给的图片地址（静态路由不带鉴权，浏览器/下载器都这么用）。
func fetchImage(url string) ([]byte, error) {
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 4K PNG 约 5MB，留一点余量即可；避免被异常响应喂爆内存。
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// checkAllModels 逐个模型发一句「只回复两个字」，验证可用性并记录耗时/思考开销。// checkAllModels 逐个模型发一句「只回复两个字」，验证可用性并记录耗时/思考开销。
//
// 为什么值得单独跑：上游目录会变，而且「能列出来」不等于「能调用」。
// 实测 sn-* 全是思考型（一句「收到」也要烧 100-200 个思考 token），
// raccoon-* 不思考但首字慢——这两类混在一个池子里，选型前得先看清。
func (c *checker) checkAllModels() result {
	started := time.Now()
	code, raw, _, err := c.do(http.MethodGet, "/v1/models", nil, nil)
	if err != nil || code != 200 {
		return result{name: "模型巡检", detail: fmt.Sprintf("拉模型列表失败: HTTP %d %v", code, err), elapsed: time.Since(started)}
	}
	var payload struct {
		Data []struct {
			ID   string `json:"id"`
			Caps struct {
				Image bool `json:"image"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return result{name: "模型巡检", detail: "模型列表不是 JSON", elapsed: time.Since(started)}
	}
	type row struct {
		id       string
		ok       bool
		dur      time.Duration
		tokens   int64
		thinking int64
		note     string
	}
	var rows []row
	bad := 0
	for _, m := range payload.Data {
		// 生图模型（合成 id 与 gpt-image-* / dall-e-* 别名）不能用 /chat 验：
		// 网关会按设计回 400，当成失败会把巡检结果搞脏。它们交给 -image。
		if m.ID == "raccoon-image-gen" || m.Caps.Image {
			continue
		}
		body := map[string]any{
			"model":      m.ID,
			"max_tokens": 256,
			"messages":   []any{map[string]any{"role": "user", "content": "只回复两个字：收到"}},
		}
		t0 := time.Now()
		st, resp, _, cerr := c.do(http.MethodPost, "/v1/chat/completions", body, nil)
		el := time.Since(t0)
		r := row{id: m.ID, dur: el}
		if cerr != nil || st != 200 {
			r.note = fmt.Sprintf("HTTP %d %v", st, cerr)
			dur := el
			r.dur = dur
			bad++
			rows = append(rows, r)
			continue
		}
		var out struct {
			Choices []struct {
				Message struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"message"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal(resp, &out); err != nil {
			r.note = "响应不是 JSON"
			bad++
			rows = append(rows, r)
			continue
		}
		content := ""
		thinking := 0
		if len(out.Choices) > 0 {
			content = strings.TrimSpace(out.Choices[0].Message.Content)
			thinking = len([]rune(out.Choices[0].Message.ReasoningContent))
		}
		r.ok = content != ""
		r.thinking = int64(thinking)
		if v, ok := out.Usage["total_tokens"].(float64); ok {
			r.tokens = int64(v)
		}
		if !r.ok {
			r.note = "正文为空"
			bad++
		}
		rows = append(rows, r)
	}

	fmt.Println("\n模型巡检：")
	for _, r := range rows {
		mark := "✓"
		if !r.ok {
			mark = "✗"
		}
		extra := ""
		if r.thinking > 0 {
			extra = fmt.Sprintf(" 思考 %d 字", r.thinking)
		}
		if r.note != "" {
			extra += " " + r.note
		}
		fmt.Printf("  %s %-30s %6s %5d tokens%s\n", mark, r.id, r.dur.Round(time.Millisecond), r.tokens, extra)
	}
	if bad > 0 {
		return result{name: "模型巡检", detail: fmt.Sprintf("%d/%d 不可用", bad, len(rows)), elapsed: time.Since(started)}
	}
	return result{name: "模型巡检", ok: true, detail: fmt.Sprintf("%d 个全部可用", len(rows)), elapsed: time.Since(started)}
}
