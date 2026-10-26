// points_audit.go 积分收入体检：这个号还能从哪拿到分、哪些已经领过了。
//
// 上游的积分来源是「发放制」不是「任务制」（官网定价页：免费版 300 积分/日，
// 每日 0 点重置、不累计），客户端 i18n 里把来源列得很清楚：
//
//	daily        每日积分（0 点发，当天登录会延长有效期到 7 天）
//	monthly      会员每月积分
//	newUser      新人注册礼包
//	desktopLogin 桌面端登录奖励
//	oldUser      老用户回馈
//	other        其它奖励
//
// 所以「自动化」能做的只有三件事：签到领桌面端那一笔、保活延长有效期、
// 把已经领过/还没领过的状态摆出来，避免重复撞墙。
package upstream

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
)

// PointSource 是一个积分来源。
type PointSource struct {
	Key     string `json:"key"`     // new_user / desktop_login / mobile_login / daily / other
	Name    string `json:"name"`    // 中文名
	Points  int64  `json:"points"`  // 已到账合计
	Count   int    `json:"count"`   // 到账笔数
	LastAt  string `json:"last_at"` // 最近一次到账时间
	Claimed bool   `json:"claimed"`
	// Hint 说明这一类还能不能补领。
	Hint string `json:"hint,omitempty"`
}

// PointAudit 是一个号的积分体检结果。
type PointAudit struct {
	UID     string        `json:"uid"`
	Remain  int64         `json:"remain"`
	Daily   int64         `json:"daily"`
	Reward  int64         `json:"reward"`
	Topup   int64         `json:"topup"`
	Expire  int64         `json:"expire"` // unix 秒，0 = 不过期
	Sources []PointSource `json:"sources"`
	// Spent7d 是近 7 天消耗的积分（负数流水取绝对值）。
	Spent7d int64 `json:"spent_7d"`
	// SpentToday 是今天（东八区）已经消耗的积分。
	SpentToday int64 `json:"spent_today"`
	// BurnPerDay 是近 7 天的日均消耗，0 = 不消耗。
	BurnPerDay float64 `json:"burn_per_day"`
	// DaysLeft 是按日均消耗估算的可用天数；-1 = 不消耗（用不完）。
	DaysLeft float64 `json:"days_left"`
	// NextDailyAt 是下一次每日发放的预计时间（次日 00:00 东八区）。
	NextDailyAt string `json:"next_daily_at"`
	// Note 是给人看的结论（例如「今天 300 已到账」「这个号没有每日资格」）。
	Note string `json:"note"`
}

// PointAudit 拉余额 + 近期流水，推出各来源的到账情况。
func (c *Client) PointAudit(ctx context.Context, a *auth.Auth) (*PointAudit, error) {
	usage, uerr := c.UserEntUsageDetailWithExpiry(ctx, a)
	if uerr != nil {
		return nil, uerr
	}
	bills, berr := c.RecentBills(ctx, a, 100)
	if berr != nil {
		// 流水查不到不是致命：余额已经拿到了，就按「什么都没领」展示。
		bills = nil
	}
	audit := &PointAudit{
		UID: a.UID, Remain: usage.Remain, Daily: usage.Daily,
		Reward: usage.Reward, Topup: usage.Topup, Expire: usage.Expire,
	}

	agg := map[string]*PointSource{}
	add := func(key, name string, row BillRow) {
		src := agg[key]
		if src == nil {
			src = &PointSource{Key: key, Name: name}
			agg[key] = src
		}
		src.Points += row.Points
		src.Count++
		if row.CreatedAt > src.LastAt {
			src.LastAt = row.CreatedAt
		}
	}
	today := time.Now().In(shanghaiTZ).Format("2006-01-02")
	dailyToday := false
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for _, row := range bills {
		if row.Points < 0 {
			// 消耗单独统计：近 7 天 + 今天，用来估算还能撑几天。
			at, perr := time.Parse(time.RFC3339, row.CreatedAt)
			if perr != nil {
				continue
			}
			if at.After(cutoff) {
				audit.Spent7d += -row.Points
			}
			if at.In(shanghaiTZ).Format("2006-01-02") == today {
				audit.SpentToday += -row.Points
			}
			continue
		}
		if row.Points == 0 {
			continue
		}
		label := row.EventName
		switch {
		case row.BizType == "daily_grant":
			add("daily", "每日积分", row)
			// created_at 是「2026-10-26T09:42:11+08:00」这种形状，取前 10 位比时间。
			if strings.HasPrefix(row.CreatedAt, today) {
				dailyToday = true
			}
		case strings.Contains(label, "新人"):
			add("new_user", "新人注册礼包", row)
		case strings.Contains(label, "桌面"):
			add("desktop_login", "桌面端登录奖励", row)
		case strings.Contains(label, "手机") || strings.Contains(label, "移动"):
			// 上游流水里写的是「移动端登录奖励」，客户端 i18n 里叫 Mobile，
			// 只匹配「手机」会把它归到 other（实测踩过）。
			add("mobile_login", "移动端登录奖励", row)
		default:
			add("other", "其它奖励", row)
		}
	}

	order := []struct{ key, name, hint string }{
		{"new_user", "新人注册礼包", "一次性，注册时自动发放"},
		{"desktop_login", "桌面端登录奖励", "一次性；网签到时会去领这一笔"},
		{"mobile_login", "移动端登录奖励", "一次性；与桌面端额度独立"},
		{"daily", "每日积分", "每天 0 点发放，当天登录可延长有效期到 7 天"},
		{"other", "其它奖励", "活动/兑换码等"},
	}
	for _, item := range order {
		src := agg[item.key]
		if src == nil {
			src = &PointSource{Key: item.key, Name: item.name}
		}
		src.Claimed = src.Count > 0
		src.Hint = item.hint
		audit.Sources = append(audit.Sources, *src)
	}

	audit.BurnPerDay = float64(audit.Spent7d) / 7
	if audit.BurnPerDay > 0 {
		audit.DaysLeft = float64(audit.Remain) / audit.BurnPerDay
	} else {
		audit.DaysLeft = -1
	}

	now := time.Now().In(shanghaiTZ)
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, shanghaiTZ).Add(24 * time.Hour)
	audit.NextDailyAt = next.Format(time.RFC3339)
	switch {
	case usage.Daily == 0 && !dailyToday:
		audit.Note = fmt.Sprintf("今日 300 分未到账，下一轮 %s", next.Format("01-02 15:04"))
	case dailyToday:
		audit.Note = "今日每日积分已到账"
	default:
		audit.Note = "今日每日积分在账（余额口径）"
	}
	return audit, nil
}
