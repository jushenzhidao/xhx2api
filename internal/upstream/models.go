// models.go 模型目录：GET {llm}/model_catalog 的解析、缓存与别名口径。
//
// 上游形态：
//
//	{code, data:{categories:[{type, models:[{name, description, tags, visible,
//	 context_window, max_tokens, billing_multiplier, params:{...}}]}]}}
//
// context_window / max_tokens 可能是字符串，也可能嵌在 params 里（两个都认），
// 因为上游在两个版本里换过位置。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
)

// ModelInfo 是归一化后的目录条目（面板与 /v1/models 共用的形状）。
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Realm         string `json:"realm"`
	ContextWindow int64  `json:"context_length"`
	MaxOutput     int64  `json:"max_output_tokens"`
	Reasoning     bool   `json:"reasoning"`
	VL            bool   `json:"vision"`
	Credits       string `json:"credits,omitempty"`
	// BillingStatus / FreeNote / BaseCredits 是「限时免费」透传：
	// 上游用 billing_status=limited_free + billing_effective_multiplier=0 标记，
	// 面板要能直接看出「这个模型现在不扣积分」，所以 Credits 改写成 x0，
	// 原倍率留在 BaseCredits 供页面划掉显示。
	BillingStatus string `json:"billing_status,omitempty"`
	FreeNote      string `json:"free_note,omitempty"`
	BaseCredits   string `json:"base_credits,omitempty"`
	Source        string `json:"source,omitempty"`
	UpstreamKey   string `json:"upstream_key,omitempty"`
	Visible       bool   `json:"visible"`
	Category      string `json:"category,omitempty"`
}

// staticModels 是目录拉不到时的兜底（同名同 id，避免面板空表）。
//
// 只放一条：多放几条假模型会让客户端以为能用，点名后上游报「模型不存在」，
// 排查成本比空表高得多。DefaultModel 是客户端实测接受的 id。
func staticModels() []ModelInfo {
	return []ModelInfo{{
		ID: DefaultModel, Name: "Raccoon Chat", Realm: auth.RealmCN,
		ContextWindow: 180_000, MaxOutput: 80_000, Reasoning: true,
		Credits: "x1", Source: "raccoon", UpstreamKey: DefaultModel, Visible: true,
	}}
}

// Catalog 返回目录缓存（进程启动即为兜底表，RefreshModels 之后换成上游真目录）。
func (c *Client) Catalog() []ModelInfo {
	c.catMu.RLock()
	defer c.catMu.RUnlock()
	if len(c.catalog) == 0 {
		return staticModels()
	}
	out := make([]ModelInfo, len(c.catalog))
	copy(out, c.catalog)
	return out
}

// CatalogAge 返回上次成功刷新的时刻（零值 = 从未刷新）。
func (c *Client) CatalogAge() time.Time {
	c.catMu.RLock()
	defer c.catMu.RUnlock()
	return c.catAt
}

// SetCatalog 直接注入目录（持久化缓存/测试用）。
func (c *Client) SetCatalog(models []ModelInfo) {
	if len(models) == 0 {
		return
	}
	c.catMu.Lock()
	c.catalog = models
	if c.catAt.IsZero() {
		// 从磁盘恢复的目录不算「刚刷新」：给它一个偏旧的时刻，让下一次
		// /v1/models 访问触发真正的上游刷新，而不是把陈旧目录当新的用 10 分钟。
		c.catAt = time.Now().Add(-11 * time.Minute)
	}
	c.catMu.Unlock()
}

// LoadCatalogFile 从持久化缓存恢复目录（进程启动时调用）。
//
// 目的：上游偶发不可达时 /v1/models 仍然有真实模型名可列，而不是只剩一条兜底。
func (c *Client) LoadCatalogFile(path string) error {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var payload struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	c.SetCatalog(payload.Models)
	return nil
}

// saveCatalogFile 把目录原子写盘（失败只影响下次启动的兜底，不影响本次请求）。
func (c *Client) saveCatalogFile(path string, models []ModelInfo) {
	if path == "" || len(models) == 0 {
		return
	}
	raw, err := json.MarshalIndent(struct {
		SavedAt time.Time   `json:"saved_at"`
		Models  []ModelInfo `json:"models"`
	}{time.Now(), models}, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// SetCatalogFile 设置持久化路径（空 = 不落盘）。
func (c *Client) SetCatalogFile(path string) {
	c.catMu.Lock()
	c.catalogFile = path
	c.catMu.Unlock()
}

// ResolveModel 按 id 查目录（大小写不敏感；别名由 server 层先归一）。
func (c *Client) ResolveModel(model string) (ModelInfo, bool) {
	model = strings.TrimSpace(model)
	for _, mi := range c.Catalog() {
		if strings.EqualFold(mi.ID, model) {
			return mi, true
		}
	}
	return ModelInfo{}, false
}

// RefreshModels 拉一次上游目录；force=false 时 10 分钟内复用缓存。
func (c *Client) RefreshModels(ctx context.Context, a *auth.Auth, force bool) ([]ModelInfo, error) {
	c.catMu.RLock()
	fresh := len(c.catalog) > 0 && time.Since(c.catAt) < 10*time.Minute
	file := c.catalogFile
	c.catMu.RUnlock()
	if fresh && !force {
		return c.Catalog(), nil
	}
	if a == nil {
		return c.Catalog(), &Error{Kind: ErrClient, Status: 400, Msg: "没有可用账号，无法拉取模型目录"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.llmBase()+CatalogPath, nil)
	if err != nil {
		return c.Catalog(), err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.JWT())
	for key, value := range c.clientIDHeaders() {
		req.Header.Set(key, value)
	}
	resp, err := c.do(req)
	if err != nil {
		return c.Catalog(), err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return c.Catalog(), &Error{Kind: Classify(resp.Status, string(resp.Body)), Status: resp.Status,
			Msg: fmt.Sprintf("拉取模型目录失败（HTTP %d）: %s", resp.Status, truncate(errorText(resp.Body), 200))}
	}
	models, err := parseCatalog(resp.Body)
	if err != nil {
		return c.Catalog(), err
	}
	c.catMu.Lock()
	c.catalog, c.catAt = models, time.Now()
	c.catMu.Unlock()
	c.saveCatalogFile(file, models)
	return models, nil
}

// parseCatalog 平铺 categories[].models；同名首次出现者胜；空 name 丢弃。
func parseCatalog(body []byte) ([]ModelInfo, error) {
	data, err := payloadOf(&apiResponse{Status: 200, Body: body}, "模型目录")
	if err != nil {
		return nil, err
	}
	rawCategories, _ := data["categories"].([]any)
	seen := map[string]bool{}
	out := make([]ModelInfo, 0, 16)
	for _, rawCategory := range rawCategories {
		category, _ := rawCategory.(map[string]any)
		categoryName := text(category, "type", "name", "title")
		models, _ := category["models"].([]any)
		for _, rawModel := range models {
			entry, _ := rawModel.(map[string]any)
			if entry == nil {
				continue
			}
			mi, ok := normalizeEntry(entry)
			if !ok || seen[mi.ID] {
				continue
			}
			mi.Category = categoryName
			seen[mi.ID] = true
			out = append(out, mi)
		}
	}
	if len(out) == 0 {
		// 上游把目录挂在 data.models（没有 categories）时也要认，否则
		// 目录换个信封就整站模型消失。
		if models, ok := data["models"].([]any); ok {
			for _, rawModel := range models {
				entry, _ := rawModel.(map[string]any)
				if entry == nil {
					continue
				}
				mi, ok := normalizeEntry(entry)
				if !ok || seen[mi.ID] {
					continue
				}
				seen[mi.ID] = true
				out = append(out, mi)
			}
		}
	}
	if len(out) == 0 {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "模型目录为空"}
	}
	return out, nil
}

// normalizeEntry 把一条上游目录条目归一化。
func normalizeEntry(entry map[string]any) (ModelInfo, bool) {
	id := text(entry, "name", "model_name", "id")
	if id == "" {
		return ModelInfo{}, false
	}
	name := text(entry, "display_name", "displayDescription", "display_description", "description")
	if name == "" {
		name = id
	}
	params, _ := entry["params"].(map[string]any)
	pick := func(keys ...string) int64 {
		for _, key := range keys {
			if n, ok := int64Of(entry[key]); ok && n > 0 {
				return n
			}
			if params != nil {
				if n, ok := int64Of(params[key]); ok && n > 0 {
					return n
				}
			}
		}
		return 0
	}
	tags := []string{}
	if rawTags, ok := entry["tags"].([]any); ok {
		for _, t := range rawTags {
			if s, ok := t.(string); ok {
				tags = append(tags, s)
			}
		}
	}
	visible := true
	if v, ok := entry["visible"].(bool); ok {
		visible = v
	}
	credits := ""
	if raw, ok := entry["billing_multiplier"]; ok {
		if f, ok := floatOf(raw); ok && f > 0 {
			// FormatFloat -1 会去掉所有尾随 0（"2" 而不是 "2.00"）。
			credits = "x" + strconv.FormatFloat(f, 'f', -1, 64)
		}
	}
	// 限免：上游 billing_status=limited_free（effective 倍率 0）。只认 effective==0
	// 或 status==limited_free，普通折扣（如 eff 0.75）不当限免。
	billingStatus := text(entry, "billing_status")
	freeNote := text(entry, "billing_status_note")
	baseCredits := ""
	if eff, hasEff := floatOf(entry["billing_effective_multiplier"]); billingStatus == "limited_free" || (hasEff && eff == 0) {
		if billingStatus == "limited_free" || credits != "" {
			baseCredits = credits
			credits = "x0"
			if freeNote == "" {
				freeNote = discountNote(entry, "限时免费")
			}
		}
	} else {
		billingStatus = ""
	}
	return ModelInfo{
		ID:            id,
		Name:          name,
		Realm:         auth.RealmCN,
		ContextWindow: pick("context_window", "contextWindow"),
		MaxOutput:     pick("max_tokens", "maxTokens"),
		Reasoning:     tagMatch(tags, "thinking", "reasoning"),
		VL:            tagMatch(tags, "vision", "image", "vl"),
		Credits:       credits,
		BillingStatus: billingStatus,
		FreeNote:      freeNote,
		BaseCredits:   baseCredits,
		Source:        "raccoon",
		UpstreamKey:   id,
		Visible:       visible,
	}, true
}

// discountNote 限免文案兜底：billing_status_note 为空时取 billing_discounts 里
// 名字最像的一条。
func discountNote(entry map[string]any, fallback string) string {
	discounts, _ := entry["billing_discounts"].([]any)
	for _, raw := range discounts {
		d, _ := raw.(map[string]any)
		if d == nil {
			continue
		}
		if active, ok := d["active"].(bool); ok && !active {
			continue
		}
		if name := text(d, "name", "note", "title"); name != "" {
			return name
		}
	}
	return fallback
}

func tagMatch(tags []string, wanted ...string) bool {
	for _, tag := range tags {
		for _, w := range wanted {
			if strings.EqualFold(tag, w) {
				return true
			}
		}
	}
	return false
}
