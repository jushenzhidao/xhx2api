// points.go 积分收入面板：把「还能从哪拿分 / 哪些已经领过」摆出来，并支持补领。
//
// 上游是发放制不是任务制（详见 upstream/points_audit.go 的说明），所以这里的
// 「任务」只有三件：每日 300 的到账情况、桌面端登录奖励、手机端首登奖励。
// 后两个都是一次性且幂等，补领不会重复加分。
package panel

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
)

// pointsRow 是一个账号的体检结果（含错误）。
type pointsRow struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Audit    any    `json:"audit,omitempty"`
	Error    string `json:"error,omitempty"`
}

// pointsOverview 并发拉所有账号的积分体检（单号 12s 超时，最多 4 并发）。
//
// 走 bills + balance 两个上游请求，号多的时候串行会很慢，所以限并发而不是全并发：
// 上游对同一出口的突发请求有软限流，打太猛会让正常转发也跟着抖。
func (p *Panel) pointsOverview(w http.ResponseWriter, r *http.Request) {
	entries := p.cfg.Pool.List()
	rows := make([]pointsRow, len(entries))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, entry := range entries {
		rows[i] = pointsRow{UID: entry.UID, Nickname: entry.Nickname}
		account := p.cfg.Pool.AuthByUID(entry.UID)
		if account == nil {
			rows[i].Error = "账号不在内存池里"
			continue
		}
		wg.Add(1)
		go func(idx int, acc *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
			defer cancel()
			audit, err := p.cfg.Upstream.PointAudit(ctx, acc)
			if err != nil {
				rows[idx].Error = err.Error()
				return
			}
			rows[idx].Audit = audit
		}(i, account)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"accounts": rows})
}

// pointsClaim 补领一次性奖励（桌面端登录 + 手机端首登）。
//
// 两个接口都是幂等的：领过返回 granted=false。所以这里可以放心重复点，
// 不会把积分刷出来——如果哪天返回 granted=true，那是上游真发了。
func (p *Panel) pointsClaim(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	account := p.cfg.Pool.AuthByUID(uid)
	if account == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	desktop, derr := p.cfg.Upstream.CheckinClaim(ctx, account)
	mobile, merr := p.cfg.Upstream.ClaimMobileGrant(ctx, account)
	out := map[string]any{"uid": uid, "desktop_granted": desktop, "mobile_granted": mobile}
	if derr != nil {
		out["desktop_error"] = derr.Error()
	}
	if merr != nil {
		out["mobile_error"] = merr.Error()
	}
	if desktop || mobile {
		if p.cfg.Logs != nil {
			p.cfg.Logs.Add("补领积分成功：" + uid)
		}
	}
	// 补领成功后顺手刷余额，避免面板还显示旧值。
	if remain, total, _, err := p.cfg.Upstream.UserEntUsage(ctx, account); err == nil {
		p.cfg.Pool.NoteCredits(uid, remain)
		out["remain"] = remain
		out["total"] = total
	}
	status := http.StatusOK
	if derr != nil && merr != nil {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, out)
}
