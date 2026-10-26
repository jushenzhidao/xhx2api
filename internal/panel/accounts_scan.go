// accounts_scan.go 两条「批量/免手工」的加号路径：
//
//  1. 扫本机客户端目录：桌面端把票明文写在 Application Support 里，
//     直接读出来比让人工挖 token 靠谱得多（换机器/重装也能一键捡回来）。
//  2. 手机号验证码登录：走上游 /send_sms + /login_with_sms。
//     这条链路唯一的人工环节是阿里云验证码——票据由浏览器里的控件产出，
//     所以前端要真跑一次控件（见 index.html 的 sms 页），后端只负责加密与转发。
package panel

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/rsfree/xhx2api/internal/clientdir"
	"github.com/rsfree/xhx2api/internal/upstream"
)

// captchaConfig 是登录页内联配置里的阿里云验证码参数（2026-10-26 实测值）。
//
// 前端拿到它才能 initAliyunCaptcha；这三个值由上游下发、可能随版本变，
// 所以放在接口里返回而不是写死在前端——换值时只改这里。
type captchaConfig struct {
	Scene  string `json:"scene"`
	Prefix string `json:"prefix"`
	Region string `json:"region"`
}

var defaultCaptcha = captchaConfig{Scene: "1pkmy0x3", Prefix: "hk1r5l", Region: "cn"}

// scanClient 扫描本机客户端凭证。
//
// 只读：不改任何文件、不删任何东西。读出来的票会拿去做一次 user_info
// （幂等 GET），既验证票是否还活着，也把 IAM 主体（pu-…）换成 office 域数字 uid。
func (p *Panel) scanClient(w http.ResponseWriter, r *http.Request) {
	locations := clientdir.Locations(p.cfg.ClientDirs)
	paths := make([]string, 0, len(locations))
	for _, loc := range locations {
		if loc.Exists {
			paths = append(paths, loc.Path)
		}
	}
	candidates := clientdir.Scan(paths)

	type view struct {
		clientdir.Candidate
		AlreadyImported bool   `json:"already_imported"`
		VerifyError     string `json:"verify_error,omitempty"`
	}
	out := make([]view, 0, len(candidates))
	for _, cand := range candidates {
		item := view{Candidate: cand}
		if cand.Err != "" || cand.Token == "" {
			out = append(out, item)
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		profile, err := p.cfg.Upstream.FetchProfile(ctx, cand.Token)
		cancel()
		if err != nil {
			item.VerifyError = friendlyVerifyError(err)
		} else {
			if profile.ID != "" {
				item.UID = profile.ID
			}
			if profile.Name != "" {
				item.Nickname = profile.Name
			}
			item.Phone = maskPhone(profile.Phone)
			if p.cfg.Pool.AuthByUID(item.UID) != nil {
				item.AlreadyImported = true
			}
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"platform":   runtime.GOOS,
		"locations":  locations,
		"candidates": out,
	})
}

// importScanned 导入指定路径的客户端凭证（路径由前端从扫描结果里带回来）。
//
// 不接受「导入全部扫描结果」这种隐式行为：用户勾哪条就导哪条，避免在多账号
// 机器上把别人的号也塞进来。
func (p *Panel) importScanned(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Paths []string `json:"paths"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(payload.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "没有选中任何文件")
		return
	}
	if len(payload.Paths) > 20 {
		writeErr(w, http.StatusBadRequest, "一次最多导入 20 个文件")
		return
	}
	type result struct {
		Path    string `json:"path"`
		OK      bool   `json:"ok"`
		UID     string `json:"uid,omitempty"`
		Name    string `json:"nickname,omitempty"`
		Error   string `json:"error,omitempty"`
		Updated bool   `json:"updated,omitempty"`
	}
	results := make([]result, 0, len(payload.Paths))
	imported := 0
	for _, raw := range payload.Paths {
		path := clientdir.ExpandPath(raw)
		res := result{Path: path}
		cands := clientdir.Scan([]string{path})
		if len(cands) == 0 {
			res.Error = "文件不存在或不是普通文件"
			results = append(results, res)
			continue
		}
		cand := cands[0]
		if cand.Err != "" {
			res.Error = cand.Err
			results = append(results, res)
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		account, err := p.cfg.Upstream.PrepareAccount(ctx, map[string]any{"access_token": cand.Token})
		cancel()
		if err != nil {
			res.Error = friendlyVerifyError(err)
			results = append(results, res)
			continue
		}
		res.Updated = p.cfg.Pool.AuthByUID(account.UID) != nil
		if err := p.admitQuiet(account); err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		res.OK, res.UID, res.Name = true, account.UID, account.Nickname
		imported++
		results = append(results, res)
	}
	status := http.StatusOK
	if imported == 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"imported": imported, "results": results})
}

// smsConfig 返回前端初始化阿里云验证码需要的参数。
func (p *Panel) smsConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, defaultCaptcha)
}

// smsSend 发送登录验证码。captcha_param 由前端控件回调拿到后原样带上来。
func (p *Panel) smsSend(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Phone         string `json:"phone"`
		NationCode    string `json:"nation_code"`
		CaptchaParam  string `json:"captcha_param"`
		InviterUserID string `json:"inviter_user_id"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// 要记邀请归因时必须走桌面端通道，发码也得跟着走同一条，
	// 否则验证码对不上（两条通道的码不通用）。
	preferLegacy := strings.TrimSpace(payload.InviterUserID) != ""
	res, err := p.cfg.Upstream.SendSmsCode(ctx, payload.NationCode, payload.Phone, payload.CaptchaParam, preferLegacy)
	if err != nil {
		writeErr(w, smsStatus(err), err.Error())
		return
	}
	p.rememberSms(payload.Phone, smsPending{
		verificationID: res.VerificationID, via: res.Via,
		inviter: strings.TrimSpace(payload.InviterUserID), at: time.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "sent": res.Sent, "captcha_ok": res.CaptchaOK,
		"verification_id": res.VerificationID, "via": res.Via,
	})
}

// smsLogin 用手机号 + 验证码登录并入池。
func (p *Panel) smsLogin(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Phone          string `json:"phone"`
		NationCode     string `json:"nation_code"`
		Code           string `json:"code"`
		VerificationID string `json:"verification_id"`
		// InviterUserID 选填：注册时记到某个账号名下（上游的归因字段，
		// 实测**没有积分奖励**，纯归属记录）。留空 = 自然流量。
		InviterUserID string `json:"inviter_user_id"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 发码时记下的通道信息优先：它才知道这条码是哪条通道发的。
	pending := p.lookupSms(payload.Phone)
	verificationID := payload.VerificationID
	if verificationID == "" {
		verificationID = pending.verificationID
	}
	inviter := strings.TrimSpace(payload.InviterUserID)
	if inviter == "" {
		inviter = pending.inviter
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	account, err := p.cfg.Upstream.LoginWithSms(ctx, payload.NationCode, payload.Phone, payload.Code, verificationID, inviter)
	if err != nil {
		msg := err.Error()
		// 验证码 5 分钟有效：超时之后填对了也只会得到「不正确」，
		// 不解释一句用户会以为是自己输错了。
		if !pending.at.IsZero() && time.Since(pending.at) > 5*time.Minute && !pending.viaMissing() {
			msg += "（这条验证码是 " + pending.at.Format("15:04") + " 发出的，短信码 5 分钟内有效，请重新发送）"
		}
		writeErr(w, smsStatus(err), msg)
		return
	}
	p.forgetSms(payload.Phone)
	updated := p.cfg.Pool.AuthByUID(account.UID) != nil
	if err := p.admitQuiet(account); err != nil {
		writeErr(w, http.StatusInternalServerError, "入池失败: "+err.Error())
		return
	}
	if p.cfg.Logs != nil {
		p.cfg.Logs.Add("短信登录成功：" + account.Nickname + "（" + account.UID + "）")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "updated": updated,
		"account": map[string]any{
			"uid": account.UID, "nickname": account.Nickname,
			"expires_at":  account.ExpiresAtValue(),
			"has_refresh": account.CanRefresh(),
		},
	})
}

// smsStatus 把上游哨兵错误映射成 HTTP 状态码：
// 参数/验证码类都是 400（用户改一下再试），其余按 502（上游问题）。
func smsStatus(err error) int {
	switch {
	case errors.Is(err, upstream.ErrCaptchaRejected),
		errors.Is(err, upstream.ErrSmsCodeRejected),
		errors.Is(err, upstream.ErrPhoneRejected):
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

// friendlyVerifyError 把「票不能用」翻成用户能行动的话。
func friendlyVerifyError(err error) string {
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "401"), strings.Contains(msg, "登录态"), strings.Contains(msg, "unauthorized"):
		return "票已失效（IAM 票 30 天，过期需要重新登录客户端）：" + msg
	case strings.Contains(lower, "timeout"), strings.Contains(msg, "超时"):
		return "校验超时，上游没响应：" + msg
	}
	return msg
}

// maskPhone 只留后 4 位。
func maskPhone(phone string) string {
	phone = strings.TrimSpace(phone)
	if len(phone) <= 4 {
		return phone
	}
	return strings.Repeat("*", len(phone)-4) + phone[len(phone)-4:]
}

// normalizePhoneKey 把手机号归一成 map key（去掉空格与连字符）。
func normalizePhoneKey(phone string) string {
	return strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(strings.TrimSpace(phone))
}

// rememberSms 记录本次发码的通道；顺手清掉超过 10 分钟的旧记录。
func (p *Panel) rememberSms(phone string, pending smsPending) {
	key := normalizePhoneKey(phone)
	if key == "" {
		return
	}
	p.smsMu.Lock()
	defer p.smsMu.Unlock()
	if p.smsPending == nil {
		p.smsPending = map[string]smsPending{}
	}
	for k, v := range p.smsPending {
		if time.Since(v.at) > 10*time.Minute {
			delete(p.smsPending, k)
		}
	}
	p.smsPending[key] = pending
}

// lookupSms 取回发码记录（不删除：用户可能连续输错几次）。
func (p *Panel) lookupSms(phone string) smsPending {
	key := normalizePhoneKey(phone)
	p.smsMu.Lock()
	defer p.smsMu.Unlock()
	return p.smsPending[key]
}

// forgetSms 登录成功后清掉记录。
func (p *Panel) forgetSms(phone string) {
	key := normalizePhoneKey(phone)
	p.smsMu.Lock()
	defer p.smsMu.Unlock()
	delete(p.smsPending, key)
}

// viaMissing 判断这条记录是否存在（零值表示没有发码记录）。
func (s smsPending) viaMissing() bool { return s.at.IsZero() }
