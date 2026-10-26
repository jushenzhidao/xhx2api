// Package prompt 处理「是否用网关自有提示词替换 system」。
//
// 两种模式：
//
//	passthrough  原样透传（默认）。用户写什么上游就看到什么，最适合编码代理——
//	             它们对 system 的结构敏感，替换会让工具调用退化。
//	custom       用配置文件里的文本替换 system。上游对「非官方客户端」的识别
//	             有一部分来自 system 特征，替换成固定文案能降低被判定为
//	             批量代理的概率；代价是丢掉客户端自己的指令。
package prompt

import (
	"encoding/json"
	"os"
	"strings"
)

// Modes 支持的取值。
const (
	ModePassthrough = "passthrough"
	ModeCustom      = "custom"
)

// Options 是注入参数。
type Options struct {
	Mode string
	Text string
}

// Normalize 校验模式（未知值回落 passthrough，不阻断启动）。
func Normalize(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeCustom:
		return ModeCustom
	default:
		return ModePassthrough
	}
}

// LoadText 读提示词文件；file 为空时返回空串（custom 模式下等同于「删除 system」）。
func LoadText(file string) (string, error) {
	if strings.TrimSpace(file) == "" {
		return "", nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// Apply 按模式改写请求体里的 messages。
//
// 返回改写后的 body 与是否发生了改写。解析失败时原样返回（让上游去报 JSON 错，
// 网关不替它编错误信息）。
func Apply(body []byte, opts Options) ([]byte, bool) {
	if Normalize(opts.Mode) != ModeCustom {
		return body, false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		// Anthropic 入站的 system 是顶层字段；协议桥已经把它折算成 messages，
		// 走到这里说明是 Responses 入站或空对话——两种都不该动。
		return body, false
	}
	out := make([]any, 0, len(messages)+1)
	if strings.TrimSpace(opts.Text) != "" {
		out = append(out, map[string]any{"role": "system", "content": opts.Text})
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			continue // custom 模式：客户端自己的 system 一律丢弃
		}
		out = append(out, msg)
	}
	payload["messages"] = out
	next, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return next, true
}
