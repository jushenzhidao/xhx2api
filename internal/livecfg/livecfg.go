// Package livecfg 保存「面板改完立即生效」的运行期配置。
//
// 为什么要单独一层：进程启动时装配进各种对象的值（HTTP client 超时、监听地址、
// 账号目录）改不了；但 api_key、软冷却基数、模型黑名单、来源记录开关这类纯读
// 参数可以放进一个原子快照里，每个请求读一次。这样面板保存后下一个请求就生效，
// 不必为了改一个密钥重启整个网关（重启会打断在途的流式生成）。
package livecfg

import (
	"strings"
	"sync/atomic"
)

// Snapshot 是一次读取看到的全部热配置。
type Snapshot struct {
	// APIKey 空 = 不鉴权（仅本机/私网）。
	APIKey string
	// SoftCooldown 是 429 软冷却基数（连续触发指数退避，封顶在 pool 里）。
	SoftCooldown int64 // 纳秒
	// HiddenModels 是模型黑名单（裸名 = 隐藏；"cn:xxx" 形式为域前缀，小浣熊
	// 只有一个域，所以前缀会被剥掉后按裸名处理）。
	HiddenModels []string
	// RecordClientInfo 是否在请求日志里记录客户端 IP / User-Agent。
	RecordClientInfo bool
	// PromptMode / PromptText 是 system 提示词注入策略。
	PromptMode string
	PromptText string
	// Aliases 把客户端通用名映射到上游 id；DefaultModel 是请求未给 model 时用的名字。
	//
	// 这两项原本只在启动时注入，改完别名必须重启才生效——而面板明明有别名编辑框，
	// 保存后还回报「无需重启」，结果短名继续 400（2026-10-26 实测踩过）。
	// 放进快照后，下一个请求就能用上新别名。
	Aliases      map[string]string
	DefaultModel string
	// ImageAliases 是生图通道接受的模型别名（nil = 用 upstream.ImageAliases 默认表）。
	//
	// 放进快照的理由同上：面板改 images.aliases 后应该立刻反映在 /v1/models
	// 与对话接口的指路文案里，不该逼用户重启。
	ImageAliases []string
}

// Holder 是原子快照容器。
type Holder struct {
	v atomic.Value // 存 *Snapshot
}

// New 建一个带初始值的容器。
func New(initial Snapshot) *Holder {
	h := &Holder{}
	cp := initial
	h.v.Store(&cp)
	return h
}

// Load 读当前快照（永不返回 nil）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if v := h.v.Load(); v != nil {
		return *v.(*Snapshot)
	}
	return Snapshot{}
}

// Store 整体替换快照。
//
// 刻意做成「整体替换」而不是逐字段更新：一次保存的多个字段应当同时生效，
// 逐字段更新会出现「新 api_key 已生效、旧黑名单还在」的中间态。
func (h *Holder) Store(next Snapshot) {
	if h == nil {
		return
	}
	next.HiddenModels = NormalizeHiddenModels(next.HiddenModels)
	cp := next
	h.v.Store(&cp)
}

// NormalizeHiddenModels 清洗黑名单：去空白、去重、剥掉域前缀（小浣熊单域）。
func NormalizeHiddenModels(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		// "cn:xxx" / "global:xxx" → xxx：保留对老配置的兼容。
		if i := strings.Index(name, ":"); i > 0 {
			prefix := strings.ToLower(name[:i])
			if prefix == "cn" || prefix == "global" || prefix == "intl" {
				name = strings.TrimSpace(name[i+1:])
			}
		}
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		out = append(out, name)
	}
	return out
}

// Alias 查别名（大小写不敏感，与网关侧 lookupAlias 行为一致）。
func (s Snapshot) Alias(name string) (string, bool) {
	if len(s.Aliases) == 0 {
		return "", false
	}
	if v, ok := s.Aliases[name]; ok {
		return v, true
	}
	lower := strings.ToLower(name)
	for k, v := range s.Aliases {
		if strings.ToLower(k) == lower {
			return v, true
		}
	}
	return "", false
}

// Hidden 判断模型是否在黑名单里（大小写不敏感）。
func (s Snapshot) Hidden(model string) bool {
	return hiddenIn(s.HiddenModels, model)
}

// hiddenIn 是 Hidden 的纯函数版本（便于测试）。
func hiddenIn(list []string, model string) bool {
	for _, name := range list {
		if strings.EqualFold(name, model) {
			return true
		}
	}
	return false
}
