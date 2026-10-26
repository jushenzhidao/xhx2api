// Package session 实现会话粘性路由：同一段对话尽量打到同一个账号。
//
// 为什么需要：小浣熊的 chat 是无状态的，换号不影响正确性；但同一会话在多个
// 账号之间跳，会让上游看到「同一段上下文短时间内在多个登录态下出现」，这在
// 风控视角里很像共享/爬取流量。粘性把抖动压到「会话级」而不是「请求级」。
//
// 两个键来源：
//   - 客户端显式给了会话 id（x-session-id / conversation_id / prompt_cache_key）；
//   - 没给就用消息前缀指纹（system + 首条 user 的 hash）。
//
// 前缀指纹有个天然缺陷：客户端常把时间戳、工作目录写进 system，每次都变，
// 于是永远凑不成一对。所以面板同时暴露「前缀种数」与「新会话数」，让用户能
// 区分「样本不够」和「指纹不稳定」。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Config 是路由参数。
type Config struct {
	TTL        time.Duration // 绑定有效期
	GCInterval time.Duration // 清理周期
}

// binding 是一条会话 → 账号的绑定。
type binding struct {
	uid      string
	expireAt time.Time
}

// Router 是并发安全的粘性路由表。
type Router struct {
	cfg Config

	mu       sync.Mutex
	bindings map[string]binding
	// prefixes 记录见过的前缀指纹种类数（只记 key，不记内容）。
	prefixes map[string]struct{}
	sessions int64
	hits     int64
	misses   int64
	stop     chan struct{}
	stopOnce sync.Once
}

// New 建路由表并起 GC。
func New(cfg Config) *Router {
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	r := &Router{
		cfg:      cfg,
		bindings: map[string]binding{},
		prefixes: map[string]struct{}{},
		stop:     make(chan struct{}),
	}
	go r.gcLoop()
	return r
}

// Close 停止 GC。
func (r *Router) Close() {
	r.stopOnce.Do(func() { close(r.stop) })
}

func (r *Router) gcLoop() {
	ticker := time.NewTicker(r.cfg.GCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.gc()
		}
	}
}

func (r *Router) gc() {
	now := time.Now()
	r.mu.Lock()
	for key, b := range r.bindings {
		if b.expireAt.Before(now) {
			delete(r.bindings, key)
		}
	}
	r.mu.Unlock()
}

// Lookup 查会话绑定的账号；未绑定/已过期返回空。
func (r *Router) Lookup(sessionKey string) (string, bool) {
	if sessionKey == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.bindings[sessionKey]
	if !ok || b.expireAt.Before(time.Now()) {
		if ok {
			delete(r.bindings, sessionKey)
		}
		r.misses++
		return "", false
	}
	b.expireAt = time.Now().Add(r.cfg.TTL) // 滑动续期
	r.bindings[sessionKey] = b
	r.hits++
	return b.uid, true
}

// Bind 把会话绑到账号（已有绑定则覆盖）。
func (r *Router) Bind(sessionKey, uid string) {
	if sessionKey == "" || uid == "" {
		return
	}
	r.mu.Lock()
	r.bindings[sessionKey] = binding{uid: uid, expireAt: time.Now().Add(r.cfg.TTL)}
	r.sessions++
	r.mu.Unlock()
}

// Forget 解除会话绑定（账号冷却/失败换号时调用）。
func (r *Router) Forget(sessionKey string) {
	if sessionKey == "" {
		return
	}
	r.mu.Lock()
	delete(r.bindings, sessionKey)
	r.mu.Unlock()
}

// NotePrefix 记录一个前缀指纹（用于「前缀种数」观测）。
func (r *Router) NotePrefix(prefix string) {
	if prefix == "" {
		return
	}
	r.mu.Lock()
	r.prefixes[prefix] = struct{}{}
	r.mu.Unlock()
}

// Stats 返回观测数据。
type Stats struct {
	Bindings   int     `json:"bindings"`
	Prefixes   int     `json:"prefixes"`
	Sessions   int64   `json:"sessions"`
	Hits       int64   `json:"hits"`
	Misses     int64   `json:"misses"`
	HitRate    float64 `json:"hit_rate"`
	TTLSeconds int     `json:"ttl_seconds"`
	Enabled    bool    `json:"enabled"`
}

// Stats 取当前观测。
func (r *Router) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Stats{
		Bindings: len(r.bindings), Prefixes: len(r.prefixes),
		Sessions: r.sessions, Hits: r.hits, Misses: r.misses,
		TTLSeconds: int(r.cfg.TTL.Seconds()), Enabled: true,
	}
	if total := r.hits + r.misses; total > 0 {
		st.HitRate = float64(r.hits) / float64(total)
	}
	return st
}

// Key 从请求与请求体推导会话键。
//
// 优先级：显式头 > 请求体里的 conversation/prompt_cache_key > 消息前缀指纹。
// 显式头优先，是因为客户端自己最清楚「哪些请求属于同一个会话」；前缀指纹只是
// 没头可用时的启发式。
func Key(headers func(string) string, body []byte) (key string, prefix string) {
	for _, name := range []string{"X-Session-Id", "X-Conversation-Id", "Conversation-Id"} {
		if v := strings.TrimSpace(headers(name)); v != "" {
			return "sid:" + v, ""
		}
	}
	var payload map[string]any
	if len(body) > 0 && json.Unmarshal(body, &payload) == nil {
		for _, field := range []string{"prompt_cache_key", "conversation_id", "session_id", "user"} {
			if v, ok := payload[field].(string); ok && strings.TrimSpace(v) != "" {
				return "sid:" + strings.TrimSpace(v), ""
			}
		}
		if messages, ok := payload["messages"].([]any); ok {
			return "", prefixKey(messages)
		}
		// Responses API 用 input 而不是 messages。
		if input, ok := payload["input"]; ok {
			return "", "p:" + hashJSON(input)
		}
	}
	return "", ""
}

// prefixKey 用「system + 前两条消息」做指纹。
//
// 只取前两条：再往后每次都会变（用户新的问题），取进来等于没有指纹；
// system 与首轮问题在同一次对话里是稳定的。
func prefixKey(messages []any) string {
	var sb strings.Builder
	count := 0
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content := flattenContent(msg["content"])
		sb.WriteString(role)
		sb.WriteString("\x00")
		sb.WriteString(content)
		sb.WriteString("\x00")
		count++
		if count >= 2 {
			break
		}
	}
	if sb.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return "p:" + hex.EncodeToString(sum[:8])
}

// flattenContent 把 content（字符串或多模态数组）拍平成文本用于哈希。
func flattenContent(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case []any:
		var sb strings.Builder
		for _, item := range typed {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := part["text"].(string); ok {
				sb.WriteString(t)
			}
			if t, ok := part["type"].(string); ok {
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// hashJSON 把任意值哈希成短指纹。
func hashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
