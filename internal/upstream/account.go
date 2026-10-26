// account.go 账号侧口径：积分、每日签到、凭证导入与授权码换票。
//
// 小浣熊只有一套账号体系：
//
//	GET  {points}/balance   积分（available_points 及四个来源分类）
//	GET  {points}/bills     逐笔发放/消费流水（带 expire_at，可对账）
//	POST {origin}/api/web/desktop/v1/login/points/grant   桌面端登录领取（幂等）
//	POST {origin}/api/web/mobile/v1/login/points/grant    手机端首登奖励（另一笔）
//
// 每日 +300 是服务端 00:00（东八区）自动发放，不存在「调一下接口就发分」的
// 客户端开关。grant 是登录弹窗式的手动领取，领过回 granted:false。所以「签到
// 成功」的判定以 bills 里当天有 daily_grant 为准，接口返回码只作参考。
package upstream

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
)

// 计量单位：小浣熊按积分计费。
const (
	UnitCredit = "积分"
	UnitUSD    = "USD"
)

// EntUsage 是额度快照（面板/调度器共用）。
type EntUsage struct {
	Remain int64 `json:"remain"`
	Total  int64 `json:"total"`
	// Expire 是最早一批会作废的每日积分的到期时刻（unix 秒）；0 = 未知。
	Expire int64 `json:"expire,omitempty"`
	// ExpirePoints 是上面那个时刻会作废的积分额（只算每日批次，不含不会过期的奖励/充值）。
	ExpirePoints int64   `json:"expire_points,omitempty"`
	Daily        int64   `json:"daily,omitempty"`
	Monthly      int64   `json:"monthly,omitempty"`
	Reward       int64   `json:"reward,omitempty"`
	Topup        int64   `json:"topup,omitempty"`
	Unit         string  `json:"unit,omitempty"`
	UsedUSD      float64 `json:"used_usd,omitempty"`
	LimitUSD     float64 `json:"limit_usd,omitempty"`
}

// ExpireAt 返回到期时刻（毫秒），满足池/面板的统一接口。
func (u EntUsage) ExpireAt() int64 {
	if u.Expire <= 0 {
		return 0
	}
	return u.Expire * 1000
}

// RemainUSD 是面板的美元口径占位：小浣熊只有积分，恒 0。
func (u EntUsage) RemainUSD() float64 { return 0 }

// CreditPackage 是面板额度页的打包形状。
type CreditPackage struct {
	Name   string `json:"name"`
	Unit   string `json:"unit"`
	Remain int64  `json:"remain"`
	Size   int64  `json:"size"`
	Expire int64  `json:"expire,omitempty"`
}

// FetchQuota 返回原始额度节点（面板排障用）。
func (c *Client) FetchQuota(ctx context.Context, a *auth.Auth) (map[string]any, error) {
	resp, err := c.doJSON(ctx, http.MethodGet, c.pointsBase()+BalancePath, nil, c.authHeader(a))
	if err != nil {
		return nil, err
	}
	return payloadOf(resp, "额度查询")
}

// FetchPlan 返回订阅节点：小浣熊没有订阅接口，交给同一个额度响应兜住。
func (c *Client) FetchPlan(ctx context.Context, a *auth.Auth) (map[string]any, error) {
	return c.FetchQuota(ctx, a)
}

// UserEntUsageDetail 查一次积分余额并归一成 EntUsage。
func (c *Client) UserEntUsageDetail(ctx context.Context, a *auth.Auth) (EntUsage, error) {
	data, err := c.FetchQuota(ctx, a)
	if err != nil {
		return EntUsage{}, err
	}
	usage := EntUsage{
		Unit:    UnitCredit,
		Remain:  mustInt(data["available_points"]),
		Daily:   mustInt(data["daily_points"]),
		Monthly: mustInt(data["monthly_points"]),
		Reward:  mustInt(data["reward_points"]),
		Topup:   mustInt(data["topup_points"]),
	}
	if usage.Remain < 0 {
		return EntUsage{}, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊额度响应里没有 available_points"}
	}
	usage.Total = usage.Remain
	a.SetCredit(usage.Remain)
	return usage, nil
}

// UserEntUsage 是老接口形状（remain,total,expire）。
func (c *Client) UserEntUsage(ctx context.Context, a *auth.Auth) (remain, total, expire int64, err error) {
	u, err := c.UserEntUsageDetail(ctx, a)
	return u.Remain, u.Total, u.Expire, err
}

// BalancePackages 返回面板额度页需要的钱包明细。
//
// 上游没有「资源包」概念，构成页口径 = balance 的四个来源分类（每日/每月会员/
// 奖励/充值），每桶 remain=size（余额本身没有「面额」概念，两者一致就是诚实口径）。
// 「可用积分」不单独成包——它就是四桶之和，再进列表会把构成条撑爆、重复计数。
// 全 0 的桶不输出：否则前端把每个空桶归进「已用完」，纯噪音。
func (c *Client) BalancePackages(ctx context.Context, a *auth.Auth) ([]CreditPackage, EntUsage, error) {
	usage, err := c.UserEntUsageDetail(ctx, a)
	if err != nil {
		return nil, EntUsage{}, err
	}
	dailyExpire := c.dailyBatchExpiry(ctx, a)
	wallet := []struct {
		name   string
		value  int64
		expire int64
	}{
		{"每日积分", usage.Daily, dailyExpire},
		{"每月会员积分", usage.Monthly, 0},
		{"奖励积分", usage.Reward, 0},
		{"充值积分", usage.Topup, 0},
	}
	packs := make([]CreditPackage, 0, len(wallet))
	for _, w := range wallet {
		if w.value <= 0 {
			continue
		}
		packs = append(packs, CreditPackage{Name: w.name, Unit: UnitCredit, Remain: w.value, Size: w.value, Expire: w.expire})
	}
	return packs, usage, nil
}

// dailyBatchExpiry 查 bills，取每日发放批次里最早的**未过期**到期时刻（unix 秒）。
// 失败静默返回 0（构成页照常显示余额，只是到期分布缺这一列）。
func (c *Client) dailyBatchExpiry(ctx context.Context, a *auth.Auth) int64 {
	resp, err := c.doJSON(ctx, http.MethodGet, c.pointsBase()+BillsPath+"?"+BillsQuery, nil, c.authHeader(a))
	if err != nil {
		return 0
	}
	data, err := payloadOf(resp, "积分流水")
	if err != nil {
		return 0
	}
	now := time.Now()
	var earliest int64
	items, _ := data["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil || text(item, "biz_type") != "daily_grant" {
			continue
		}
		exp := parseRFC3339(text(item, "expire_at"))
		if exp <= 0 || time.Unix(exp, 0).Before(now) {
			continue
		}
		if earliest == 0 || exp < earliest {
			earliest = exp
		}
	}
	return earliest
}

// UserEntUsageDetailWithExpiry 在余额之外补上「最早会作废的那批积分」。
//
// balance 接口完全不返回到期时间（只有余额桶），所以模型里的 Expire 永远是 0，
// 面板「到期」列与到期提醒卡片就一直是空的。真正带到期语义的只有积分流水：
// 每笔 daily_grant 都有自己的 expire_at，上游按「失效时刻先后」扣减（FEFO），
// 所以「最近会作废的那批」= 所有未过期批次里最早的那个 expire_at。
func (c *Client) UserEntUsageDetailWithExpiry(ctx context.Context, a *auth.Auth) (EntUsage, error) {
	usage, err := c.UserEntUsageDetail(ctx, a)
	if err != nil {
		return usage, err
	}
	expire, points := c.earliestBatchExpiry(ctx, a)
	usage.Expire, usage.ExpirePoints = expire, points
	return usage, nil
}

// earliestBatchExpiry 返回「最早过期的那批每日积分」的到期时刻与它的积分额。
//
// 只报时刻不够：账号余额里往往混着不会过期的奖励/充值积分，面板若拿可用总额
// 去算「哪天会作废多少」，会得出「6490 分 5 天后全没」这种吓人的假数。
func (c *Client) earliestBatchExpiry(ctx context.Context, a *auth.Auth) (expire, points int64) {
	resp, err := c.doJSON(ctx, http.MethodGet, c.pointsBase()+BillsPath+"?"+BillsQuery, nil, c.authHeader(a))
	if err != nil {
		return 0, 0
	}
	data, err := payloadOf(resp, "积分流水")
	if err != nil {
		return 0, 0
	}
	now := time.Now()
	byExpire := map[int64]int64{}
	items, _ := data["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil || text(item, "biz_type") != "daily_grant" {
			continue
		}
		exp := parseRFC3339(text(item, "expire_at"))
		if exp <= 0 || time.Unix(exp, 0).Before(now) {
			continue
		}
		pts := mustInt(item["points"])
		if pts <= 0 {
			continue
		}
		byExpire[exp] += pts
	}
	for exp, pts := range byExpire {
		if expire == 0 || exp < expire {
			expire, points = exp, pts
		}
	}
	return expire, points
}

// parseRFC3339 把上游时间戳（2006-01-02T15:04:05+08:00）转 unix 秒；解析不了返回 0。
func parseRFC3339(s string) int64 {
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
		return t.Unix()
	}
	return 0
}

// 东八区时区（无 tzdata 环境回落固定 +8）。
var shanghaiTZ = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}()

// CheckinStatus 查「今日每日发放是否已到账」。
//
// 口径是 bills 流水里今天有没有 daily_grant；流水查不到时退回余额的
// daily_points>0（不精确但比「一律说没签」强）。
func (c *Client) CheckinStatus(ctx context.Context, a *auth.Auth) (checkedIn bool, credits int64, err error) {
	usage, err := c.UserEntUsageDetail(ctx, a)
	if err != nil {
		return false, 0, err
	}
	credits = usage.Remain
	if today, _, berr := c.DailyGrant(ctx, a); berr == nil {
		checkedIn = today
	} else {
		checkedIn = usage.Daily > 0
	}
	if checkedIn {
		a.MarkCheckedIn()
	}
	return checkedIn, credits, nil
}

// DailyGrant 读 bills 流水：今天（东八区）是否已有 daily_grant 入账，以及这个号
// **历史上有没有过** daily_grant。
//
// 后一半是给「上游压根没给这个号开每日积分」用的：实测有的号注册多日、bills 里
// 只有 reward_grant（注册礼包/桌面端登录奖励），daily_points 恒为 0。这种号
// claim 再多轮也不会有 daily_grant，报「签到失败」是谎报。
func (c *Client) DailyGrant(ctx context.Context, a *auth.Auth) (today, ever bool, err error) {
	resp, err := c.doJSON(ctx, http.MethodGet, c.pointsBase()+BillsPath+"?"+BillsQuery, nil, c.authHeader(a))
	if err != nil {
		return false, false, err
	}
	data, err := payloadOf(resp, "积分流水")
	if err != nil {
		return false, false, err
	}
	day := time.Now().In(shanghaiTZ).Format("2006-01-02")
	items, _ := data["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil || text(item, "biz_type") != "daily_grant" {
			continue
		}
		ever = true
		grantAt := text(item, "grant_at", "created_at")
		if len(grantAt) >= 10 && grantAt[:10] == day {
			today = true
		}
	}
	return today, ever, nil
}

// CheckinClaim 走桌面端登录领取接口（上游幂等：今日已发放则 granted=false）。
//
// 只有 granted=true 才记本地台账——发放本体是服务端 00:00 做的，这里只是
// 兜住「登录弹窗式手动领取」那条路径。
func (c *Client) CheckinClaim(ctx context.Context, a *auth.Auth) (bool, error) {
	resp, err := c.doJSON(ctx, http.MethodPost, c.desktopBase()+"/login/points/grant", map[string]any{}, c.authHeader(a))
	if err != nil {
		return false, err
	}
	data, err := payloadOf(resp, "登录领取积分")
	if err != nil {
		return false, err
	}
	granted, _ := data["granted"].(bool)
	if granted {
		a.MarkCheckedIn()
	}
	return granted, nil
}

// ClaimMobileGrant 领手机端首登奖励（一次性新手福利的另一笔，与桌面端额度独立）。
//
// 端点与桌面端同名不同域（/api/web/mobile/v1），身份头换成官方安卓 App 那两个；
// token 与桌面端是同一把 Bearer——JWT claims 里没有平台字段（实测确认）。
// granted=false 表示该号在手机端已领过：同样是「已结算」，调用方不必再问。
func (c *Client) ClaimMobileGrant(ctx context.Context, a *auth.Auth) (bool, error) {
	h := c.authHeader(a)
	// APK 逆向：平台值是 Capacitor getPlatform() 加 app- 前缀，版本头带 v。
	h["x-client-platform"] = "app-android"
	h["x-client-version"] = "v1.0.3"
	resp, err := c.doJSON(ctx, http.MethodPost, c.originBase()+"/api/web/mobile/v1"+MobileGrantPath, map[string]any{}, h)
	if err != nil {
		return false, err
	}
	data, err := payloadOf(resp, "手机端首登奖励")
	if err != nil {
		return false, err
	}
	granted, _ := data["granted"].(bool)
	return granted, nil
}

// BillRow 一条积分流水（只保留对账要用的字段）。
type BillRow struct {
	BillID    string `json:"bill_id"`
	BizID     string `json:"biz_id"`
	BizType   string `json:"biz_type"`
	EventName string `json:"event_name"`
	Points    int64  `json:"points"` // 消费为负，发放为正
	CreatedAt string `json:"created_at"`
}

// RecentBills 拉最近若干条积分流水（含消费行），用于把「每次请求到底扣了多少分」
// 对回面板记录：消费行的 event_name 会带上我们出站时的 X-RACCOON-Title，
// points 是负值。
func (c *Client) RecentBills(ctx context.Context, a *auth.Auth, limit int) ([]BillRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	query := fmt.Sprintf("paging.limit=%d&type=all&paging.offset=0", limit)
	resp, err := c.doJSON(ctx, http.MethodGet, c.pointsBase()+BillsPath+"?"+query, nil, c.authHeader(a))
	if err != nil {
		return nil, err
	}
	data, err := payloadOf(resp, "积分流水")
	if err != nil {
		return nil, err
	}
	items, _ := data["items"].([]any)
	out := make([]BillRow, 0, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		out = append(out, BillRow{
			BillID:    text(item, "bill_id"),
			BizID:     text(item, "biz_id"),
			BizType:   text(item, "biz_type"),
			EventName: text(item, "event_name"),
			Points:    mustInt(item["points"]),
			CreatedAt: text(item, "created_at"),
		})
	}
	return out, nil
}

// UserCreatedAt 取账号注册时间。
//
// 只用来区分「上游今天还没发每日积分」和「签到了但没落账」：每日积分发放是
// 服务端 00:00 的定时任务，当天新注册的号要等次日首批，此前 bills 里根本没有
// daily_grant——那不是签到失败，是还没到发放时点。
func (c *Client) UserCreatedAt(ctx context.Context, a *auth.Auth) (time.Time, bool) {
	if a == nil || a.JWT() == "" {
		return time.Time{}, false
	}
	resp, err := c.doJSON(ctx, http.MethodGet, c.authBase()+UserInfoPath, nil,
		map[string]string{"Accept": "application/json", "Authorization": "Bearer " + a.JWT()})
	if err != nil {
		return time.Time{}, false
	}
	data, err := payloadOf(resp, "用户资料")
	if err != nil {
		return time.Time{}, false
	}
	raw := text(data, "created_at", "register_time", "createdAt")
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, perr := time.Parse(layout, raw); perr == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ---------------------------------------------------------------------------
// 导入与授权码

// PrepareAccount 把面板粘贴的内容变成账号：授权回调链接换票，或直接是凭证 JSON。
//
// payload 来自面板的导入端点：
//
//	{"access_token":"…","refresh_token":"…"}
//	{"callback":"office-raccoon://auth/callback?code=…&state=…"}
//	{"code":"ac_…"}
func (c *Client) PrepareAccount(ctx context.Context, payload map[string]any) (*auth.Auth, error) {
	if raw, ok := payload["callback"].(string); ok && strings.TrimSpace(raw) != "" {
		code, _, valid := ParseCallbackCode(raw)
		if !valid {
			return nil, &Error{Kind: ErrClient, Status: 400, Msg: "回调链接不合法，应形如 office-raccoon://auth/callback?code=…&state=…"}
		}
		return c.exchangeCode(ctx, code)
	}
	if code, ok := payload["code"].(string); ok && strings.TrimSpace(code) != "" {
		return c.exchangeCode(ctx, strings.TrimSpace(code))
	}
	access := text(payload, "access_token", "accessToken", "token")
	refresh := text(payload, "refresh_token", "refreshToken")
	if access == "" && refresh == "" {
		return nil, &Error{Kind: ErrClient, Status: 400,
			Msg: "无法识别的内容：请粘贴小浣熊账号 JSON（access_token / refresh_token），或 office-raccoon:// 授权回调链接"}
	}
	account := &auth.Auth{Data: auth.Data{
		AccessToken:  stripBearerToken(access),
		RefreshToken: stripBearerToken(refresh),
		UID:          text(payload, "uid", "user_id", "userId"),
		Nickname:     text(payload, "nickname", "name"),
		ExpiresAt:    millisOf(payload["expires_at"]),
	}}
	if account.AccessToken == "" {
		// 只给了 refresh_token：先换一次票，顺带验证它还能用。
		probe := &auth.Auth{Data: auth.Data{UID: account.UID, RefreshToken: account.RefreshToken}}
		if err := c.RefreshToken(ctx, probe); err != nil {
			return nil, err
		}
		account.AccessToken, account.ExpiresAt = probe.JWT(), probe.ExpiresAtValue()
	}
	if account.ExpiresAt == 0 {
		account.ExpiresAt = auth.JWTExpiry(account.AccessToken)
	}
	if uid, nick, _, err := c.GetUserInfo(ctx, account); err == nil {
		if uid != "" {
			account.UID = uid
		}
		if account.Nickname == "" {
			account.Nickname = nick
		}
	}
	if account.UID == "" {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: auth.ErrNoIdentity.Error()}
	}
	if account.Nickname == "" {
		account.Nickname = account.UID
	}
	return account, nil
}

// exchangeCode 用一次性授权码换凭证。
func (c *Client) exchangeCode(ctx context.Context, code string) (*auth.Auth, error) {
	resp, err := c.doJSON(ctx, http.MethodPost, c.authBase()+LoginCodePath,
		map[string]string{"authorization_code": code}, c.clientIDHeaders())
	if err != nil {
		return nil, err
	}
	if biz, ok := businessCodeOf(resp.Body); ok && biz == CodeAuthExpired {
		return nil, ErrCodeExpired
	}
	data, err := payloadOf(resp, "授权码换票")
	if err != nil {
		return nil, err
	}
	access := text(data, "access_token", "accessToken")
	refresh := text(data, "refresh_token", "refreshToken")
	if access == "" || refresh == "" {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊未返回完整凭证（access_token/refresh_token）"}
	}
	account := &auth.Auth{Data: auth.Data{
		AccessToken:  stripBearerToken(access),
		RefreshToken: stripBearerToken(refresh),
		Nickname:     text(data, "office_org_name", "office_identity"),
		ExpiresAt:    auth.JWTExpiry(access),
	}}
	// 授权码换票的响应里带 user_id，但那是**平台 id**（pu-…，iam 域的），不是
	// office 域的用户 id——同一个人在 token 导入路径里存的是 4465567/7719383
	// 这种 office id，两条路用不同 id 会让同一个号在池子里出现两行。
	// 所以这里以 user_info 的 id 为准，换票响应里的 user_id 只当兜底。
	account.UID = orDefault(text(data, "user_id", "uid", "userId"), auth.JWTUserID(access))
	if uid, nick, _, uerr := c.GetUserInfo(ctx, account); uerr == nil {
		if uid != "" {
			account.UID = uid
		}
		if nick != "" {
			account.Nickname = nick
		}
	}
	if account.UID == "" {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊授权码换票未返回用户标识"}
	}
	if account.Nickname == "" {
		account.Nickname = account.UID
	}
	return account, nil
}

// ParseCallbackCode 从 office-raccoon://auth/callback?code=..&state=.. 取 code/state。
// 只认该 scheme+host+path，避免别的链接被当成回调。
func ParseCallbackCode(raw string) (code, state string, ok bool) {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexAny(raw, "\r\n"); i >= 0 {
		raw = raw[:i]
	}
	// 裸授权码（ac_…）直接认：用户在浏览器里可能只截到 code。
	if strings.HasPrefix(raw, "ac_") && !strings.Contains(raw, "://") && !strings.Contains(raw, "?") {
		return raw, "", true
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	query := parsed.Query()
	switch {
	case strings.EqualFold(parsed.Scheme, "office-raccoon"):
		// 官方客户端回调：office-raccoon://auth/callback?code=…&state=…
		if parsed.Host != "auth" || parsed.Path != "/callback" {
			return "", "", false
		}
	case parsed.Scheme == "http" || parsed.Scheme == "https":
		// 环回回调：http://127.0.0.1:<port>/auth/callback?code=…
		// 面板部署在服务器/局域网时，浏览器打不开这个本机地址，但地址栏里的链接
		// 是完整可用的——用户把它整条粘过来即可（host 不限 127.0.0.1：反正路径形状
		// 是我们自己的）。
		if parsed.Path != "/auth/callback" {
			return "", "", false
		}
	default:
		return "", "", false
	}
	code = query.Get("code")
	if code == "" {
		code = query.Get("authorization_code")
	}
	return code, query.Get("state"), code != ""
}

// AuthorizeURL 构造网页授权页（面板展示给人手动打开用）。
func (c *Client) AuthorizeURL(appName string) string {
	if appName == "" {
		appName = "办公小浣熊客户端"
	}
	query := url.Values{}
	query.Set("login_source", "desktop")
	query.Set("client", "desktop")
	query.Set("app_name", appName)
	return c.originBase() + "/code/authorize?" + query.Encode()
}

// ---------------------------------------------------------------------------
// 浏览器环回授权（不需要官方客户端，也不需要人工粘贴）

// DeviceLogin 是浏览器授权会话。
//
// 官方网页授权页本来就支持给「非官方桌面客户端」投递授权码：
//
//	/code/authorize?login_source=desktop&appname=…&state=<64 hex>
//	                &redirect=http://127.0.0.1:<port>/auth/callback
//	                &desktop_callback=loopback
//
// 页面校验：redirect 必须匹配 ^http://127\.0\.0\.1:[1-9][0-9]{3,4}/auth/callback$、
// state 必须 64 位小写 hex、且必须带 desktop_callback=loopback。同一处还认
// vscode://sensetime.raccoon——这条通道是给 IDE/第三方客户端用的公开扩展点，
// 我们复用它，而不是去逆向客户端二进制。
//
// 于是流程是：面板给出授权页 URL → 用户在自己浏览器登录（图形验证码由页面自己做）
// → 页面把 code 打到我们的 /auth/callback → 面板 exchangeCode 换票入池。
type DeviceLogin struct {
	client *Client
	state  string
	url    string

	mu   sync.Mutex
	code string // 回调已经送到的授权码（单次使用）
}

// ErrLoopbackUnsupported 说明回调基址不可用。
var ErrLoopbackUnsupported = errors.New(
	"回调基址必须是 http(s)://<host>[:port]/auth/callback 形状；请检查 login.callback_base 配置")

// validCallback 校验回调基址形状。
//
// 上游 authorize 页早期只认 `http://127.0.0.1:<1024-65535>/auth/callback`
// （参考实现的注释如此），但 2026-10-26 实测：把 redirect 换成
// `https://xhx2api.1task.cn/auth/callback` 后页面**原样透传**该地址，
// 没有回退到 office-raccoon:// —— 说明域名回调是被接受的。
// 所以这里不再硬卡 127.0.0.1，只校验「http(s) + /auth/callback」这个形状。
//
// 保底：即便某些客户端分支会拒绝域名，面板的「粘贴回调链接」仍然可用
// （ParseCallbackCode 对 host 不设限），所以放宽不会把用户堵死。
func validCallback(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	if parsed.Host == "" {
		return false
	}
	return parsed.Path == "/auth/callback"
}

// NewDeviceLogin 建一次浏览器授权会话，callbackBase 形如 http://127.0.0.1:17867。
func NewDeviceLogin(client *Client, callbackBase, appName string) (*DeviceLogin, error) {
	if client == nil {
		return nil, ErrLoopbackUnsupported
	}
	state, err := randState()
	if err != nil {
		return nil, err
	}
	callback := strings.TrimRight(callbackBase, "/") + "/auth/callback"
	if !validCallback(callback) {
		return nil, ErrLoopbackUnsupported
	}
	if appName == "" {
		appName = "办公小浣熊客户端"
	}
	query := url.Values{}
	query.Set("login_source", "desktop")
	query.Set("appname", appName)
	query.Set("state", state)
	query.Set("redirect", callback)
	query.Set("desktop_callback", "loopback")
	return &DeviceLogin{client: client, state: state, url: client.originBase() + "/code/authorize?" + query.Encode()}, nil
}

// randState 生成 32 字节随机数的 hex（64 位小写），与网页端 state 校验同形。
func randState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// State 既是面板轮询用的会话 id，也是回调要核对的 state。
func (d *DeviceLogin) State() string { return d.state }

// AuthURL 返回授权页地址，面板展示 + 一键打开。
func (d *DeviceLogin) AuthURL() string { return d.url }

// AcceptCallback 收下浏览器送回的授权码（state 必须与本会话一致，且只收一次）。
func (d *DeviceLogin) AcceptCallback(state, code string) error {
	if subtle.ConstantTimeCompare([]byte(state), []byte(d.state)) != 1 {
		return errors.New("state 不匹配：这条回调不属于当前登录会话")
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("回调里没有 code")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.code != "" {
		return errors.New("这个登录会话已经用过了，请回面板重新添加")
	}
	d.code = code
	return nil
}

// Poll 还没收到回调 → (nil,false,nil)；收到了 → 换票并返回凭证（授权码一次性，取走即清）。
func (d *DeviceLogin) Poll(ctx context.Context) (*auth.Auth, bool, error) {
	d.mu.Lock()
	code := d.code
	d.code = ""
	d.mu.Unlock()
	if code == "" {
		return nil, false, nil
	}
	creds, err := d.client.exchangeCode(ctx, code)
	if err != nil {
		return nil, false, err
	}
	return creds, true, nil
}

// stripBearerToken 去 "Bearer " 前缀。
func stripBearerToken(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return v
}

// mustInt 读整数字段；缺失/非法返回 -1（与「真的是 0」区分开）。
func mustInt(value any) int64 {
	if value == nil {
		return -1
	}
	if n, ok := asInt64(value); ok {
		return n
	}
	return -1
}

// businessCodeOf 从响应体里取业务码（没有返回 false）。
func businessCodeOf(body []byte) (int, bool) {
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0, false
	}
	return intOf(envelope["code"])
}
