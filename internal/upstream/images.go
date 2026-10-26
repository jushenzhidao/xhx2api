// Package upstream 的生图通道：POST {llm}/images/gen。
//
// 2026-10-26 实测（真号）：
//   - 请求体是 OpenAI Images 的形状，只有 prompt 必填（上游 Go 结构体
//     RelayImageForm 的 required 校验，空 prompt 直接 400）。
//   - size / quality 会生效：size=1024x1024&quality=high → 1024x1024 high，
//     图更大、image_tokens 从 229 涨到 7050；给非法 size 会被忽略而不是报错。
//   - 响应用 data[0].b64_json 回图（1-2.5MB base64），没有 url 字段。
//   - 模型组是 raccoon-image-gen（LiteLLM），HTTP 层看不到模型名；
//     上游目录 model_catalog 里**不列**图片模型，所以网关把它作为合成条目暴露。
//   - 耗时 20-200s，比对话慢一个数量级；同时并发会抢账号容量（实测与
//     raccoon-8c4485 并发时后者 180s 超时）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/rsfree/xhx2api/internal/auth"
)

// ImagesPath 是生图端点（相对 llmBase）。
const ImagesPath = "/images/gen"

// ImagesEditPath 是图生图端点（multipart/form-data）。
const ImagesEditPath = "/images/edits"

// ImageModelID 是网关暴露给客户端的合成模型名。
//
// 上游没有这个名字（它的模型组叫 raccoon-image-gen，且不在 model_catalog 里），
// 所以网关自己造一个稳定 id，让客户端能在 /v1/models 里发现「这里能生图」。
const ImageModelID = "raccoon-image-gen"

// ImageAliases 是客户端常用的 OpenAI 图片模型名，默认全部接受。
//
// **2026-10-26 用真号实测：上游 /images/gen 完全忽略 model 字段**——
//   - model=definitely-not-an-image-model-xyz → HTTP 200 正常出图
//   - model=gpt-image-2                      → HTTP 200，image_tokens=196
//   - model=gpt-image-2.5                    → HTTP 200，image_tokens=196
//
// 三者（1024x1024、默认档）的 image_tokens 一模一样，响应体里也没有 model
// 字段可回显，说明这条通道只有 LiteLLM 的 raccoon-image-gen 一个组，**没有
// 真正的模型选择能力**。同一轮实测 size 倒是完全生效：
//
//	1536x1024 → 1536x1024 真实像素，158 image_tokens
//	1792x1024 → 1792x1024 真实像素，139 image_tokens
//	1024x1024 → 196 image_tokens（quality 回显 low）
//
// 那为什么还认这些名字？因为 Cherry Studio / OpenWebUI / LobeChat 这类客户端
// 会把 gpt-image-1 / dall-e-3 硬编码成「图片模型」的默认值或候选值。网关要是
// 直接 400，用户看到的就是「明明能生图却不可用」。所以这些名字只当**别名**：
// 能在 /v1/models 里被发现、能路由到同一条通道，但不改变上游实际用的模型。
//
// 想关掉：配置 images.aliases = []（显式空数组；省略该字段则用本默认表）。
var ImageAliases = []string{
	"gpt-image-1",
	"gpt-image-1-mini",
	"gpt-image-2",
	"gpt-image-2.5",
	"dall-e-2",
	"dall-e-3",
}

// EffectiveImageAliases 返回实际生效的别名表。
//
// nil（配置里没写）→ 默认表；非 nil（哪怕是空数组）→ 用调用方给的，
// 这样 images.aliases=[] 能表达「一个别名都不要」。
func EffectiveImageAliases(configured []string) []string {
	if configured == nil {
		return ImageAliases
	}
	out := make([]string, 0, len(configured))
	seen := map[string]bool{strings.ToLower(ImageModelID): true}
	for _, raw := range configured {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, name)
	}
	return out
}

// ImageModelIDs 返回要暴露给客户端的生图模型 id（合成 id 在首位）。
func ImageModelIDs(aliases []string) []string {
	out := make([]string, 0, len(aliases)+1)
	out = append(out, ImageModelID)
	return append(out, EffectiveImageAliases(aliases)...)
}

// IsImageModel 判断名字是否是生图模型（合成 id 或别名，大小写不敏感）。
func IsImageModel(name string, aliases []string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, id := range ImageModelIDs(aliases) {
		if strings.EqualFold(id, name) {
			return true
		}
	}
	return false
}

// imageBodyLimit 是生图响应上限。
//
// 实测 quality=high 的 1024x1024 PNG 约 0.6MB、默认档 1254x1254 约 2.4MB，
// base64 后再涨 33%。给 64MiB 足够覆盖更大的图，同时防住离谱响应。
const imageBodyLimit = 64 << 20

// ImageReply 是一次生图原始响应。
type ImageReply struct {
	Status int
	Body   []byte
}

// Usage 从响应里取出 usage（token 统计），没有就返回 nil。
func (r *ImageReply) Usage() any {
	var payload map[string]any
	if err := json.Unmarshal(r.Body, &payload); err != nil {
		return nil
	}
	return payload["usage"]
}

// MaxImageEdge 是上游允许的最长边。
//
// 2026-10-26 实测：4096x4096 回 400
//
//	Invalid size '4096x4096'. The longest edge must be less than or equal to 3840
//
// 而且是在**等了 106 秒之后**才报——上游先跑了一遍生成才校验尺寸。3840x2160
// （4K UHD，最长边正好 3840）正常出图，实测 147.9s / 371 image_tokens /
// 1635KB PNG。所以边长上限是 3840，不是 4096。
const MaxImageEdge = 3840

// MaxImagePixels 是上游生成侧的总像素预算。
//
// 这条约束比边长更隐蔽：3840x3840 边长合法（=3840），**不报错**，但生成结果
// 会被静默降级成 2880x2880。而 2880×2880 = 8,294,400 = 3840×2160，正好是
// 4K UHD 的像素数——所以真正的上限是总像素，超了不报错、只给一张小图。
//
// 两条约束都要满足：
//   - 最长边 ≤ MaxImageEdge（LiteLLM 硬校验，超了先跑一遍再 400）
//   - 总像素 ≤ MaxImagePixels（生成侧预算，超了静默降级）
const MaxImagePixels = MaxImageEdge * 2160

// ClampImageSize 把超出上游约束的 size 等比压到上限内。
//
// 为什么必须由网关来压：
//   - 边长超限 → 上游先跑 106 秒再回 400，用户白等两分钟；
//   - 像素超限 → 上游不报错，直接给一张比请求小的图，用户以为拿到了 4K。
//
// 两种失败模式都比「图小一点但能拿到」差，所以这里统一提前等比压缩。
// 返回 (新尺寸, 是否改动)。解析不了的原样返回，让上游自己判（它认 auto 之类）。
func ClampImageSize(size string) (string, bool) {
	w, h, ok := parseImageSize(size)
	if !ok {
		return size, false
	}
	ow, oh := w, h
	// 1) 最长边：LiteLLM 的硬校验。
	longest := w
	if h > w {
		longest = h
	}
	if longest > MaxImageEdge {
		w = w * MaxImageEdge / longest
		h = h * MaxImageEdge / longest
	}
	// 2) 总像素：生成侧的静默降级阈值。
	if w*h > MaxImagePixels {
		scale := math.Sqrt(float64(MaxImagePixels) / float64(w*h))
		w = int(float64(w) * scale)
		h = int(float64(h) * scale)
	}
	// 实测上游接受的尺寸（1024/1536/1792/2160/2880/3840）都是 8 的倍数，
	// 对齐到 8，避免踩到「不支持的尺寸」。向下取整只会更小，不会越界。
	if w > 8 {
		w = w / 8 * 8
	}
	if h > 8 {
		h = h / 8 * 8
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	if w == ow && h == oh {
		return size, false
	}
	return fmt.Sprintf("%dx%d", w, h), true
}

// parseImageSize 解析 "3840x2160"（大小写 x 都认）。
func parseImageSize(size string) (w, h int, ok bool) {
	fields := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(size)), func(r rune) bool {
		return r == 'x' || r == '*' || r == '×'
	})
	if len(fields) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(fields[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(fields[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// ImagePromptOf 从请求体里取 prompt（用于日志与必填校验）。
func ImagePromptOf(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if v, ok := payload["prompt"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// GenerateImage 调一次生图。
//
// 用独立的 HTTP 客户端：生图 20-200s，而流式客户端共用 transport 的
// ResponseHeaderTimeout 是 120s——刚好卡在边上。这里给生图更宽的窗口，
// 具体上限由 ctx 决定（handler 按 images.timeout_seconds 设）。
func (c *Client) GenerateImage(ctx context.Context, a *auth.Auth, body []byte, turnTitle string) (*ImageReply, error) {
	return c.postImage(ctx, a, ImagesPath, body, "application/json", turnTitle)
}

// EditImage 转发图生图（POST {llm}/images/edits）。
//
// 上游这个端点只吃 multipart/form-data：实测传 application/json 会回
// 415 unsupported_media_type_error（"only multipart/form-data is supported"），
// 所以 body 与 contentType 都由调用方按原样重建后交进来。
func (c *Client) EditImage(ctx context.Context, a *auth.Auth, body []byte, contentType, turnTitle string) (*ImageReply, error) {
	return c.postImage(ctx, a, ImagesEditPath, body, contentType, turnTitle)
}

// postImage 是生图与改图共用的发送段（只有路径和 Content-Type 不同）。
func (c *Client) postImage(ctx context.Context, a *auth.Auth, path string, body []byte, contentType, turnTitle string) (*ImageReply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.llmBase()+path, bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Kind: ErrServer, Status: 500, Msg: "构造小浣熊生图请求失败"}
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	for key, value := range c.clientIDHeaders() {
		req.Header.Set(key, value)
	}
	req.Header.Set("User-Agent", c.currentUA())
	if title := strings.TrimSpace(turnTitle); title != "" {
		req.Header.Set("X-RACCOON-Title", title)
	}
	if token := a.JWT(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.imageClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Error{Kind: ErrClient, Status: 499, Msg: "客户端已断开"}
		}
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "无法连接小浣熊生图服务"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, imageBodyLimit))
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Error{Kind: ErrClient, Status: 499, Msg: "生图超时"}
		}
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取小浣熊生图响应失败"}
	}
	reply := &ImageReply{Status: resp.StatusCode, Body: raw}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return reply, &Error{
			Kind:   Classify(resp.StatusCode, string(raw)),
			Status: resp.StatusCode,
			Msg:    fmt.Sprintf("小浣熊生图失败（HTTP %d）: %s", resp.StatusCode, truncate(imageErrorText(raw), 200)),
		}
	}
	return reply, nil
}

// imageErrorText 从 OpenAI 形状的错误体里取 message。
//
// 上游是 {"error":{"code":"400","message":"...","type":...}}；超时类错误
// 有时是纯文本，所以解不出来就退回原文。
func imageErrorText(raw []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && strings.TrimSpace(payload.Error.Message) != "" {
		return payload.Error.Message
	}
	return strings.TrimSpace(string(raw))
}

// imageClient 返回生图专用客户端（懒建）。
//
// 直接 clone 流式客户端的 Transport：这样自定义 dialer（出口隔离/代理）、
// TLS 配置都自动继承——否则后注入的 SetDialer 会漏掉生图这条链路。
// 只把 ResponseHeaderTimeout 清零：生图 20-200s，120s 的窗口卡在边上，
// 真正的上限交给 ctx（images.timeout_seconds）。
func (c *Client) imageClient() *http.Client {
	c.imageOnce.Do(func() {
		if c.ImageHTTP != nil {
			return
		}
		if c.StreamHTTP != nil {
			if tr, ok := c.StreamHTTP.Transport.(*http.Transport); ok && tr != nil {
				clone := tr.Clone()
				clone.ResponseHeaderTimeout = 0
				c.ImageHTTP = &http.Client{Transport: clone}
				return
			}
		}
		c.ImageHTTP = &http.Client{Transport: newTransport(0)}
	})
	if c.ImageHTTP != nil {
		return c.ImageHTTP
	}
	return http.DefaultClient
}
